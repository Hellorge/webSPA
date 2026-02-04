package main

import (
    "fmt"
    "log"
    "os"
    "path/filepath"
    "sync"
    "time"

    "github.com/fsnotify/fsnotify"
)

type Watcher struct {
    ctx         *BuildContext
    watcher     *fsnotify.Watcher
    events      chan string
    debounceMap sync.Map
}

func (ctx *BuildContext) watchFiles() error {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        return fmt.Errorf("failed to create watcher: %w", err)
    }
    defer watcher.Close()

    w := &Watcher{
        ctx:     ctx,
        watcher: watcher,
        events:  make(chan string, 100),
    }

    // Add directories to watch
    for _, dir := range toBuildDir {
        fullDir := filepath.Join(ctx.config.Directories.Web, dir)
        if err := filepath.Walk(fullDir, func(path string, info os.FileInfo, err error) error {
            if err != nil {
                return err
            }
            if info.IsDir() {
                return watcher.Add(path)
            }
            return nil
        }); err != nil {
            return fmt.Errorf("failed to add watch paths: %w", err)
        }
    }

    log.Println("Watching for file changes...")

    // Process events
    go w.handleEvents()

    // Watch for events
    for {
        select {
        case event, ok := <-watcher.Events:
            if !ok {
                return nil
            }
            if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove) != 0 {
                w.debounceEvent(event.Name)
            }

        case err, ok := <-watcher.Errors:
            if !ok {
                return nil
            }
            log.Printf("Watcher error: %v", err)
        }
    }
}

func (w *Watcher) debounceEvent(path string) {
    const debounceTime = 100 * time.Millisecond

    if _, ok := w.debounceMap.Load(path); !ok {
        // First event for this path
        w.debounceMap.Store(path, struct{}{})
        time.AfterFunc(debounceTime, func() {
            w.debounceMap.Delete(path)
            w.events <- path
        })
    }
}

func (w *Watcher) handleEvents() {
    for path := range w.events {
        if err := w.processChange(path); err != nil {
            log.Printf("Error processing change for %s: %v", path, err)
        }
    }
}

func (w *Watcher) processChange(path string) error {
    // Get file info
    info, err := os.Stat(path)
    if err != nil {
        if os.IsNotExist(err) {
            // File was deleted, handle cleanup
            return w.handleDelete(path)
        }
        return err
    }

    // Skip if it's a directory
    if info.IsDir() {
        return nil
    }

    // Get relative path
    relPath, err := filepath.Rel(w.ctx.config.Directories.Web, path)
    if err != nil {
        return err
    }

    // Check if we should process this file
    if shouldIgnore(filepath.Dir(path), path) {
        return nil
    }

    log.Printf("Processing changed file: %s", relPath)

    // Process the file
    w.ctx.workerpool.Submit(WorkItem{
        Path:    path,
        RelPath: relPath,
        Info:    info,
    })

    // Find and process dependents
    if deps := w.ctx.depGraph.GetDependents(relPath); len(deps) > 0 {
        log.Printf("Processing %d dependents of %s", len(deps), relPath)
        for _, dep := range deps {
            depPath := filepath.Join(w.ctx.config.Directories.Web, dep)
            depInfo, err := os.Stat(depPath)
            if err != nil {
                continue
            }
            w.ctx.workerpool.Submit(WorkItem{
                Path:    depPath,
                RelPath: dep,
                Info:    depInfo,
            })
        }
    }

    return nil
}

func (w *Watcher) handleDelete(path string) error {
    // Get relative path
    relPath, err := filepath.Rel(w.ctx.config.Directories.Web, path)
    if err != nil {
        return err
    }

    // Clean up caches
    if entry, ok := w.ctx.cache.Get(relPath); ok {
        os.Remove(entry.FileInfo.DistPath)
    }

    // Process dependents
    deps := w.ctx.depGraph.GetDependents(relPath)
    for _, dep := range deps {
        depPath := filepath.Join(w.ctx.config.Directories.Web, dep)
        if depInfo, err := os.Stat(depPath); err == nil {
            w.ctx.workerpool.Submit(WorkItem{
                Path:    depPath,
                RelPath: dep,
                Info:    depInfo,
            })
        }
    }

    return nil
}
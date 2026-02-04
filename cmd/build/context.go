package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"gogogo/modules/config"
	"gogogo/modules/filemanager"
	"gogogo/modules/router"
	"gogogo/modules/templates"
	"gogogo/modules/fileaccess"

	"github.com/BurntSushi/toml"
)

type BuildContext struct {
	config      *config.Config
	concurrency int
	cache       *Cache
	depGraph    *DependencyGraph
	bufferPool  *BufferPool
	workerpool  *WorkerPool
	minifier       *MinificationWorker
	templateEngine *templates.TemplateEngine
	errors         *ErrorCollector
	aliasMap    map[string]string
	usedAliases map[string]string
	force       bool
	target      string
	dryRun      bool
	stats       bool
	outputDir   string
	buildStats  *BuildStats
}

type BuildStats struct {
	StartTime      time.Time
	EndTime        time.Time
	TotalFiles     int32
	ProcessedFiles int32
	SkippedFiles   int32
	AliasedPaths   int
	TotalSize      int64
	MinifiedSize   int64
}

type MetaData struct {
	Alias string `toml:"alias"`
}

func (ctx *BuildContext) initialize() error {
	// Initialize components

	ctx.buildStats = &BuildStats{
		StartTime: time.Now(),
	}
	ctx.aliasMap = make(map[string]string)
	ctx.usedAliases = make(map[string]string)

	ctx.workerpool = NewWorkerPool(ctx.concurrency, ctx)
	ctx.minifier = NewMinificationWorker()
	ctx.cache = NewCache()
	ctx.depGraph = NewDependencyGraph()
	ctx.bufferPool = NewBufferPool(defaultBufferSize)
	ctx.errors = NewErrorCollector()

	// Initialize file manager for templates to use
	fa := fileaccess.New()
	fm := filemanager.New(fa, nil, nil, filemanager.Config{
		RootDir: ctx.config.Directories.Web,
	})
	ctx.templateEngine = templates.New(fm, ctx.config.Directories.Templates, ctx.config.Templates.Main, true) // Always production mode for builder

	if ctx.dryRun {
		log.Println("DRY RUN - no files will be written")
	}

	if ctx.target != "" {
		targetPath := filepath.Join(ctx.config.Directories.Web, ctx.target)
		if _, err := os.Stat(targetPath); err != nil {
			return fmt.Errorf("target directory not found: %s", targetPath)
		}
		// Override toBuildDir with just the target
		toBuildDir = []string{targetPath}
		log.Printf("Building target directory: %s", ctx.target)
	} else {
		entries, err := os.ReadDir(ctx.config.Directories.Web)
		if err != nil {
			log.Fatalf("failed to load read web directory: %w", err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				toBuildDir = append(toBuildDir, filepath.Join(ctx.config.Directories.Web, entry.Name()))
			}
		}
	}

	if ctx.outputDir == "" {
		ctx.outputDir = ctx.config.Directories.Dist
	}

	if ctx.concurrency <= 0 {
		ctx.concurrency = runtime.NumCPU()
	} else if ctx.concurrency > maxWorkers {
		ctx.concurrency = maxWorkers
		log.Printf("Concurency set to %d", ctx.concurrency)
	}

	// Load cache
	buildCachePath = filepath.Join(ctx.config.Directories.Meta, "build_cache.json")
	if err := ctx.cache.Load(buildCachePath); err != nil {
		return fmt.Errorf("failed to load cache: %w", err)
	}

	return nil
}

func (ctx *BuildContext) build() error {
	log.Println("Starting build process...")

	// Count total files first
	for _, dir := range toBuildDir {
		filepath.Walk(dir,
			func(_ string, info os.FileInfo, _ error) error {
				if info != nil && !info.IsDir() {
					atomic.AddInt32(&ctx.buildStats.TotalFiles, 1)
				}
				return nil
			})
	}

	// Process directories
	for _, dir := range toBuildDir {
		if err := ctx.processDirectory(dir); err != nil {
			return err
		}
	}

	if !ctx.dryRun {
		// Wait for completion
		if err := ctx.workerpool.Wait(); err != nil {
			return err
		}

		// Build router binary
		if err := ctx.buildRouterBinary(); err != nil {
			return err
		}

		// Save caches
		if err := ctx.saveAllCaches(); err != nil {
			return err
		}
	}

	if ctx.errors.HasErrors() {
		return ctx.errors.Error()
	}

	if ctx.stats {
		ctx.buildStats.EndTime = time.Now()
		ctx.buildStats.AliasedPaths = len(ctx.aliasMap)
		ctx.printBuildStats()
	}

	return nil
}

func (ctx *BuildContext) saveAllCaches() error {
	return ctx.cache.Save(buildCachePath)
}

func (ctx *BuildContext) getAliasedPath(path string) string {
	if path == "." || path == "" {
		return path
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Use the alias if it exists, otherwise use the original name
	if alias, exists := ctx.aliasMap[path]; exists {
		base = alias
	}

	parentPath := ctx.getAliasedPath(dir)
	if parentPath == "." {
		return base
	}

	return filepath.Join(parentPath, base)
}

func (ctx *BuildContext) processAlias(path string) error {
	metaPath := filepath.Join(path, "meta.toml")
	relPath, err := filepath.Rel(ctx.config.Directories.Web, path)
	if err != nil {
		return fmt.Errorf("error calculating relative path for %s: %w", path, err)
	}

	if _, err := os.Stat(metaPath); err == nil {
		data, err := os.ReadFile(metaPath)
		if err != nil {
			return fmt.Errorf("error reading meta.toml at %s: %w", path, err)
		}

		meta := &MetaData{}
		if err := toml.Unmarshal(data, meta); err != nil {
			return fmt.Errorf("error parsing meta.toml at %s: %w", path, err)
		}

		if meta.Alias != "" {
			if strings.ContainsAny(meta.Alias, "<>:\"\\|?*") {
				return fmt.Errorf("invalid characters in alias for path %q: %q", relPath, meta.Alias)
			}

			if len(meta.Alias) > 100 {
				return fmt.Errorf("alias too long for path %q: %q (max 100 characters)", relPath, meta.Alias)
			}

			if existing, exists := ctx.usedAliases[meta.Alias]; exists {
				return fmt.Errorf("duplicate alias detected:\n"+
					"  Alias: %s\n"+
					"  Path: %s\n"+
					"  Conflicts with: %s\n",
					meta.Alias, relPath, existing)
			}

			ctx.aliasMap[relPath] = meta.Alias
			ctx.usedAliases[meta.Alias] = relPath
		}
	}
	return nil
}

func (ctx *BuildContext) processDirectory(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			ctx.errors.Add(fmt.Errorf("error accessing %s: %w", path, err))
			return nil
		}

		if info.IsDir() || shouldIgnore(dir, path) {
			if info.IsDir() {
				if err := ctx.processAlias(path); err != nil {
					ctx.errors.Add(err)
				}
			}
			return nil
		}

		relPath, err := filepath.Rel(ctx.config.Directories.Web, path)
		if err != nil {
			ctx.errors.Add(fmt.Errorf("error calculating relative path for %s: %w", path, err))
			return nil
		}

		if ctx.shouldProcess(relPath, info) {
			aliasedPath := ctx.getAliasedPath(relPath)

			if ctx.dryRun {
				log.Printf("Would build: %s -> %s", relPath, aliasedPath)
				atomic.AddInt32(&ctx.buildStats.ProcessedFiles, 1)
				return nil
			}

			// Track file sizes for stats if needed
			if ctx.stats {
				atomic.AddInt64(&ctx.buildStats.TotalSize, info.Size())
			}

			ctx.workerpool.Submit(WorkItem{
				Path:        path,
				RelPath:     relPath,
				AliasedPath: aliasedPath,
				Info:        info,
			})
			atomic.AddInt32(&ctx.buildStats.ProcessedFiles, 1)

			if ctx.buildStats.ProcessedFiles%10 == 0 {
				log.Printf("Progress: %d/%d files processed",
					ctx.buildStats.ProcessedFiles, ctx.buildStats.TotalFiles)
			}
		} else {
			atomic.AddInt32(&ctx.buildStats.SkippedFiles, 1)
		}

		return nil
	})
}

func (ctx *BuildContext) shouldProcess(relPath string, info os.FileInfo) bool {
	if ctx.force {
		return true
	}

	entry, exists := ctx.cache.Get(relPath)
	if !exists {
		return true
	}

	if info.ModTime().After(entry.FileInfo.ModTime) {
		return true
	}

	dependents := ctx.depGraph.GetDependents(relPath)
	return len(dependents) > 0
}

func (ctx *BuildContext) buildRouterBinary() error {
	root := &router.RadixNode{
		Children: make([]*router.RadixNode, 0, 16),
	}

	cacheEntries := ctx.cache.GetAll()
	for path, entry := range cacheEntries {
		segments := strings.Split(strings.Trim(path, "/"), "/")
		root.Insert(segments, &entry.FileInfo)
	}

	// Create meta directory if needed
	if err := os.MkdirAll(ctx.config.Directories.Meta, 0755); err != nil {
		return err
	}

	buf := ctx.bufferPool.Get()
	defer ctx.bufferPool.Put(buf)

	buffer := bytes.NewBuffer(buf)
	buffer.Grow(1 << 20)

	if err := gob.NewEncoder(buffer).Encode(root); err != nil {
		return err
	}

	return atomicWrite(
		filepath.Join(ctx.config.Directories.Meta, "router_binary.bin"),
		buffer.Bytes(),
	)
}

func (ctx *BuildContext) printBuildStats() {
	duration := ctx.buildStats.EndTime.Sub(ctx.buildStats.StartTime)
	filesPerSec := float64(ctx.buildStats.ProcessedFiles) / duration.Seconds()

	fmt.Printf("\nBuild Statistics:\n")
	fmt.Printf("================\n")
	fmt.Printf("Duration: %v\n", duration)
	fmt.Printf("Total Files: %d\n", ctx.buildStats.TotalFiles)
	fmt.Printf("Processed: %d\n", ctx.buildStats.ProcessedFiles)
	fmt.Printf("Skipped: %d\n", ctx.buildStats.SkippedFiles)
	fmt.Printf("Aliased Paths: %d\n", ctx.buildStats.AliasedPaths)
	fmt.Printf("Files/Second: %.2f\n", filesPerSec)

	if ctx.buildStats.MinifiedSize > 0 {
		reduction := (1 - float64(ctx.buildStats.MinifiedSize)/float64(ctx.buildStats.TotalSize)) * 100
		fmt.Printf("Size Reduction: %.2f%%\n", reduction)
	}

	if ctx.target != "" {
		fmt.Printf("Target Directory: %s\n", ctx.target)
	}
	if ctx.outputDir != "" {
		fmt.Printf("Output Directory: %s\n", ctx.outputDir)
	}
}

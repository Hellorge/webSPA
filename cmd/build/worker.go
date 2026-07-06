package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

type WorkerPool struct {
	workers    []*Worker
	workChan   chan WorkItem
	ctx        *BuildContext
	wg         sync.WaitGroup
	activeJobs int32
}

type Worker struct {
	id  int
	ctx *BuildContext
}

type WorkItem struct {
	Path        string
	RelPath     string
	Info        os.FileInfo
	AliasedPath string
}

func NewWorkerPool(concurrency int, ctx *BuildContext) *WorkerPool {
	wp := &WorkerPool{
		workers:  make([]*Worker, concurrency),
		workChan: make(chan WorkItem, 1000),
		ctx:      ctx,
	}

	for i := 0; i < concurrency; i++ {
		wp.workers[i] = &Worker{id: i, ctx: ctx}
		go wp.workers[i].start(wp)
	}

	return wp
}

func (wp *WorkerPool) Submit(item WorkItem) {
	atomic.AddInt32(&wp.activeJobs, 1)
	wp.wg.Add(1)
	wp.workChan <- item
}

func (wp *WorkerPool) Wait() error {
	for atomic.LoadInt32(&wp.activeJobs) > 0 {
		wp.wg.Wait()
	}
	close(wp.workChan)

	return wp.ctx.errors.Error()
}

func (w *Worker) start(pool *WorkerPool) {
	for item := range pool.workChan {
		if err := w.process(item); err != nil {
			w.ctx.errors.Add(fmt.Errorf("worker %d: %v", w.id, err))
		}
		atomic.AddInt32(&pool.activeJobs, -1)
		pool.wg.Done()
	}
}

func (w *Worker) process(item WorkItem) error {
	result, err := w.processFile(item)
	if err != nil {
		return fmt.Errorf("error processing %s: %w", item.Path, err)
	}
	if result.Skip {
		// File is metadata (e.g. meta.toml); not registered as a route.
		return nil
	}

	if w.ctx.stats && !w.ctx.dryRun {
		atomic.AddInt64(&w.ctx.buildStats.MinifiedSize, int64(len(result.FileInfo.EmbeddedData)))
	}

	if !w.ctx.dryRun {
		w.ctx.cache.Set(item.RelPath, CacheEntry{
			FileInfo: result.FileInfo,
			RelPath:  item.RelPath,
			Holes:    result.Holes,
			FilePath: result.FilePath,
		})
	}

	return nil
}

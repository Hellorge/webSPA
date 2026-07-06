package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"gogogo/modules/build"
	"gogogo/modules/config"
)

// BuildContext is per-site. cmd/build/main.go constructs one BuildContext
// per [[site]] entry in config and runs each in turn. Per-site state means
// per-site renderer (different templates root), per-site cache (so an edit
// in web/docs doesn't invalidate web/main), and per-site manifest output.
type BuildContext struct {
	config       *config.Config
	site         config.Site
	siteRoot     string // web/<site.Name>, absolute
	distRoot     string // dist/<site.Name> (or ctx.outputDir/<site.Name> if overridden)
	cachePath    string // meta/<site.Name>_cache.json
	manifestPath string // meta/<site.Name>_router.bin

	concurrency int
	cache       *Cache
	depGraph    *DependencyGraph
	workerpool  *WorkerPool
	minifier    *MinificationWorker
	renderer    *build.PageRenderer
	errors      *ErrorCollector
	aliasMap    map[string]string
	usedAliases map[string]string
	force       bool
	target      string
	dryRun      bool
	stats       bool
	outputDir   string
	buildStats  *BuildStats
	toBuildDir  []string
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


func (ctx *BuildContext) initialize() error {
	ctx.buildStats = &BuildStats{StartTime: time.Now()}
	ctx.aliasMap = make(map[string]string)
	ctx.usedAliases = make(map[string]string)

	siteRoot, err := filepath.Abs(filepath.Join(ctx.config.Directories.Web, ctx.site.Name))
	if err != nil {
		return fmt.Errorf("resolve siteRoot for %q: %w", ctx.site.Name, err)
	}
	ctx.siteRoot = siteRoot

	distBase := ctx.outputDir
	if distBase == "" {
		distBase = ctx.config.Directories.Dist
	}
	ctx.distRoot = filepath.Join(distBase, ctx.site.Name)
	ctx.cachePath = filepath.Join(ctx.config.Directories.Meta, ctx.site.Name+"_cache.json")
	ctx.manifestPath = filepath.Join(ctx.config.Directories.Meta, ctx.site.Name+"_router.bin")

	templatesDir := filepath.Join(ctx.siteRoot, ctx.config.Directories.Templates)

	ctx.workerpool = NewWorkerPool(ctx.concurrency, ctx)
	ctx.cache = NewCache()
	ctx.depGraph = NewDependencyGraph()
	ctx.errors = NewErrorCollector()
	ctx.minifier = NewMinificationWorker()
	ctx.renderer = build.NewPageRenderer(templatesDir)

	if ctx.dryRun {
		log.Println("DRY RUN - no files will be written")
	}

	contentDir := filepath.Join(ctx.siteRoot, ctx.config.Directories.Content)
	staticDir := filepath.Join(ctx.siteRoot, ctx.config.Directories.Static)

	if ctx.target != "" {
		targetPath := filepath.Join(contentDir, ctx.target)
		if _, err := os.Stat(targetPath); err != nil {
			return fmt.Errorf("target directory not found: %s", targetPath)
		}
		ctx.toBuildDir = []string{targetPath}
		log.Printf("[%s] Building target directory: %s", ctx.site.Name, ctx.target)
	} else {
		// Content tree (each top-level dir under content/ is a "page family").
		if entries, err := os.ReadDir(contentDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					ctx.toBuildDir = append(ctx.toBuildDir, filepath.Join(contentDir, entry.Name()))
				}
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("[%s] read content dir: %w", ctx.site.Name, err)
		}

		// Static assets.
		if _, err := os.Stat(staticDir); err == nil {
			ctx.toBuildDir = append(ctx.toBuildDir, staticDir)
		}
	}

	if ctx.concurrency <= 0 {
		ctx.concurrency = runtime.NumCPU()
	} else if ctx.concurrency > maxWorkers {
		ctx.concurrency = maxWorkers
		log.Printf("Concurency set to %d", ctx.concurrency)
	}

	if err := ctx.cache.Load(ctx.cachePath); err != nil {
		return fmt.Errorf("[%s] load cache: %w", ctx.site.Name, err)
	}

	return nil
}

func (ctx *BuildContext) build() error {
	log.Printf("[%s] Starting build...", ctx.site.Name)

	for _, dir := range ctx.toBuildDir {
		filepath.Walk(dir,
			func(_ string, info os.FileInfo, _ error) error {
				if info != nil && !info.IsDir() {
					atomic.AddInt32(&ctx.buildStats.TotalFiles, 1)
				}
				return nil
			})
	}

	for _, dir := range ctx.toBuildDir {
		if err := ctx.processDirectory(dir); err != nil {
			return err
		}
	}

	if !ctx.dryRun {
		if err := ctx.workerpool.Wait(); err != nil {
			return err
		}

		if err := ctx.buildRouterBinary(); err != nil {
			return err
		}

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
	return ctx.cache.Save(ctx.cachePath)
}

// urlForRelPath computes the public URL for a build-tree relPath
// (relative to siteRoot). Rules, applied in order:
//
//   1. Files outside content/ (e.g. static/app.js) keep their relPath
//      as the URL — no alias logic applies.
//   2. Strip the content/ prefix (build-tree convention, not URL).
//   3. Strip the canonical content.html filename.
//   4. For each remaining segment, look up its aliasMap entry. An alias
//      of "/" drops that segment (root mapping — used by site landing
//      pages). An alias of "X" replaces the segment with X. No alias =
//      keep.
func (ctx *BuildContext) urlForRelPath(relPath string) string {
	s := filepath.ToSlash(relPath)
	if !strings.HasPrefix(s, "content/") {
		return strings.TrimPrefix(s, "/")
	}
	s = strings.TrimPrefix(s, "content/")
	s = strings.TrimSuffix(s, "/content.html")
	s = strings.TrimSuffix(s, "content.html")
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}

	segments := strings.Split(s, "/")
	var out []string
	accum := "content"
	for _, seg := range segments {
		accum = accum + "/" + seg
		if alias, ok := ctx.aliasMap[accum]; ok {
			if alias == "/" {
				continue
			}
			out = append(out, alias)
		} else {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// processAlias discovers a directory's URL alias. The alias controls how
// urlForRelPath transforms the directory's segment when building the
// final URL.
//
// Discovery is two-step:
//   1. If the dir contains a content.html, byte-scan its first 1 KiB for
//      a {% alias "X" %} tag (build.ScanAlias). If found, that's the
//      alias.
//   2. Otherwise no alias is registered — urlForRelPath falls back to
//      the directory name verbatim.
//
// No more meta.toml. Page metadata lives inside content.html.
func (ctx *BuildContext) processAlias(path string) error {
	relPath, err := filepath.Rel(ctx.siteRoot, path)
	if err != nil {
		return fmt.Errorf("error calculating relative path for %s: %w", path, err)
	}

	contentPath := filepath.Join(path, "content.html")
	src, err := os.ReadFile(contentPath)
	if err != nil {
		// No content.html in this directory — nothing to scan. Walking
		// continues into subdirs; their content.html will be handled then.
		return nil
	}

	alias := build.ScanAlias(src)
	if alias == "" {
		return nil
	}

	// `:` and `*` are trie pattern markers (`/users/:id`, `/static/*path`)
	// — legitimate in an alias, not filesystem-unsafe. Keep blocking
	// characters that would break URL parsing or filesystem operations.
	if strings.ContainsAny(alias, "<>\"\\|?") {
		return fmt.Errorf("invalid characters in alias for path %q: %q", relPath, alias)
	}
	if len(alias) > 100 {
		return fmt.Errorf("alias too long for path %q: %q (max 100 characters)", relPath, alias)
	}
	if existing, exists := ctx.usedAliases[alias]; exists {
		return fmt.Errorf("duplicate alias detected:\n"+
			"  Alias: %s\n"+
			"  Path: %s\n"+
			"  Conflicts with: %s\n",
			alias, relPath, existing)
	}

	ctx.aliasMap[relPath] = alias
	ctx.usedAliases[alias] = relPath
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

		relPath, err := filepath.Rel(ctx.siteRoot, path)
		if err != nil {
			ctx.errors.Add(fmt.Errorf("error calculating relative path for %s: %w", path, err))
			return nil
		}

		if ctx.shouldProcess(relPath, info) {
			aliasedPath := ctx.urlForRelPath(relPath)

			if ctx.dryRun {
				log.Printf("[%s] Would build: %s -> %s", ctx.site.Name, relPath, aliasedPath)
				atomic.AddInt32(&ctx.buildStats.ProcessedFiles, 1)
				return nil
			}

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
				log.Printf("[%s] Progress: %d/%d", ctx.site.Name,
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
	results := make(map[string]ProcessResult)
	allEntries := ctx.cache.GetAll()

	for path, entry := range allEntries {
		results[path] = ProcessResult{
			FileInfo: entry.FileInfo,
			Holes:    entry.Holes,
			FilePath: entry.FilePath,
		}
	}

	compiler := NewV2Compiler()
	compiler.SiteName = ctx.site.Name
	return compiler.Compile(results, ctx.manifestPath)
}

func (ctx *BuildContext) printBuildStats() {
	duration := ctx.buildStats.EndTime.Sub(ctx.buildStats.StartTime)
	filesPerSec := float64(ctx.buildStats.ProcessedFiles) / duration.Seconds()

	fmt.Printf("\nBuild Statistics [%s]:\n", ctx.site.Name)
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
	fmt.Printf("Output: %s\n", ctx.manifestPath)
}

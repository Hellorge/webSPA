package main

import (
	"flag"
	"fmt"
	"gogogo/modules/config"
	"gogogo/modules/db"
	"log"
	"path/filepath"
	"runtime"
	"time"

)

const (
	defaultBufferSize = 32 * 1024
	defaultMapSize    = 1000
	maxWorkers        = 8192
)

var (
	buildStartTime = time.Now()
	processedFiles int32
	totalFiles     int32
)

func main() {
	treeFlag := flag.Bool("tree", false, "Display directory structure with aliases")
	watch := flag.Bool("watch", false, "Watch for file changes")
	concurrency := flag.Int("concurrency", runtime.NumCPU(), "Number of concurrent workers")
	verbose := flag.Bool("v", false, "Verbose output")
	forceMode := flag.Bool("force", false, "Force build all files")
	target := flag.String("target", "", "Build specific directory (relative to content dir)")
	dryRun := flag.Bool("dry-run", false, "Show what would be built without actually building")
	stats := flag.Bool("stats", false, "Show detailed build statistics")
	out := flag.String("out", "", "Custom output directory (default: dist)")
	siteFilter := flag.String("site", "", "Build only the named site (default: all sites in config)")
	embedThreshold := flag.Int64("embed-threshold", 1<<20, "Files at or above this byte size go to dist/ as ActionFile (default 1 MiB)")
	flag.Parse()

	embedThresholdBytes = *embedThreshold

	log.SetFlags(0)
	if *verbose {
		log.SetFlags(log.Ltime | log.Lmicroseconds)
	}

	cfg, err := config.LoadConfig("web/config.toml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.Sites) == 0 {
		log.Fatalf("No [[site]] entries in web/config.toml — multi-site build needs at least one")
	}

	dbPath := filepath.Join(cfg.Directories.Meta, "gogogo.db")
	if err := db.Init(dbPath); err != nil {
		log.Fatalf("Failed to init DB for build: %v", err)
	}

	sitesToBuild := cfg.Sites
	if *siteFilter != "" {
		sitesToBuild = nil
		for _, s := range cfg.Sites {
			if s.Name == *siteFilter {
				sitesToBuild = append(sitesToBuild, s)
				break
			}
		}
		if len(sitesToBuild) == 0 {
			log.Fatalf("Site %q not found in config", *siteFilter)
		}
	}

	// Watch mode is per-site for now: pick the named site or refuse if
	// multiple sites are present without -site. Building all sites in one
	// watcher would need cross-site dispatch on file events; not worth it
	// until we have a use case.
	if *watch {
		if len(sitesToBuild) != 1 {
			log.Fatalf("-watch needs a single site; pass -site=<name>")
		}
		ctx := newContext(&cfg, sitesToBuild[0], *concurrency, *forceMode, *target, *dryRun, *stats, *out)
		if err := ctx.initialize(); err != nil {
			log.Fatalf("[%s] init: %v", sitesToBuild[0].Name, err)
		}
		log.Printf("[%s] Starting watch mode...", sitesToBuild[0].Name)
		if err := ctx.watchFiles(); err != nil {
			log.Fatalf("Watch failed: %v", err)
		}
		return
	}

	for _, site := range sitesToBuild {
		ctx := newContext(&cfg, site, *concurrency, *forceMode, *target, *dryRun, *stats, *out)
		if err := ctx.initialize(); err != nil {
			log.Fatalf("[%s] init: %v", site.Name, err)
		}
		if err := ctx.build(); err != nil {
			log.Fatalf("[%s] build: %v", site.Name, err)
		}

		if *treeFlag {
			treeCmd := NewTreeCommand()
			if err := treeCmd.Execute(ctx.siteRoot); err != nil {
				log.Fatalf("[%s] tree: %v", site.Name, err)
			}
		}
	}

	// Studio is a module-managed site — its content lives under
	// modules/studio/content and its routes/handlers come from the studio
	// package. Built separately because it doesn't follow the
	// content/static/templates layout.
	studioManifestPath := filepath.Join(cfg.Directories.Meta, "studio_router.bin")
	if err := buildStudioManifest(studioManifestPath); err != nil {
		log.Fatalf("Studio build failed: %v", err)
	}

	fmt.Printf("All builds completed in %v\n", time.Since(buildStartTime))
}

func newContext(cfg *config.Config, site config.Site, concurrency int, force bool, target string, dryRun, stats bool, outDir string) *BuildContext {
	return &BuildContext{
		config:      cfg,
		site:        site,
		concurrency: concurrency,
		force:       force,
		target:      target,
		dryRun:      dryRun,
		stats:       stats,
		outputDir:   outDir,
	}
}

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"gogogo/modules/cache"
	"gogogo/modules/coalescer"
	"gogogo/modules/config"
	"gogogo/modules/fileaccess"
	"gogogo/modules/filemanager"
	"gogogo/modules/handlers"
	"gogogo/modules/router"
	"gogogo/modules/server"
	"gogogo/modules/templates"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	cfg, err := config.LoadConfig("web/config.toml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize base layers
	fa := fileaccess.New()

	var cacheInstance *cache.Cache
	if cfg.Server.CachingEnabled {
		cacheInstance = cache.NewCache(cfg.Cache.MaxSize)
	}

	var coalescerInstance *coalescer.Coalescer
	if cfg.Server.CoalescerEnabled {
		coalescerInstance = coalescer.NewCoalescer()
	}

	// Load router in production mode
	var r *router.Router
	if cfg.Server.ProductionMode {
		r, err = router.LoadFromBinary(filepath.Join(cfg.Directories.Meta, "router_binary.bin"))
		if err != nil {
			log.Fatalf("Failed to load router: %v", err)
		}
	}

	// Initialize file manager
	fm := filemanager.New(fa, cacheInstance, coalescerInstance, filemanager.Config{
		RootDir: cfg.Directories.Web,
		Router:  r,
	})

	templateEngine := templates.New(fm, cfg.Directories.Templates, cfg.Templates.Main, cfg.Server.ProductionMode)

	// Initialize all handlers
	webHandler := handlers.NewWebHandler(fm, templateEngine, cfg.Directories.Content, cfg.Server.SPAMode)
	staticHandler := handlers.NewStaticHandler(fm)
	apiHandler := handlers.NewAPIHandler(fm, cfg.Directories.Content)

	// SPA handler only if SPA mode enabled
	var spaHandler http.Handler
	if cfg.Server.SPAMode {
		spaHandler = handlers.NewSPAHandler(fm, cfg.Directories.Content, cfg.Server.SPAMode)
	}

	srv := server.New(server.Handlers{
		Web:    webHandler,
		SPA:    spaHandler,
		Static: staticHandler,
		API:    apiHandler,
	}, cfg)

	// Handle shutdown
	done := make(chan bool, 1)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Server is shutting down...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v\n", err)
		}

		close(done)
	}()

	log.Printf("Server starting on %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	if err := srv.Start(); err != nil {
		log.Printf("Server error: %v\n", err)
	}

	<-done
}

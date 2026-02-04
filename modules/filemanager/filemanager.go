package filemanager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gogogo/modules/cache"
	"gogogo/modules/coalescer"
	"gogogo/modules/fileaccess"
	"gogogo/modules/router"
	"gogogo/modules/metrics"
)

var (
	ErrNotFound = errors.New("file not found")
)

type FileManager struct {
	fileAccess *fileaccess.FileAccess
	cache      *cache.Cache
	coalescer  *coalescer.Coalescer
	router     *router.Router
	rootDir    string
	GetContent        func(path string) ([]byte, error)
	GetContentEncoded func(path string, encoding string) ([]byte, string, error)
	OpenFile          func(path string) (*os.File, error)
	Exists            func(path string) bool
}

type Config struct {
	RootDir string
	Router  *router.Router // Will be nil in dev mode
}

func New(fa *fileaccess.FileAccess, ca *cache.Cache, co *coalescer.Coalescer, cfg Config) *FileManager {
	fm := &FileManager{
		fileAccess: fa,
		cache:      ca,
		coalescer:  co,
		router:     cfg.Router,
		rootDir:    cfg.RootDir,
	}

	// Set the appropriate GetContent function based on whether Router exists
	if cfg.Router != nil {
		fm.GetContent = fm.getProduction
		fm.GetContentEncoded = fm.getProductionEncoded
		fm.Exists = fm.ExistsProduction
		fm.OpenFile = fm.OpenProduction
	} else {
		fm.GetContent = fm.getDevelopment
		fm.GetContentEncoded = fm.getDevelopmentEncoded
		fm.Exists = fm.ExistsDevelopment
		fm.OpenFile = fm.OpenDevelopment
	}

	return fm
}

func (fm *FileManager) getDevelopment(path string) ([]byte, error) {
	return fm.fileAccess.Read(filepath.Join(fm.rootDir, path))
}

func (fm *FileManager) getDevelopmentEncoded(path string, encoding string) ([]byte, string, error) {
	// In development, we don't serve pre-compressed assets usually
	content, err := fm.getDevelopment(path)
	return content, "", err
}

func (fm *FileManager) getProduction(path string) ([]byte, error) {
	distPath, _, embedded, ok := fm.router.RouteWithBrotli(path)
	if !ok {
		return nil, ErrNotFound
	}

	if embedded != nil {
		return embedded, nil
	}

	return fm.readWithCache(distPath)
}

func (fm *FileManager) getProductionEncoded(path string, encoding string) ([]byte, string, error) {
	dist, br, embedded, ok := fm.router.RouteWithBrotli(path)
	if !ok {
		return nil, "", ErrNotFound
	}

	// Zero-IO High Priority
	if embedded != nil {
		return embedded, "", nil
	}

	// Balanced Encoding Selection
	target, actualEnc := dist, ""
	if encoding == "br" && br != "" {
		target, actualEnc = br, "br"
	}

	content, err := fm.readWithCache(target)
	return content, actualEnc, err
}

func (fm *FileManager) readWithCache(path string) ([]byte, error) {
	return fm.coalescer.Do(path, func() ([]byte, error) {
		if fm.cache != nil {
			if data, ok := fm.cache.Get(path); ok {
				metrics.Get().IncCacheHit()
				fmt.Printf("DEBUG: CACHE HIT for %s\n", path)
				return data, nil
			} else {
				fmt.Printf("DEBUG: CACHE MISS for %s\n", path)
			}
		}

		data, err := fm.fileAccess.Read(path)
		if err != nil {
			return nil, err
		}

		if fm.cache != nil {
			fm.cache.Set(path, data, time.Now().Add(24*time.Hour))
		}

		return data, nil
	})
}

// OpenFile opens a file for direct reading (used by ServeContent)
func (fm *FileManager) OpenDevelopment(path string) (*os.File, error) {
	return fm.fileAccess.Open(path)
}

func (fm *FileManager) OpenProduction(path string) (*os.File, error) {
	distPath, ok := fm.router.Route(path)
	if !ok {
		return nil, ErrNotFound
	}
	return fm.fileAccess.Open(distPath)
}

func (fm *FileManager) ExistsDevelopment(path string) bool {
	_, err := fm.fileAccess.Stat(filepath.Join(fm.rootDir, path))
	return err == nil
}

func (fm *FileManager) ExistsProduction(path string) bool {
	_, ok := fm.router.Route(path)
	return ok
}

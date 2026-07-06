package main

import (
	"encoding/json"
	"gogogo/modules/build"
	"gogogo/modules/router"
	"os"
	"sync"
)

type Cache struct {
	mu    sync.RWMutex
	data  map[string]CacheEntry
	dirty bool
}

type CacheEntry struct {
	FileInfo router.FileInfo
	RelPath  string
	Holes    []build.Hole
	FilePath string // for ActionFile entries — disk path the runtime sendfiles
}

type DependencyGraph struct {
	mu    sync.RWMutex
	nodes map[string]map[string]struct{}
}

func NewCache() *Cache {
	return &Cache{
		data: make(map[string]CacheEntry, defaultMapSize),
	}
}

func (c *Cache) Load(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if len(data) > 0 {
		return json.Unmarshal(data, &c.data)
	}
	return nil
}

func (c *Cache) Save(path string) error {
	c.mu.RLock()
	if !c.dirty {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(c.data)
	if err != nil {
		return err
	}

	return atomicWrite(path, data)
}

func (c *Cache) Get(key string) (CacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.data[key]
	return entry, ok
}

func (c *Cache) Set(key string, entry CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = entry
	c.dirty = true
}

func (c *Cache) GetAll() map[string]CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]CacheEntry, len(c.data))
	for k, v := range c.data {
		result[k] = v
	}
	return result
}

// BuildCache removed as it's merged into Cache

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		nodes: make(map[string]map[string]struct{}, defaultMapSize),
	}
}

func (dg *DependencyGraph) AddDependency(file, dependency string) {
	dg.mu.Lock()
	defer dg.mu.Unlock()

	if _, exists := dg.nodes[file]; !exists {
		dg.nodes[file] = make(map[string]struct{})
	}
	dg.nodes[file][dependency] = struct{}{}
}

func (dg *DependencyGraph) GetDependents(file string) []string {
	dg.mu.RLock()
	defer dg.mu.RUnlock()

	dependents := make([]string, 0)
	for dependent, deps := range dg.nodes {
		if _, exists := deps[file]; exists {
			dependents = append(dependents, dependent)
		}
	}
	return dependents
}

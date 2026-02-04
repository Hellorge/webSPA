package cache

import (
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/btree"
)

const (
    minShards     = 256
    maxShards     = 4096
    defaultShards = 512
)

type CacheEntry struct {
    Key        string
    Value      []byte
    Expiry     int64
    Frequency  uint32
    LastAccess int64
}

type Shard struct {
    items    *btree.BTree
    lock     sync.RWMutex
    maxItems int
}

type Cache struct {
    shards      [maxShards]*Shard  // Pre-allocate maximum possible shards
    activeShard int32             // Current active shard count
    maxSize     int
    resizing    bool
}

func entryCompare(a, b interface{}) bool {
    return a.(CacheEntry).Key < b.(CacheEntry).Key
}

func calculateOptimalShardCount() int32 {
    cpus := runtime.NumCPU()
    shardCount := int32(cpus * 64)

    if shardCount < minShards {
        return minShards
    }
    if shardCount > maxShards {
        return maxShards
    }
    return shardCount
}

func NewCache(maxSize int) *Cache {
    if maxSize <= 0 {
        maxSize = 10000
    }

    initialShards := calculateOptimalShardCount()
    cache := &Cache{
        maxSize: maxSize,
    }

    shardSize := maxSize / int(initialShards)
    if shardSize < 100 {
        shardSize = 100
    }

    for i := int32(0); i < initialShards; i++ {
        cache.shards[i] = &Shard{
            items:    btree.New(entryCompare),
            maxItems: shardSize,
        }
    }

    atomic.StoreInt32(&cache.activeShard, initialShards)

    // Start monitoring goroutine
    go cache.monitor()

    return cache
}

func (c *Cache) monitor() {
    ticker := time.NewTicker(time.Minute)
    for range ticker.C {
        c.adjustShards()
    }
}

func (c *Cache) adjustShards() {
    if c.resizing {
        return
    }

    contentionRate := c.getContentionRate()
    currentShards := atomic.LoadInt32(&c.activeShard)

    if contentionRate > 0.7 && currentShards < maxShards { // 70% threshold
        c.resizing = true
        newCount := currentShards * 2
        if newCount > maxShards {
            newCount = maxShards
        }
        c.rebalanceShards(newCount)
        atomic.StoreInt32(&c.activeShard, newCount)
        c.resizing = false
    }
}

func (c *Cache) getContentionRate() float64 {
    var contested int32
    activeShards := atomic.LoadInt32(&c.activeShard)

    for i := int32(0); i < activeShards; i++ {
        shard := c.shards[i]
        if !shard.lock.TryLock() {
            atomic.AddInt32(&contested, 1)
            continue
        }
        shard.lock.Unlock()
    }

    return float64(contested) / float64(activeShards)
}

func (c *Cache) shardIndex(key string) int32 {
    h := fnv.New64a()
    h.Write([]byte(key))
    activeShards := atomic.LoadInt32(&c.activeShard)
    return int32(h.Sum64() % uint64(activeShards))
}

func (c *Cache) Get(key string) ([]byte, bool) {
    idx := c.shardIndex(key)
    shard := c.shards[idx]

    shard.lock.RLock()
    item := shard.items.Get(CacheEntry{Key: key})
    if item == nil {
        shard.lock.RUnlock()
        return nil, false
    }

    entry := item.(CacheEntry)
    now := time.Now().Unix()

    if now > entry.Expiry {
        shard.lock.RUnlock()
        go c.cleanupEntry(shard, entry)
        return nil, false
    }

    value := entry.Value
    shard.lock.RUnlock()

    go c.updateEntryStats(shard, entry)
    return value, true
}

func (c *Cache) Set(key string, value []byte, expiry time.Time) {
    idx := c.shardIndex(key)
    shard := c.shards[idx]

    shard.lock.Lock()
    if shard.items.Len() >= shard.maxItems {
        c.evict(shard)
    }

    entry := CacheEntry{
        Key:        key,
        Value:      value,
        Expiry:     expiry.Unix(),
        Frequency:  1,
        LastAccess: time.Now().Unix(),
    }

    shard.items.Set(entry)
    shard.lock.Unlock()
}

func (c *Cache) cleanupEntry(shard *Shard, entry CacheEntry) {
    shard.lock.Lock()
    if current := shard.items.Get(CacheEntry{Key: entry.Key}); current != nil {
        currentEntry := current.(CacheEntry)
        if currentEntry.Expiry == entry.Expiry {
            shard.items.Delete(entry)
        }
    }
    shard.lock.Unlock()
}

func (c *Cache) updateEntryStats(shard *Shard, entry CacheEntry) {
    shard.lock.Lock()
    if current := shard.items.Get(CacheEntry{Key: entry.Key}); current != nil {
        currentEntry := current.(CacheEntry)
        if currentEntry.Expiry == entry.Expiry {
            currentEntry.Frequency = atomic.AddUint32(&currentEntry.Frequency, 1)
            currentEntry.LastAccess = time.Now().Unix()
            shard.items.Set(currentEntry)
        }
    }
    shard.lock.Unlock()
}

func (c *Cache) evict(shard *Shard) {
    now := time.Now().Unix()
    maxEvict := shard.items.Len() / 4
    candidates := make([]CacheEntry, 0, maxEvict)

    shard.items.Ascend(CacheEntry{}, func(item interface{}) bool {
        entry := item.(CacheEntry)
        if now > entry.Expiry ||
           (now - entry.LastAccess > 3600 && entry.Frequency < 10) {
            candidates = append(candidates, entry)
        }
        return len(candidates) < maxEvict
    })

    if len(candidates) > 0 {
        for _, entry := range candidates {
            shard.items.Delete(entry)
        }
    }
}

func (c *Cache) rebalanceShards(newCount int32) {
    // Create temporary storage for entries
    allEntries := make([]CacheEntry, 0)
    currentShards := atomic.LoadInt32(&c.activeShard)

    // Collect all entries
    for i := int32(0); i < currentShards; i++ {
        shard := c.shards[i]
        shard.lock.Lock()
        shard.items.Ascend(CacheEntry{}, func(item interface{}) bool {
            allEntries = append(allEntries, item.(CacheEntry))
            return true
        })
        shard.lock.Unlock()
    }

    // Reinitialize shards
    shardSize := c.maxSize / int(newCount)
    for i := int32(0); i < newCount; i++ {
        c.shards[i] = &Shard{
            items:    btree.New(entryCompare),
            maxItems: shardSize,
        }
    }

    // Redistribute entries
    for _, entry := range allEntries {
        h := fnv.New64a()
        h.Write([]byte(entry.Key))
        idx := int32(h.Sum64() % uint64(newCount))
        shard := c.shards[idx]
        shard.lock.Lock()
        shard.items.Set(entry)
        shard.lock.Unlock()
    }
}

func (c *Cache) Clear() {
    activeShards := atomic.LoadInt32(&c.activeShard)
    for i := int32(0); i < activeShards; i++ {
        shard := c.shards[i]
        shard.lock.Lock()
        shard.items = btree.New(entryCompare)
        shard.lock.Unlock()
    }
}

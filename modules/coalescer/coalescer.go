package coalescer

import (
	"hash/fnv"
	"sync"
	"gogogo/modules/metrics"
)

const shardCount = 32 // Balance between memory usage and lock contention

type Call struct {
	wg     sync.WaitGroup
	val    []byte
	err    error
	loaded int32
}

type Shard struct {
	sync.RWMutex
	calls map[string]*Call
}

type Coalescer struct {
	shards [shardCount]Shard
}

func NewCoalescer() *Coalescer {
	c := &Coalescer{}
	// Initialize shards
	for i := range c.shards {
		c.shards[i].calls = make(map[string]*Call)
	}
	return c
}

func (c *Coalescer) getShard(key string) *Shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &c.shards[h.Sum32()%shardCount]
}

// Do coalesces multiple requests for the same key into a single operation
func (c *Coalescer) Do(key string, fn func() ([]byte, error)) ([]byte, error) {
    shard := c.getShard(key)

    // Fast path with read lock
    shard.RLock()
    if call, ok := shard.calls[key]; ok {
        shard.RUnlock()
		metrics.Get().IncCoalesced()
        call.wg.Wait()
        return call.val, call.err
    }
    shard.RUnlock()

    // Slow path needs write lock
    shard.Lock()
    if call, ok := shard.calls[key]; ok {
        shard.Unlock()
		metrics.Get().IncCoalesced()
        call.wg.Wait()
        return call.val, call.err
    }

    call := &Call{}
    call.wg.Add(1)
    shard.calls[key] = call
    shard.Unlock()

    // Execute function
    call.val, call.err = fn()

    // Cleanup with write lock
    shard.Lock()
    delete(shard.calls, key)
    shard.Unlock()

    call.wg.Done()
    return call.val, call.err
}

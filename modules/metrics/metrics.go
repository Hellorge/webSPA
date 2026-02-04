package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// MetricCollector handles high-performance metric gathering using atomic operations.
type MetricCollector struct {
	totalRequests  uint64
	totalLatency   uint64 // in nanoseconds
	totalBytes     uint64
	activeRequest  uint64
	cacheHits      uint64
	coalescedReqs  uint64

	startTime time.Time
	
	// Optional: Snapshot storage for P99 etc.
	mu sync.RWMutex
}

var instance *MetricCollector
var once sync.Once

// Get returns the singleton instance of the metric collector.
func Get() *MetricCollector {
	once.Do(func() {
		instance = &MetricCollector{
			startTime: time.Now(),
		}
	})
	return instance
}

// StartRequest increments the active request counter.
func (c *MetricCollector) StartRequest() {
	atomic.AddUint64(&c.activeRequest, 1)
	atomic.AddUint64(&c.totalRequests, 1)
}

// EndRequest decrements the active request counter and records latency.
func (c *MetricCollector) EndRequest(duration time.Duration, bytes uint64) {
	atomic.AddUint64(&c.activeRequest, ^uint64(0)) // Decrement by 1
	atomic.AddUint64(&c.totalLatency, uint64(duration.Nanoseconds()))
	atomic.AddUint64(&c.totalBytes, bytes)
}

func (c *MetricCollector) IncCacheHit() {
	atomic.AddUint64(&c.cacheHits, 1)
}

func (c *MetricCollector) IncCoalesced() {
	atomic.AddUint64(&c.coalescedReqs, 1)
}

// Snapshot returns a copy of current metrics.
type Snapshot struct {
	TotalRequests  uint64
	TotalBytes     uint64
	ActiveRequests uint64
	AvgLatency     time.Duration
	Uptime         time.Duration
	Throughput     float64 // MiB/s
	CacheHits      uint64
	CoalescedReqs  uint64
}

func (c *MetricCollector) GetSnapshot() Snapshot {
	uptime := time.Since(c.startTime)
	totalReq := atomic.LoadUint64(&c.totalRequests)
	totalLat := atomic.LoadUint64(&c.totalLatency)
	totalBytes := atomic.LoadUint64(&c.totalBytes)
	active := atomic.LoadUint64(&c.activeRequest)

	var avgLat time.Duration
	if totalReq > 0 {
		avgLat = time.Duration(totalLat / totalReq)
	}

	throughput := float64(totalBytes) / (1024 * 1024) / uptime.Seconds()

	return Snapshot{
		TotalRequests:  totalReq,
		TotalBytes:     totalBytes,
		ActiveRequests: active,
		AvgLatency:     avgLat,
		Uptime:         uptime,
		Throughput:     throughput,
		CacheHits:      atomic.LoadUint64(&c.cacheHits),
		CoalescedReqs:  atomic.LoadUint64(&c.coalescedReqs),
	}
}

package metrics

import (
	"sync/atomic"
	"time"
)

// BouncerMetric holds high-performance counters for a specific CPU core/bouncer.
// It is padded to prevent false sharing (L1 cache line contention).
type BouncerMetric struct {
	TotalRequests uint64
	TotalLatency  uint64
	TotalBytes    uint64
	Active        uint64
	_             [32]byte // Padding to 64 bytes
}

type MetricCollector struct {
	segments      []BouncerMetric
	cacheHits     uint64
	coalescedReqs uint64
	startTime     time.Time
}

var instance *MetricCollector

// Init initializes the segmented collector with the given number of bouncers.
func Init(numBouncers int) {
	instance = &MetricCollector{
		segments:  make([]BouncerMetric, numBouncers),
		startTime: time.Now(),
	}
}

func Get() *MetricCollector {
	return instance
}

func (c *MetricCollector) IncCacheHit() {
	atomic.AddUint64(&c.cacheHits, 1)
}

func (c *MetricCollector) IncCoalesced() {
	atomic.AddUint64(&c.coalescedReqs, 1)
}

// Record hooks into the end of a connection lifecycle (Passive Recording)
func (c *MetricCollector) Record(bouncerID int, duration time.Duration, bytes uint64) {
	if bouncerID >= len(c.segments) { return }
	seg := &c.segments[bouncerID]
	atomic.AddUint64(&seg.TotalRequests, 1)
	atomic.AddUint64(&seg.TotalLatency, uint64(duration.Nanoseconds()))
	atomic.AddUint64(&seg.TotalBytes, bytes)
}

// DecActive is called when a connection starts/ends
func (c *MetricCollector) IncActive(bouncerID int) {
	if bouncerID >= len(c.segments) { return }
	atomic.AddUint64(&c.segments[bouncerID].Active, 1)
}

func (c *MetricCollector) DecActive(bouncerID int) {
	if bouncerID >= len(c.segments) { return }
	atomic.AddUint64(&c.segments[bouncerID].Active, ^uint64(0))
}

type Snapshot struct {
	TotalRequests  uint64  `json:"total_requests"`
	TotalBytes     uint64  `json:"total_bytes"`
	ActiveRequests uint64  `json:"active_requests"`
	AvgLatency     int64   `json:"avg_latency"` 
	Uptime         int64   `json:"uptime"`
	Throughput     float64 `json:"throughput"`
	Bouncers       int     `json:"bouncers"`
	CacheHits      uint64  `json:"cache_hits"`
	CoalescedReqs  uint64  `json:"coalesced_reqs"`
}

func (c *MetricCollector) GetSnapshot() Snapshot {
	uptime := time.Since(c.startTime)
	var totalReq, totalLat, totalBytes, active uint64

	for i := range c.segments {
		totalReq += atomic.LoadUint64(&c.segments[i].TotalRequests)
		totalLat += atomic.LoadUint64(&c.segments[i].TotalLatency)
		totalBytes += atomic.LoadUint64(&c.segments[i].TotalBytes)
		active += atomic.LoadUint64(&c.segments[i].Active)
	}

	var avgLat time.Duration
	if totalReq > 0 {
		avgLat = time.Duration(totalLat / totalReq)
	}

	throughput := float64(totalBytes) / (1024 * 1024) / uptime.Seconds()

	return Snapshot{
		TotalRequests:  totalReq,
		TotalBytes:     totalBytes,
		ActiveRequests: active,
		AvgLatency:     int64(avgLat),
		Uptime:         int64(uptime),
		Throughput:     throughput,
		Bouncers:       len(c.segments),
		CacheHits:      atomic.LoadUint64(&c.cacheHits),
		CoalescedReqs:  atomic.LoadUint64(&c.coalescedReqs),
	}
}

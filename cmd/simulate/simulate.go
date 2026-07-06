package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	targetURL   string
	hostHeader  string
	concurrency int
	duration    time.Duration
	totalReqs   uint64
	successReqs uint64
	failedReqs  uint64
	bytesRead   uint64
)

// latencies are sampled per-worker into thread-local slabs to avoid lock
// contention on the hot path; merged at the end. Each sample is one
// successful request's wall-clock latency in nanoseconds.

func main() {
	flag.StringVar(&targetURL, "url", "http://localhost:8082/about", "Target URL")
	flag.StringVar(&hostHeader, "host", "", "Override Host header (for vhost testing, e.g. docs.localhost)")
	flag.IntVar(&concurrency, "c", 100, "Concurrency level (one keep-alive conn per worker)")
	flag.DurationVar(&duration, "d", 10*time.Second, "Duration")
	flag.Parse()

	fmt.Printf("Gogogo loadgen → %s", targetURL)
	if hostHeader != "" {
		fmt.Printf("  (Host: %s)", hostHeader)
	}
	fmt.Printf("\nConcurrency: %d | Duration: %v\n\n", concurrency, duration)

	// Per-worker latency slabs. Pre-size assuming up to ~50k req/sec/worker;
	// we'll truncate or reslice at the end.
	slabSize := 200_000
	latencies := make([][]int64, concurrency)
	for i := range latencies {
		latencies[i] = make([]int64, 0, slabSize)
	}

	tr := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		MaxConnsPerHost:     concurrency * 2,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false,
		DisableCompression:  true, // we want raw bytes off the wire, not Go-decompressed
		ForceAttemptHTTP2:   false,
		DialContext: (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			endTime := start.Add(duration)
			for time.Now().Before(endTime) {
				doRequest(client, &latencies[idx])
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Merge latency slabs.
	total := 0
	for _, s := range latencies {
		total += len(s)
	}
	merged := make([]int64, 0, total)
	for _, s := range latencies {
		merged = append(merged, s...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })

	printStats(elapsed, merged)
}

func doRequest(client *http.Client, slab *[]int64) {
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		atomic.AddUint64(&failedReqs, 1)
		atomic.AddUint64(&totalReqs, 1)
		return
	}
	if hostHeader != "" {
		req.Host = hostHeader
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		atomic.AddUint64(&failedReqs, 1)
		atomic.AddUint64(&totalReqs, 1)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		dt := time.Since(t0).Nanoseconds()
		*slab = append(*slab, dt)
		atomic.AddUint64(&successReqs, 1)
		atomic.AddUint64(&bytesRead, uint64(n))
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		atomic.AddUint64(&failedReqs, 1)
	}
	atomic.AddUint64(&totalReqs, 1)
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func printStats(elapsed time.Duration, latSorted []int64) {
	reqs := atomic.LoadUint64(&totalReqs)
	succ := atomic.LoadUint64(&successReqs)
	fail := atomic.LoadUint64(&failedReqs)
	bytes := atomic.LoadUint64(&bytesRead)

	qps := float64(reqs) / elapsed.Seconds()
	mbps := float64(bytes) * 8 / 1_000_000 / elapsed.Seconds()

	fmt.Println("------------------------------------------------")
	fmt.Printf("Requests:        %d  (success %d, fail %d)\n", reqs, succ, fail)
	fmt.Printf("Duration:        %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Bytes read:      %d  (%.1f MiB)\n", bytes, float64(bytes)/(1<<20))
	fmt.Println("------------------------------------------------")
	fmt.Printf("Throughput:      %.0f req/s   (%.1f Mbps)\n", qps, mbps)
	if len(latSorted) > 0 {
		fmt.Printf("Latency p50:     %v\n", time.Duration(percentile(latSorted, 0.50)))
		fmt.Printf("Latency p90:     %v\n", time.Duration(percentile(latSorted, 0.90)))
		fmt.Printf("Latency p99:     %v\n", time.Duration(percentile(latSorted, 0.99)))
		fmt.Printf("Latency p999:    %v\n", time.Duration(percentile(latSorted, 0.999)))
		fmt.Printf("Latency max:     %v\n", time.Duration(latSorted[len(latSorted)-1]))
	}
	fmt.Println("------------------------------------------------")
}

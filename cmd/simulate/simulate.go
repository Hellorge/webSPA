package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type MetricsSnapshot struct {
	TotalRequests  uint64  `json:"total_requests"`
	TotalBytes     uint64  `json:"total_bytes"`
	ActiveRequests uint64  `json:"active_requests"`
	AvgLatency     uint64  `json:"avg_latency"` // nanoseconds
	Uptime         float64 `json:"uptime"`      // seconds (float from server likely)
	Throughput     float64 `json:"throughput"` // MiB/s
	CacheHits      uint64  `json:"cache_hits"`
	CoalescedReqs  uint64  `json:"coalesced_reqs"`
}

var (
	targetURL   string
	concurrency int
	duration    time.Duration
	totalReqs   uint64
	successReqs uint64
	failedReqs  uint64
	bytesRead   uint64
)

func main() {
	flag.StringVar(&targetURL, "url", "http://localhost:8080", "Target URL")
	flag.IntVar(&concurrency, "c", 100, "Concurrency level")
	flag.DurationVar(&duration, "d", 10*time.Second, "Duration")
	flag.Parse()

	fmt.Printf("Divine Simulation Initiated: %s\n", targetURL)
	fmt.Printf("Concurrency: %d | Duration: %v\n\n", concurrency, duration)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(concurrency)

	// Divine Worker Pool
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			client := &http.Client{
				Transport: &http.Transport{
					MaxIdleConnsPerHost: 1000,
					DisableKeepAlives:   false,
				},
				Timeout: 5 * time.Second,
			}

			endTime := time.Now().Add(duration)
			for time.Now().Before(endTime) {
				doRequest(client)
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	printStats(elapsed)
}

func doRequest(client *http.Client) {
	resp, err := client.Get(targetURL)
	atomic.AddUint64(&totalReqs, 1)
	if err != nil {
		atomic.AddUint64(&failedReqs, 1)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		atomic.AddUint64(&successReqs, 1)
		n, _ := io.Copy(io.Discard, resp.Body)
		atomic.AddUint64(&bytesRead, uint64(n))
	} else {
		atomic.AddUint64(&failedReqs, 1)
	}
}

func printStats(elapsed time.Duration) {
	reqs := atomic.LoadUint64(&totalReqs)
	succ := atomic.LoadUint64(&successReqs)
	fail := atomic.LoadUint64(&failedReqs)
	bytes := atomic.LoadUint64(&bytesRead)

	qps := float64(reqs) / elapsed.Seconds()
	mbps := float64(bytes) * 8 / 1000000 / elapsed.Seconds()

	fmt.Println("------------------------------------------------")
	fmt.Printf("Divine Speed Results:\n")
	fmt.Printf("Total Requests: %d\n", reqs)
	fmt.Printf("Success:        %d\n", succ)
	fmt.Printf("Failed:         %d\n", fail)
	fmt.Printf("Duration:       %v\n", elapsed)
	fmt.Println("------------------------------------------------")
	fmt.Printf("Requests/sec:   %.2f\n", qps)
	fmt.Printf("Throughput:     %.2f Mbps\n", mbps)
	fmt.Println("------------------------------------------------")

	// Fetch server-side metrics
	fetchAndPrintServerMetrics()
}

func fetchAndPrintServerMetrics() {
	// Assume metric endpoint is at root host + /api/metrics
	// In a real CLI we might parse targetURL to find the host.
	// For now, hardcode or try to derive.
	resp, err := http.Get("http://localhost:8080/api/metrics")
	if err != nil {
		fmt.Printf("Could not fetch server metrics: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var snapshot MetricsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		fmt.Printf("Failed to decode server metrics: %v\n", err)
		return
	}

	fmt.Println("\nServer-Side Verification:")
	fmt.Println("------------------------------------------------")
	fmt.Printf("Cache Hits:  %d\n", snapshot.CacheHits)
	fmt.Printf("Request Merges (Coalesced): %d\n", snapshot.CoalescedReqs)
	fmt.Println("------------------------------------------------")
}

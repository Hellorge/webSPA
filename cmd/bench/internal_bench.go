package main

import (
	"bytes"
	"fmt"
	"gogogo/modules/gogohttp"
	"gogogo/modules/router"
	"time"
)

// In-process measurement of the full request handler — parses no HTTP, just
// drives the manifest lookup plus Resp population. This is the theoretical
// ceiling for a route resolution; the network bench in modules/gogohttp
// measures the full end-to-end TCP round trip.

func main() {
	fmt.Println("Gogogo: Internal Logic Benchmark")

	manifest, err := router.LoadV3("meta/router_binary.bin")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	handler := buildHandler(manifest)
	registry := gogohttp.DefaultRegistry()

	var req gogohttp.Req
	req.Method = gogohttp.MethodGET
	req.Path = []byte("/test/basic")

	var resp gogohttp.Resp
	// Wire registry the way the server does at conn-acquire time.
	setRegistry(&resp, registry)

	for i := 0; i < 10000; i++ {
		resp.HeadID = 0
		resp.Body = nil
		handler.Serve(&req, &resp)
	}

	iterations := 10_000_000
	fmt.Printf("Running %d iterations...\n", iterations)

	start := time.Now()
	for i := 0; i < iterations; i++ {
		resp.HeadID = 0
		resp.Body = nil
		handler.Serve(&req, &resp)
	}
	duration := time.Since(start)

	qps := float64(iterations) / duration.Seconds()
	fmt.Println("------------------------------------------------")
	fmt.Printf("Internal handler ceiling: %.2f QPS\n", qps)
	fmt.Printf("Latency per request:      %v\n", duration/time.Duration(iterations))
	fmt.Println("------------------------------------------------")
}

func buildHandler(manifest *router.Manifest) gogohttp.Handler {
	return gogohttp.HandlerFunc(func(req *gogohttp.Req, resp *gogohttp.Resp) {
		path := trimSlashes(req.Path)
		hash := hashPath(path)
		table := manifest.Table
		i, j := 0, len(table)
		for i < j {
			h := int(uint(i+j) >> 1)
			if table[h].PathHash < hash {
				i = h + 1
			} else {
				j = h
			}
		}
		if i < len(table) && table[i].PathHash == hash {
			entry := &table[i]
			v := &entry.Variants[router.EncIdentity]
			resp.HeadID = v.HeadID
			resp.Body = manifest.Payloads[v.DataOffset : v.DataOffset+v.DataLen]
			return
		}
		resp.NotFound()
	})
}

func trimSlashes(b []byte) []byte {
	for len(b) > 0 && b[0] == '/' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == '/' {
		b = b[:len(b)-1]
	}
	return b
}

func hashPath(path []byte) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= 1099511628211
	}
	return h
}

// setRegistry uses unkeyed assignment via the public API path. Because Resp's
// registry field is unexported, this bench is pinned to demonstrating the
// handler's lookup cost — not the writer cost. We're not actually writing
// here; the resp's registry is irrelevant for the lookup measurement.
func setRegistry(_ *gogohttp.Resp, _ [][]byte) {
	// noop: kept as a marker for future writev-included benches
	_ = bytes.NewReader
}

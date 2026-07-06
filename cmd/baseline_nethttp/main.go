// baseline_nethttp serves the same response body gogogo's /about route
// serves, via Go's stdlib net/http. Apples-to-apples: same workload,
// same machine, same loadgen — only the HTTP engine differs.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"

	"gogogo/modules/server"
)

func main() {
	addr := flag.String("addr", ":9090", "listen addr")
	bodyFile := flag.String("body", "", "file to serve as body; if empty, uses 4KB synthetic html")
	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())

	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			log.Fatalf("read body: %v", err)
		}
		body = b
	} else {
		body = makeSynthetic(4 * 1024)
	}
	contentLen := fmt.Sprintf("%d", len(body))
	log.Printf("serving %d bytes per response on %s", len(body), *addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", contentLen)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	// Same SO_REUSEPORT bouncer fanout gogogo uses, so the server-side
	// scheduling shape is identical.
	for i := 0; i < runtime.NumCPU(); i++ {
		ln, err := server.NewReusePortListener("tcp", *addr)
		if err != nil {
			log.Fatalf("listener %d: %v", i, err)
		}
		srv := &http.Server{Handler: mux}
		go func(s *http.Server) {
			if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("serve: %v", err)
			}
		}(srv)
	}

	select {}
}

func makeSynthetic(n int) []byte {
	const tmpl = "<!doctype html><html><head><title>About</title></head><body>" +
		"<h1>About</h1><p>This is a baseline net/http page intended to be the " +
		"same byte-shape as gogogo's /about page so QPS comparisons are fair.</p>"
	buf := make([]byte, 0, n)
	buf = append(buf, tmpl...)
	for len(buf) < n-7 {
		buf = append(buf, "<p>fillerfillerfillerfiller</p>"...)
	}
	buf = append(buf, "</body>"...)
	if len(buf) > n {
		buf = buf[:n]
	}
	return buf
}

// baseline_fasthttp serves the same body via valyala/fasthttp — the
// reference "extreme Go" HTTP server. Same 4KB synthetic, same loadgen,
// same machine. Apples to apples vs gogogo and net/http.

package main

import (
	"flag"
	"log"
	"runtime"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

func main() {
	addr := flag.String("addr", ":9091", "listen addr")
	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())

	body := makeSynthetic(4 * 1024)
	log.Printf("serving %d bytes per response on %s", len(body), *addr)

	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetContentType("text/html; charset=utf-8")
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBody(body)
	}

	// fasthttp ships its own reuseport package — same SO_REUSEPORT
	// fanout pattern as gogogo and the net/http baseline.
	for i := 0; i < runtime.NumCPU(); i++ {
		ln, err := reuseport.Listen("tcp4", *addr)
		if err != nil {
			log.Fatalf("listener %d: %v", i, err)
		}
		s := &fasthttp.Server{
			Handler: handler,
			Name:    "fasthttp-baseline",
		}
		go func(srv *fasthttp.Server) {
			if err := srv.Serve(ln); err != nil {
				log.Printf("serve: %v", err)
			}
		}(s)
	}

	select {}
}

func makeSynthetic(n int) []byte {
	const tmpl = "<!doctype html><html><head><title>About</title></head><body>" +
		"<h1>About</h1><p>This is a baseline fasthttp page intended to be the " +
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

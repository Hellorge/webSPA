package gogohttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Well-known HeadIDs the tests use. The library has no opinion on these —
// each application picks its own layout. This block is what gogogo's main
// will look like in Phase 3.
const (
	tHead200HTML uint16 = 1
	tHead200JSON uint16 = 2
	tHead200MP4  uint16 = 3
	tHead206MP4  uint16 = 4
	tHead404     uint16 = 5
	tHead500     uint16 = 6
)

func testRegistry() [][]byte {
	r := make([][]byte, 32)
	r[tHead200HTML] = []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: text/html\r\n")
	r[tHead200JSON] = []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: application/json\r\n")
	r[tHead200MP4] = []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: video/mp4\r\nAccept-Ranges: bytes\r\n")
	r[tHead206MP4] = []byte("HTTP/1.1 206 Partial Content\r\nServer: Gogogo\r\nContent-Type: video/mp4\r\nAccept-Ranges: bytes\r\n")
	r[tHead404] = []byte("HTTP/1.1 404 Not Found\r\nServer: Gogogo\r\nContent-Type: text/plain\r\n")
	r[tHead500] = []byte("HTTP/1.1 500 Internal Server Error\r\nServer: Gogogo\r\nContent-Type: text/plain\r\n")
	return r
}

func startServer(t testing.TB, h Handler, registry [][]byte) (string, func()) {
	t.Helper()
	if registry == nil {
		registry = testRegistry()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Handler:           h,
		HeaderRegistry:    registry,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		s.Serve(ln)
		close(done)
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		s.Shutdown()
		<-done
	}
}

func dial(t testing.TB, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServerGet(t *testing.T) {
	body := []byte("hi")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		if string(req.Path) != "/hello" {
			resp.HeadID = tHead404
			return
		}
		resp.HeadID = tHead200HTML
		resp.Body = body
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /hello HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "hi" {
		t.Fatalf("body=%q", got)
	}
	if resp.Header.Get("Server") != "Gogogo" {
		t.Errorf("server=%q", resp.Header.Get("Server"))
	}
	if resp.Header.Get("Content-Type") != "text/html" {
		t.Errorf("ct=%q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Date") == "" {
		t.Error("missing Date header")
	}
	if resp.ContentLength != 2 {
		t.Errorf("CL=%d", resp.ContentLength)
	}
}

func TestServerHEAD(t *testing.T) {
	body := []byte("<html>hello</html>")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Body = body
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("HEAD / HTTP/1.1\r\nHost: x\r\n\r\n"))

	br := bufio.NewReader(c)
	// Read status line + headers.
	resp, err := http.ReadResponse(br, &http.Request{Method: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("CL=%d want=%d", resp.ContentLength, len(body))
	}
	// Body must be empty for HEAD.
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Fatalf("HEAD returned body of %d bytes", len(got))
	}
}

func TestServer404FromHandler(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead404
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /missing HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d want=404", resp.StatusCode)
	}
}

func TestServerKeepAlive(t *testing.T) {
	var calls int32
	body := []byte("ok")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		atomic.AddInt32(&calls, 1)
		resp.HeadID = tHead200HTML
		resp.Body = body
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	br := bufio.NewReader(c)

	for i := 0; i < 5; i++ {
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("iter %d: status=%d", i, resp.StatusCode)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("calls=%d want=5", got)
	}
}

func TestServerConnectionClose(t *testing.T) {
	body := []byte("bye")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Body = body
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	// net/http parses "Connection: close" into resp.Close, not resp.Header.
	if !resp.Close {
		t.Errorf("expected resp.Close=true (Connection: close in response)")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected EOF after Connection: close")
	}
}

func TestServerPostBody(t *testing.T) {
	var got []byte
	h := HandlerFunc(func(req *Req, resp *Resp) {
		got = append(got[:0], req.Body...)
		resp.HeadID = tHead200JSON
		resp.Body = []byte(`{"ok":true}`)
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	body := "hello world"
	fmt.Fprintf(c, "POST /api HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("ct=%q", resp.Header.Get("Content-Type"))
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if string(got) != body {
		t.Fatalf("body=%q want=%q", got, body)
	}
}

func TestServerRangeRequest(t *testing.T) {
	// Simulate a video file. Handler responds to a Range request with 206
	// + Content-Range using SetContentRange — same code path as a normal
	// 200 response.
	full := bytes.Repeat([]byte("x"), 1000)
	h := HandlerFunc(func(req *Req, resp *Resp) {
		// Parse Range header (ours has it in req.Headers; we'd pull it out
		// in a real handler; here we hard-code the slice for the test).
		start, end := int64(100), int64(199)
		resp.HeadID = tHead206MP4
		resp.Body = full[start : end+1]
		resp.SetContentRange(start, end, int64(len(full)))
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /v.mp4 HTTP/1.1\r\nHost: x\r\nRange: bytes=100-199\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 206 {
		t.Fatalf("status=%d want=206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 100-199/1000" {
		t.Errorf("Content-Range=%q", cr)
	}
	if resp.ContentLength != 100 {
		t.Errorf("CL=%d", resp.ContentLength)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 100 {
		t.Fatalf("body len=%d", len(got))
	}
}

func TestServerHTTP10ClosesByDefault(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Body = []byte("k")
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.0\r\nHost: x\r\n\r\n"))

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected close after HTTP/1.0")
	}
}

func TestServerRejectsMalformed(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Body = []byte("ok")
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("FOOBAR / HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want=400", resp.StatusCode)
	}
}

func TestServerHandlerPanicReturns500(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		panic("boom")
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d want=500", resp.StatusCode)
	}
}

func TestServerSlowlorisHeaderTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Handler:           HandlerFunc(func(*Req, *Resp) {}),
		HeaderRegistry:    testRegistry(),
		ReadHeaderTimeout: 200 * time.Millisecond,
	}
	go s.Serve(ln)
	defer func() { ln.Close(); s.Shutdown() }()

	c := dial(t, ln.Addr().String())
	defer c.Close()
	c.Write([]byte("GET /"))

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	if n > 0 && !strings.Contains(string(buf[:n]), "HTTP/1.1") {
		t.Fatalf("unexpected payload: %q", buf[:n])
	}
}

// --- Streaming ---

func TestServerStreamBasic(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Stream = func(w io.Writer) error {
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "chunk-%d\n", i)
			}
			return nil
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("expected chunked, got %v", resp.TransferEncoding)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "chunk-0\nchunk-1\nchunk-2\nchunk-3\nchunk-4\n"
	if string(body) != want {
		t.Fatalf("body=%q want=%q", body, want)
	}
}

func TestServerStreamLarge(t *testing.T) {
	// File-download style: handler streams a large payload via io.Copy.
	payload := bytes.Repeat([]byte("ABCDEFGH"), 100_000) // 800 KB
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Stream = func(w io.Writer) error {
			_, err := io.Copy(w, bytes.NewReader(payload))
			return err
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body length=%d want=%d (mismatch)", len(body), len(payload))
	}
}

func TestServerStreamKeepAlive(t *testing.T) {
	// After a chunked response ends with the trailer, the conn can be reused.
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Stream = func(w io.Writer) error {
			w.Write([]byte("ping"))
			return nil
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	br := bufio.NewReader(c)

	for i := 0; i < 3; i++ {
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ping" {
			t.Fatalf("iter %d: body=%q", i, body)
		}
	}
}

func TestServerStreamHEAD(t *testing.T) {
	streamCalled := false
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Stream = func(w io.Writer) error {
			streamCalled = true
			return nil
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("HEAD / HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD returned body of %d bytes", len(body))
	}
	if streamCalled {
		t.Fatal("Stream closure must not be called for HEAD")
	}
}

func TestServerSSEPattern(t *testing.T) {
	// Server-Sent Events: handler emits events over time; client reads them
	// as they arrive. We verify framing parses cleanly.
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML // would normally be "Content-Type: text/event-stream"
		resp.Stream = func(w io.Writer) error {
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "data: event-%d\n\n", i)
			}
			return nil
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /sse HTTP/1.1\r\nHost: x\r\n\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "data: event-0\n\ndata: event-1\n\ndata: event-2\n\n"
	if string(body) != want {
		t.Fatalf("body=%q want=%q", body, want)
	}
}

// --- Hijack ---

func TestServerHijack(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Hijack = func(c net.Conn) error {
			// Write some raw bytes that aren't HTTP-framed.
			_, err := c.Write([]byte("RAWDATA"))
			return err
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	all, _ := io.ReadAll(c)
	if !bytes.Contains(all, []byte("HTTP/1.1 200")) {
		t.Fatalf("missing response head: %q", all)
	}
	if !bytes.Contains(all, []byte("RAWDATA")) {
		t.Fatalf("missing raw bytes: %q", all)
	}
	// After hijack returns, server closes — verify by trying another request
	// on the same conn (should fail).
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\n")); err != nil {
		// already closed — fine
		return
	}
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	one := make([]byte, 1)
	if _, err := c.Read(one); err == nil {
		t.Fatal("expected conn closed after hijack")
	}
}

func TestServerHijackEcho(t *testing.T) {
	// Handler echoes whatever the client sends after the upgrade.
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Hijack = func(c net.Conn) error {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 64)
			n, err := c.Read(buf)
			if err != nil {
				return err
			}
			_, err = c.Write(buf[:n])
			return err
		}
	})
	addr, stop := startServer(t, h, nil)
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /upgrade HTTP/1.1\r\nHost: x\r\n\r\n"))

	// Skip past the response head.
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	c.Write([]byte("HELLO"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("echo=%q", got)
	}
}

// --- Benchmarks ---

func BenchmarkServerOurs(b *testing.B) {
	body := []byte("<html>hello</html>")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Body = body
	})
	addr, stop := startServer(b, h, nil)
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	req := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkServerStreaming(b *testing.B) {
	payload := []byte("<html>hello</html>")
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.HeadID = tHead200HTML
		resp.Stream = func(w io.Writer) error {
			_, err := w.Write(payload)
			return err
		}
	})
	addr, stop := startServer(b, h, nil)
	defer stop()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	req := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkServerStdlib(b *testing.B) {
	body := []byte("<html>hello</html>")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(body)
	})}
	go srv.Serve(ln)
	defer func() { ln.Close(); srv.Close() }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	req := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(req); err != nil {
			b.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

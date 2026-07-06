package gogohttp

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestParseGET(t *testing.T) {
	raw := "GET /home HTTP/1.1\r\nHost: example.com\r\nUser-Agent: curl/8\r\n\r\n"
	var req Req
	n, ok, err := Parse([]byte(raw), &req)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected complete")
	}
	if n != len(raw) {
		t.Fatalf("n=%d want=%d", n, len(raw))
	}
	if req.Method != MethodGET {
		t.Fatalf("method=%v", req.Method)
	}
	if string(req.Path) != "/home" {
		t.Fatalf("path=%q", req.Path)
	}
	if req.Query != nil {
		t.Fatalf("query=%q want nil", req.Query)
	}
	if !bytes.Equal(req.Host, []byte("example.com")) {
		t.Fatalf("host=%q", req.Host)
	}
	if !req.KeepAlive {
		t.Fatal("expected keep-alive")
	}
	if req.NHeaders != 2 {
		t.Fatalf("nheaders=%d", req.NHeaders)
	}
}

func TestParsePathQuery(t *testing.T) {
	raw := "GET /search?q=hello&limit=10 HTTP/1.1\r\nHost: x\r\n\r\n"
	var req Req
	if _, _, err := Parse([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if string(req.Path) != "/search" {
		t.Fatalf("path=%q", req.Path)
	}
	if string(req.Query) != "q=hello&limit=10" {
		t.Fatalf("query=%q", req.Query)
	}
}

func TestParseContentLength(t *testing.T) {
	raw := "POST /api HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\nhello"
	var req Req
	n, ok, err := Parse([]byte(raw), &req)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected complete")
	}
	if req.ContentLength != 5 {
		t.Fatalf("cl=%d", req.ContentLength)
	}
	if string([]byte(raw)[n:]) != "hello" {
		t.Fatalf("body=%q", []byte(raw)[n:])
	}
}

func TestParseHTTP10NoKeepAlive(t *testing.T) {
	raw := "GET / HTTP/1.0\r\nHost: x\r\n\r\n"
	var req Req
	if _, _, err := Parse([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.KeepAlive {
		t.Fatal("HTTP/1.0 should default to close")
	}
}

func TestParseHTTP10WithKeepAlive(t *testing.T) {
	raw := "GET / HTTP/1.0\r\nHost: x\r\nConnection: keep-alive\r\n\r\n"
	var req Req
	if _, _, err := Parse([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if !req.KeepAlive {
		t.Fatal("Connection: keep-alive should opt in")
	}
}

func TestParseHTTP11ConnectionClose(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	var req Req
	if _, _, err := Parse([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.KeepAlive {
		t.Fatal("Connection: close should opt out")
	}
}

func TestParseSmuggling_CL_and_TE(t *testing.T) {
	raw := "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n"
	var req Req
	_, _, err := Parse([]byte(raw), &req)
	if err != ErrSmuggling {
		t.Fatalf("expected ErrSmuggling, got %v", err)
	}
}

func TestParseSmuggling_DuplicateCL(t *testing.T) {
	raw := "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n"
	var req Req
	_, _, err := Parse([]byte(raw), &req)
	if err != ErrSmuggling {
		t.Fatalf("expected ErrSmuggling, got %v", err)
	}
}

func TestParseObsFold(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: x\r\nX-Custom: line1\r\n line2\r\n\r\n"
	var req Req
	_, _, err := Parse([]byte(raw), &req)
	if err != ErrObsFold {
		t.Fatalf("expected ErrObsFold, got %v", err)
	}
}

func TestParseChunked(t *testing.T) {
	raw := "POST / HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"
	var req Req
	_, _, err := Parse([]byte(raw), &req)
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != -2 {
		t.Fatalf("expected -2 (chunked), got %d", req.ContentLength)
	}
}

func TestParseCaseInsensitiveHeaders(t *testing.T) {
	raw := "POST / HTTP/1.1\r\nHOST: x\r\ncontent-length: 0\r\nconnection: CLOSE\r\n\r\n"
	var req Req
	if _, _, err := Parse([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if string(req.Host) != "x" {
		t.Fatalf("host=%q", req.Host)
	}
	if req.ContentLength != 0 {
		t.Fatalf("cl=%d", req.ContentLength)
	}
	if req.KeepAlive {
		t.Fatal("Connection: CLOSE should opt out")
	}
}

func TestParseBareLF(t *testing.T) {
	raw := "GET / HTTP/1.1\nHost: x\n\n"
	var req Req
	n, ok, err := Parse([]byte(raw), &req)
	if err != nil {
		t.Fatalf("bare LF should be accepted: %v", err)
	}
	if !ok {
		t.Fatal("expected complete")
	}
	if n != len(raw) {
		t.Fatalf("n=%d want=%d", n, len(raw))
	}
}

func TestParseIncomplete(t *testing.T) {
	cases := []string{
		"",
		"G",
		"GET ",
		"GET / HTTP/1.1",
		"GET / HTTP/1.1\r\n",
		"GET / HTTP/1.1\r\nHost: x",
		"GET / HTTP/1.1\r\nHost: x\r\n",
		"GET / HTTP/1.1\r\nHost: x\r\n\r",
	}
	for _, c := range cases {
		var req Req
		_, ok, err := Parse([]byte(c), &req)
		if err != nil {
			t.Errorf("[%q] unexpected err: %v", c, err)
		}
		if ok {
			t.Errorf("[%q] expected incomplete", c)
		}
	}
}

func TestParseRejectInvalid(t *testing.T) {
	cases := []struct {
		raw  string
		want error
	}{
		{"FOOBAR / HTTP/1.1\r\n\r\n", ErrBadMethod},
		{"GET  HTTP/1.1\r\n\r\n", ErrMalformed},
		{"GET / HTTP/X.Y\r\n\r\n", ErrBadVersion},
		{"GET / HTTP/1\r\n\r\n", ErrBadVersion},
		{"GET / HTTP/1.1\r\nBad Header: x\r\n\r\n", ErrMalformed},
		{"POST / HTTP/1.1\r\nContent-Length: abc\r\n\r\n", ErrBadContentLen},
		{"POST / HTTP/1.1\r\nTransfer-Encoding: gzip\r\n\r\n", ErrUnsupportedTE},
	}
	for _, c := range cases {
		var req Req
		_, _, err := Parse([]byte(c.raw), &req)
		if err != c.want {
			t.Errorf("[%q] err=%v want=%v", c.raw, err, c.want)
		}
	}
}

func TestParseTooManyHeaders(t *testing.T) {
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	for i := 0; i < MaxHeaders+1; i++ {
		b.WriteString("X-N: v\r\n")
	}
	b.WriteString("\r\n")
	var req Req
	_, _, err := Parse([]byte(b.String()), &req)
	if err != ErrTooManyHeaders {
		t.Fatalf("err=%v want=%v", err, ErrTooManyHeaders)
	}
}

func TestParseLineTooLong(t *testing.T) {
	raw := "GET /" + strings.Repeat("x", MaxRequestLine+10) + " HTTP/1.1\r\n\r\n"
	var req Req
	_, _, err := Parse([]byte(raw), &req)
	if err != ErrLineTooLong {
		t.Fatalf("err=%v want=%v", err, ErrLineTooLong)
	}
}

func TestParseReuse(t *testing.T) {
	var req Req
	first := "GET /a HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\n"
	if _, _, err := Parse([]byte(first), &req); err != nil {
		t.Fatal(err)
	}
	second := "POST /b HTTP/1.0\r\nHost: b\r\n\r\n"
	if _, _, err := Parse([]byte(second), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != MethodPOST {
		t.Fatalf("method=%v", req.Method)
	}
	if string(req.Path) != "/b" {
		t.Fatalf("path=%q", req.Path)
	}
	if req.ContentLength != -1 {
		t.Fatalf("CL not reset: %d", req.ContentLength)
	}
	if req.KeepAlive {
		t.Fatal("HTTP/1.0 KeepAlive not reset")
	}
}

// FuzzParseDoesNotCrash is the floor: the parser must not panic on any input.
func FuzzParseDoesNotCrash(f *testing.F) {
	seeds := []string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		"",
		"\r\n",
		"GET\r\n\r\n",
		"GET /a?b=c HTTP/1.0\r\n\r\n",
		"POST / HTTP/1.1\r\nContent-Length: 1\r\n\r\nx",
		"POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, buf []byte) {
		var req Req
		Parse(buf, &req)
	})
}

// FuzzParseParityWithStdlib checks that for inputs both parsers accept, the
// basic framing fields agree. Our parser is allowed to be stricter (we reject
// obs-fold and TE!=chunked, stdlib accepts more) — so divergent rejection is
// fine. Divergent ACCEPTANCE is the bug we're hunting.
func FuzzParseParityWithStdlib(f *testing.F) {
	seeds := []string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		"POST /a HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
		"GET /a?b=c HTTP/1.0\r\nHost: x\r\n\r\n",
		"HEAD /x HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, buf []byte) {
		var req Req
		_, ourComplete, ourErr := Parse(buf, &req)

		stdReq, stdErr := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf)))

		if ourErr != nil || !ourComplete || stdErr != nil {
			return
		}
		if req.Method.String() != stdReq.Method {
			t.Errorf("method: ours=%q std=%q buf=%q", req.Method.String(), stdReq.Method, buf)
		}
		// Stdlib uses CL=0 for "no body present" on bodiless methods; we use -1.
		// Treat them as equivalent. Compare only when stdlib reports a real body.
		if cl := stdReq.ContentLength; cl > 0 && req.ContentLength != cl {
			t.Errorf("CL: ours=%d std=%d", req.ContentLength, cl)
		}
		if string(req.Host) != stdReq.Host && stdReq.Host != "" {
			t.Errorf("host: ours=%q std=%q", req.Host, stdReq.Host)
		}
	})
}

func BenchmarkParseSimple(b *testing.B) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: curl/8.5.0\r\nAccept: */*\r\n\r\n")
	var req Req
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Parse(raw, &req)
	}
}

func BenchmarkParseRealistic(b *testing.B) {
	raw := []byte("GET /api/v1/users/12345 HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36\r\n" +
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9\r\n" +
		"Accept-Encoding: gzip, deflate, br\r\n" +
		"Accept-Language: en-US,en;q=0.5\r\n" +
		"Cookie: session=abc123; user_id=42\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n")
	var req Req
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Parse(raw, &req)
	}
}

func BenchmarkParseStdlibSimple(b *testing.B) {
	raw := []byte("GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: curl/8.5.0\r\nAccept: */*\r\n\r\n")
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	}
}

func BenchmarkParseStdlibRealistic(b *testing.B) {
	raw := []byte("GET /api/v1/users/12345 HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36\r\n" +
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9\r\n" +
		"Accept-Encoding: gzip, deflate, br\r\n" +
		"Accept-Language: en-US,en;q=0.5\r\n" +
		"Cookie: session=abc123; user_id=42\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n")
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	}
}

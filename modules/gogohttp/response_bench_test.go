package gogohttp

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// nopConn is a net.Conn that discards writes — isolates server-side response
// cost from kernel/network/client overhead.
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)         { return 0, nil }
func (nopConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) LocalAddr() net.Addr              { return nil }
func (nopConn) RemoteAddr() net.Addr             { return nil }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

func BenchmarkResponseWriteTo(b *testing.B) {
	body := []byte("<html>hello</html>")
	registry := [][]byte{
		nil,
		[]byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: text/html\r\n"),
	}
	var resp Resp
	resp.registry = registry
	c := nopConn{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp.reset()
		resp.HeadID = 1
		resp.Body = body
		resp.writeTo(c, false, false)
	}
}

func BenchmarkResponseWriteToHEAD(b *testing.B) {
	body := []byte("<html>hello</html>")
	registry := [][]byte{
		nil,
		[]byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: text/html\r\n"),
	}
	var resp Resp
	resp.registry = registry
	c := nopConn{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp.reset()
		resp.HeadID = 1
		resp.Body = body
		resp.writeTo(c, true, false)
	}
}

func BenchmarkResponseWriteToWithRange(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 1024)
	registry := [][]byte{
		nil,
		[]byte("HTTP/1.1 206 Partial Content\r\nServer: Gogogo\r\nContent-Type: video/mp4\r\nAccept-Ranges: bytes\r\n"),
	}
	var resp Resp
	resp.registry = registry
	c := nopConn{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp.reset()
		resp.HeadID = 1
		resp.Body = body[100:200]
		resp.SetContentRange(100, 199, int64(len(body)))
		resp.writeTo(c, false, false)
	}
}

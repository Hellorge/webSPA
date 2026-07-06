package gogohttp

import (
	"errors"
	"io"
	"net"
	"strconv"
)

// Resp is the response handle. Three handler-visible body modes, picked by
// which field the handler sets. Mutually exclusive — server picks the first
// non-nil in this order: Hijack, Stream, Body.
//
//   Buffered (Body): the default. Server writes the head with Content-Length
//   followed by Body in a single writev. Use for static content, JSON, images,
//   any response whose length is known when the handler returns.
//
//   Streaming (Stream): server writes the head with Transfer-Encoding: chunked,
//   then calls the handler's closure with an io.Writer. Each Write produces
//   one chunk. Server emits the trailer when the closure returns. Use for
//   SSE, live video, progressive HTML, large file downloads.
//
//   Hijack (Hijack): server writes the head only, then hands the raw conn to
//   the handler. Handler owns the conn until the closure returns. Use for
//   WebSockets, long-polling, custom protocols.
//
// In all three modes the head is the pre-baked entry from Server.HeaderRegistry
// at index HeadID. The head must NOT include Content-Length, Transfer-Encoding,
// or the empty terminator line — the server appends those based on mode.
type Resp struct {
	HeadID       uint16
	ExtraHeaders []byte                    // optional pre-formatted "Name: value\r\n..."
	Body         []byte                    // buffered mode
	Stream       func(w io.Writer) error   // streaming mode
	Hijack       func(c net.Conn) error    // hijack mode

	// internal — set by the Server when the connState is acquired
	registry [][]byte

	// scratch buffers, kept on the Resp so reuse across requests is alloc-free
	clBuf    [24]byte
	extraBuf [128]byte
}

func (r *Resp) reset() {
	r.HeadID = 0
	r.ExtraHeaders = nil
	r.Body = nil
	r.Stream = nil
	r.Hijack = nil
}

// SetContentRange formats "Content-Range: bytes start-end/total\r\n" into the
// internal scratch buffer and stores the slice as ExtraHeaders. Used by
// range-request handlers (video seeks, resumed downloads). Zero allocations.
func (r *Resp) SetContentRange(start, end, total int64) {
	s := r.extraBuf[:0]
	s = append(s, "Content-Range: bytes "...)
	s = strconv.AppendInt(s, start, 10)
	s = append(s, '-')
	s = strconv.AppendInt(s, end, 10)
	s = append(s, '/')
	s = strconv.AppendInt(s, total, 10)
	s = append(s, '\r', '\n')
	r.ExtraHeaders = s
}

var errInvalidHeadID = errors.New("gogohttp: invalid HeadID — Server.HeaderRegistry has no entry")

// writeTo dispatches to the body-mode-specific writer. The mode is fixed for
// the duration of the request — the switch runs once and the inner method is
// straight-line writev.
func (r *Resp) writeTo(c net.Conn, isHead, closeConn bool) error {
	if int(r.HeadID) >= len(r.registry) {
		return errInvalidHeadID
	}
	head := r.registry[r.HeadID]
	if head == nil {
		return errInvalidHeadID
	}
	date := CurrentDate()

	var closeSeg []byte
	if closeConn {
		closeSeg = connectionCloseLine
	}

	switch {
	case r.Hijack != nil:
		return r.writeHijack(c, head, date, closeSeg)
	case r.Stream != nil:
		if isHead {
			return r.writeStreamHEAD(c, head, date, closeSeg)
		}
		return r.writeStream(c, head, date, closeSeg)
	default:
		return r.writeBuffered(c, head, date, closeSeg, isHead)
	}
}

func (r *Resp) writeBuffered(c net.Conn, head, date, closeSeg []byte, isHead bool) error {
	cl := r.clBuf[:0]
	cl = append(cl, "Content-Length: "...)
	cl = strconv.AppendInt(cl, int64(len(r.Body)), 10)
	cl = append(cl, '\r', '\n')

	var body []byte
	if !isHead {
		body = r.Body
	}

	bufs := net.Buffers{head, date, cl, closeSeg, r.ExtraHeaders, crlf, body}
	_, err := bufs.WriteTo(c)
	return err
}

func (r *Resp) writeStream(c net.Conn, head, date, closeSeg []byte) error {
	bufs := net.Buffers{head, date, transferEncodingChunked, closeSeg, r.ExtraHeaders, crlf}
	if _, err := bufs.WriteTo(c); err != nil {
		return err
	}

	cw := chunkedWriter{conn: c}
	if err := r.Stream(&cw); err != nil {
		return err
	}
	return cw.writeTrailer()
}

func (r *Resp) writeStreamHEAD(c net.Conn, head, date, closeSeg []byte) error {
	// HEAD on a streaming endpoint: emit headers but no body framing. Stream
	// is intentionally not called — RFC 7231 §4.3.2 says HEAD must not return
	// a body, so framing headers (Transfer-Encoding) would mislead clients.
	bufs := net.Buffers{head, date, closeSeg, r.ExtraHeaders, crlf}
	_, err := bufs.WriteTo(c)
	return err
}

func (r *Resp) writeHijack(c net.Conn, head, date, closeSeg []byte) error {
	bufs := net.Buffers{head, date, closeSeg, r.ExtraHeaders, crlf}
	if _, err := bufs.WriteTo(c); err != nil {
		return err
	}
	return r.Hijack(c)
}

// chunkedWriter wraps a net.Conn to emit HTTP/1.1 chunked transfer encoding.
// Each Write becomes one chunk: "<hex-len>\r\n<bytes>\r\n". Empty Writes are
// silently skipped because a zero-length chunk is the stream terminator.
//
// Errors are sticky — once a write fails, subsequent writes return the same
// error without touching the conn, so handlers can naively loop without
// per-iteration error checks.
//
// chunkedWriter does not buffer. Each Write triggers one writev syscall.
// For real-time semantics (SSE) this is what you want. For large transfers
// callers should pass io.Copy(w, src) which uses 32 KiB chunks, or wrap
// with bufio.Writer for explicit batching.
type chunkedWriter struct {
	conn net.Conn
	err  error
}

func (w *chunkedWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(b) == 0 {
		return 0, nil
	}
	var hdr [20]byte
	h := strconv.AppendUint(hdr[:0], uint64(len(b)), 16)
	h = append(h, '\r', '\n')
	bufs := net.Buffers{h, b, crlf}
	if _, err := bufs.WriteTo(w.conn); err != nil {
		w.err = err
		return 0, err
	}
	return len(b), nil
}

func (w *chunkedWriter) writeTrailer() error {
	if w.err != nil {
		return w.err
	}
	_, err := w.conn.Write(chunkedTrailer)
	return err
}

var (
	connectionCloseLine     = []byte("Connection: close\r\n")
	transferEncodingChunked = []byte("Transfer-Encoding: chunked\r\n")
	chunkedTrailer          = []byte("0\r\n\r\n")
	crlf                    = []byte("\r\n")
)

// Pre-baked complete responses for protocol errors detected before we have a
// working Resp/Handler. These don't go through writeTo — they are written
// directly when the parser rejects a request, so the server can respond and
// close even if no HeaderRegistry was configured.
var (
	errResp400 = []byte("HTTP/1.1 400 Bad Request\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	errResp408 = []byte("HTTP/1.1 408 Request Timeout\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	errResp413 = []byte("HTTP/1.1 413 Payload Too Large\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	errResp414 = []byte("HTTP/1.1 414 URI Too Long\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	errResp431 = []byte("HTTP/1.1 431 Request Header Fields Too Large\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	errResp500 = []byte("HTTP/1.1 500 Internal Server Error\r\nServer: Gogogo\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
)

// Quiet "io" import — we expose io.Writer as part of the Resp.Stream signature.
var _ = io.Discard

func errResponseFor(err error) []byte {
	switch err {
	case ErrLineTooLong:
		return errResp414
	case ErrHeadersTooLong, ErrTooManyHeaders:
		return errResp431
	case errBodyTooLarge:
		return errResp413
	default:
		return errResp400
	}
}

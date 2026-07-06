package gogohttp

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Handler is the narrowed contract that replaces net/http's. It receives a
// pointer-into-buffer Req and a Resp the server pre-installed. There is no
// http.ResponseWriter shape to satisfy and no Header() map to allocate.
type Handler interface {
	Serve(req *Req, resp *Resp)
}

type HandlerFunc func(req *Req, resp *Resp)

func (f HandlerFunc) Serve(req *Req, resp *Resp) { f(req, resp) }

// Server runs the HTTP/1.x request loop on a net.Listener. In gogogo, each
// SO_REUSEPORT bouncer constructs its own Server (or shares one and calls
// Serve on its own listener) — the kernel load-balances accept across cores
// and conns stay on their origin core through to response.
type Server struct {
	Handler Handler

	// HeaderRegistry is indexed by Resp.HeadID. Entry 0 is reserved (a HeadID
	// of 0 means "use the dynamic path"). Each entry is the bytes from the
	// status line through the empty line that ends the head, including
	// Content-Length and any Content-Encoding — Date is added at write time.
	HeaderRegistry [][]byte

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// MaxBodyBytes caps Content-Length we will read into memory. 0 = no body.
	// Default is 1 MiB if zero is interpreted as "unset" — see Serve.
	MaxBodyBytes int64

	// MaxReadBuf bounds the read buffer size used per connection (heads + body
	// of one request). Default 64 KiB.
	MaxReadBuf int

	closed atomic.Bool
	wg     sync.WaitGroup

	// activeRegistry holds the registry currently in use by handlers. nil
	// means "use HeaderRegistry verbatim." Set via ReplaceRegistry for
	// hot-swap. Read once per request via loadRegistry — branch-predictor
	// friendly, single atomic load.
	activeRegistry atomic.Pointer[[][]byte]
}

// loadRegistry returns the active header registry. Hot path — one atomic
// load per request, returned by reference (no copy of the underlying slice).
func (s *Server) loadRegistry() [][]byte {
	if r := s.activeRegistry.Load(); r != nil {
		return *r
	}
	return s.HeaderRegistry
}

// ReplaceRegistry atomically swaps in a new HeaderRegistry. New requests
// (and new handler dispatches on existing keep-alive conns) see it
// immediately; in-flight requests finish on whatever they captured.
//
// The previous registry remains valid as long as anything references it —
// the application is responsible for keeping the previous one alive long
// enough that no in-flight request is still using it. (For the common case
// of immutable registry slices, this is automatic.)
func (s *Server) ReplaceRegistry(newReg [][]byte) {
	s.activeRegistry.Store(&newReg)
}

// connState is per-connection working memory; pooled so each conn lifetime
// allocates nothing besides the conn itself and a single sync.Pool hit.
type connState struct {
	req  Req
	resp Resp
	rbuf []byte
	rend int
}

const initialReadBuf = 4096

var connPool = sync.Pool{
	New: func() any {
		return &connState{
			rbuf: make([]byte, initialReadBuf),
		}
	},
}

var errBodyTooLarge = errors.New("gogohttp: body exceeds MaxBodyBytes")

// Serve runs the accept loop on ln. Returns when ln is closed (or hits a
// permanent error). Always call Shutdown to drain in-flight conns gracefully.
func (s *Server) Serve(ln net.Listener) error {
	if s.Handler == nil {
		return errors.New("gogohttp: nil Handler")
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 1 << 20
	}
	if s.MaxReadBuf == 0 {
		s.MaxReadBuf = 64 * 1024
	}

	for {
		c, err := ln.Accept()
		if err != nil {
			if s.closed.Load() {
				s.wg.Wait()
				return nil
			}
			// Transient accept errors (e.g., EMFILE) — back off briefly. We
			// reuse the same path for permanent errors; the listener returns
			// the same error every call until it succeeds, so this is a
			// cheap defense against tight error loops.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

// Shutdown closes future accepts and waits for in-flight conns to drain.
// Callers should close the listener first; Shutdown then waits.
func (s *Server) Shutdown() {
	s.closed.Store(true)
	s.wg.Wait()
}

func (s *Server) serveConn(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()

	cs := connPool.Get().(*connState)
	defer func() {
		// Release pool entry. Reset is cheap and keeps allocations near-zero
		// over the lifetime of the pool.
		cs.req.Reset()
		cs.resp.reset()
		cs.resp.registry = nil
		cs.rend = 0
		connPool.Put(cs)
	}()

	defer func() {
		if rec := recover(); rec != nil {
			// Handler panicked. We can't trust the Resp state, so write a
			// pre-baked 500 directly and close.
			c.Write(errResp500)
		}
	}()

	for {
		keepGoing := s.serveOne(c, cs)
		if !keepGoing {
			return
		}
	}
}

// serveOne reads, parses, dispatches, and writes one request. Returns true
// to continue the keep-alive loop, false to close the connection.
func (s *Server) serveOne(c net.Conn, cs *connState) bool {
	// Apply read deadline. On the first byte of a connection (or after a
	// keep-alive idle period), use IdleTimeout so a quiet client doesn't
	// look like a slowloris. Once we've started reading, switch to
	// ReadHeaderTimeout (or ReadTimeout) so an attacker can't drip headers.
	if cs.rend == 0 && s.IdleTimeout > 0 {
		c.SetReadDeadline(time.Now().Add(s.IdleTimeout))
	} else if to := s.headerTimeout(); to > 0 {
		c.SetReadDeadline(time.Now().Add(to))
	}

	bodyOffset := 0
	headerStarted := cs.rend > 0
	for {
		n, complete, err := Parse(cs.rbuf[:cs.rend], &cs.req)
		if err != nil {
			c.Write(errResponseFor(err))
			return false
		}
		if complete {
			bodyOffset = n
			break
		}

		// Switch to header timeout once we've read any data.
		if !headerStarted && s.headerTimeout() > 0 {
			c.SetReadDeadline(time.Now().Add(s.headerTimeout()))
		}

		if cs.rend == len(cs.rbuf) {
			if len(cs.rbuf) >= s.MaxReadBuf {
				c.Write(errResp431)
				return false
			}
			size := len(cs.rbuf) * 2
			if size > s.MaxReadBuf {
				size = s.MaxReadBuf
			}
			grown := make([]byte, size)
			copy(grown, cs.rbuf[:cs.rend])
			cs.rbuf = grown
		}

		nr, rerr := c.Read(cs.rbuf[cs.rend:])
		if nr > 0 {
			headerStarted = true
		}
		cs.rend += nr
		if rerr != nil {
			// Idle EOF or timeout: silent close.
			return false
		}
	}

	// Body framing. Phase 2 supports plain Content-Length only — gogogo's
	// dynamic endpoints (studio SQL, meta updates) all use small bodies.
	bodyLen := int64(0)
	if cs.req.ContentLength > 0 {
		bodyLen = cs.req.ContentLength
	} else if cs.req.ContentLength == -2 {
		// chunked: not yet supported in Phase 2
		c.Write(errResp400)
		return false
	}

	if bodyLen > s.MaxBodyBytes {
		c.Write(errResp413)
		return false
	}

	if bodyLen > 0 {
		// Switch to body read timeout.
		if s.ReadTimeout > 0 {
			c.SetReadDeadline(time.Now().Add(s.ReadTimeout))
		}
		needed := bodyOffset + int(bodyLen)
		for cs.rend < needed {
			if needed > len(cs.rbuf) {
				if needed > s.MaxReadBuf {
					c.Write(errResp413)
					return false
				}
				size := len(cs.rbuf) * 2
				if size < needed {
					size = needed
				}
				grown := make([]byte, size)
				copy(grown, cs.rbuf[:cs.rend])
				cs.rbuf = grown
			}
			nr, rerr := c.Read(cs.rbuf[cs.rend:])
			cs.rend += nr
			if rerr != nil && cs.rend < needed {
				return false
			}
		}
		cs.req.Body = cs.rbuf[bodyOffset:needed]
	}

	// Dispatch. Reload the registry per request so hot-swaps via
	// ReplaceRegistry take effect on the very next request, including on
	// already-established keep-alive conns.
	cs.resp.reset()
	cs.resp.registry = s.loadRegistry()
	s.Handler.Serve(&cs.req, &cs.resp)

	// Write. The server picks the response mode by which Resp field is set
	// (Hijack > Stream > Body). For Stream/Hijack we clear write deadlines
	// because those modes can be long-lived; the handler manages timeouts
	// internally if it wants them.
	isHead := cs.req.Method == MethodHEAD
	closeConn := !cs.req.KeepAlive
	hijack := cs.resp.Hijack != nil
	stream := cs.resp.Stream != nil

	switch {
	case hijack:
		c.SetDeadline(time.Time{})
	case stream:
		c.SetWriteDeadline(time.Time{})
	default:
		if s.WriteTimeout > 0 {
			c.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
		}
	}

	if err := cs.resp.writeTo(c, isHead, closeConn); err != nil {
		return false
	}

	// Hijack transferred conn ownership to the handler. Handler's closure
	// has returned, so we close the conn and stop the keep-alive loop.
	if hijack {
		return false
	}

	// Carry over any pipelined bytes (request beyond bodyOffset+bodyLen).
	consumed := bodyOffset + int(bodyLen)
	if consumed < cs.rend {
		copy(cs.rbuf, cs.rbuf[consumed:cs.rend])
		cs.rend -= consumed
	} else {
		cs.rend = 0
	}

	return !closeConn
}

func (s *Server) headerTimeout() time.Duration {
	if s.ReadHeaderTimeout > 0 {
		return s.ReadHeaderTimeout
	}
	return s.ReadTimeout
}


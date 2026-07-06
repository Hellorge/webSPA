package gogohttp

import "errors"

const (
	MaxHeaders     = 32
	MaxRequestLine = 8 * 1024
	MaxHeaderBytes = 64 * 1024
)

type Method uint8

const (
	MethodUnknown Method = iota
	MethodGET
	MethodHEAD
	MethodPOST
	MethodPUT
	MethodDELETE
	MethodOPTIONS
	MethodPATCH
	MethodCONNECT
	MethodTRACE
)

func (m Method) String() string {
	switch m {
	case MethodGET:
		return "GET"
	case MethodHEAD:
		return "HEAD"
	case MethodPOST:
		return "POST"
	case MethodPUT:
		return "PUT"
	case MethodDELETE:
		return "DELETE"
	case MethodOPTIONS:
		return "OPTIONS"
	case MethodPATCH:
		return "PATCH"
	case MethodCONNECT:
		return "CONNECT"
	case MethodTRACE:
		return "TRACE"
	}
	return ""
}

type Header struct {
	Name  []byte
	Value []byte
}

// Req holds the parsed view of one HTTP/1.x request. All []byte fields are
// slices into the source buffer the caller passed to Parse — the buffer must
// remain valid for the lifetime of the Req.
type Req struct {
	Method        Method
	Path          []byte
	Query         []byte
	Major, Minor  uint8
	Headers       [MaxHeaders]Header
	NHeaders      int
	ContentLength int64 // -1 = none, -2 = chunked, else explicit length
	KeepAlive     bool
	Host          []byte
	Body          []byte // populated by the server after Parse returns
}

// QueryValue returns the first value associated with key in the query string,
// or "" if absent. Splits on '&' then '=' — does not URL-decode. Adequate
// for simple admin endpoints; callers needing decoding should use net/url.
func (r *Req) QueryValue(key string) string {
	keyBytes := []byte(key)
	q := r.Query
	for len(q) > 0 {
		var pair []byte
		if amp := indexByte(q, '&'); amp >= 0 {
			pair = q[:amp]
			q = q[amp+1:]
		} else {
			pair = q
			q = nil
		}
		if eq := indexByte(pair, '='); eq >= 0 {
			if eqBytes(pair[:eq], keyBytes) {
				return string(pair[eq+1:])
			}
		}
	}
	return ""
}

// Local helpers — defined here to avoid pulling in `bytes` for two trivial
// operations on the hot Req surface.
func indexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func eqBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *Req) Reset() {
	r.Method = MethodUnknown
	r.Path = nil
	r.Query = nil
	r.Major = 0
	r.Minor = 0
	r.NHeaders = 0
	r.ContentLength = -1
	r.KeepAlive = false
	r.Host = nil
	r.Body = nil
}

var (
	ErrMalformed      = errors.New("gogohttp: malformed request")
	ErrTooManyHeaders = errors.New("gogohttp: too many headers")
	ErrLineTooLong    = errors.New("gogohttp: line too long")
	ErrHeadersTooLong = errors.New("gogohttp: headers too long")
	ErrSmuggling      = errors.New("gogohttp: ambiguous request framing")
	ErrBadMethod      = errors.New("gogohttp: unknown method")
	ErrBadVersion     = errors.New("gogohttp: bad HTTP version")
	ErrObsFold        = errors.New("gogohttp: obsolete line folding")
	ErrBadContentLen  = errors.New("gogohttp: bad Content-Length")
	ErrUnsupportedTE  = errors.New("gogohttp: unsupported Transfer-Encoding")
)

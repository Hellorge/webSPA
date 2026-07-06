package gogohttp

import "bytes"

// Parse decodes one HTTP/1.x request header section from buf into req.
//
// Returns:
//   n        — number of bytes consumed (offset of the body start in buf)
//   complete — true if a full header section ending in CRLFCRLF was found
//   err      — parse / framing / security error
//
// If complete=false and err=nil, buf is incomplete; the caller should read
// more bytes and call Parse again with the extended buffer.
//
// On any error, the request must be rejected and the connection closed —
// Parse rejects HTTP smuggling vectors (CL+TE, duplicate CL) and obs-fold
// continuations, both of which are deprecated and security-relevant.
func Parse(buf []byte, req *Req) (n int, complete bool, err error) {
	req.Reset()

	eol, skip := findEOL(buf)
	if eol < 0 {
		if len(buf) > MaxRequestLine {
			return 0, false, ErrLineTooLong
		}
		return 0, false, nil
	}
	if eol > MaxRequestLine {
		return 0, false, ErrLineTooLong
	}
	if err := parseRequestLine(buf[:eol], req); err != nil {
		return 0, true, err
	}
	// HTTP/1.1 keep-alive is on by default; HTTP/1.0 is off. A Connection
	// header below may flip this.
	req.KeepAlive = req.Major == 1 && req.Minor >= 1

	pos := eol + skip
	headerStart := pos

	var hasCL, hasTE bool
	for {
		if pos-headerStart > MaxHeaderBytes {
			return 0, true, ErrHeadersTooLong
		}
		if pos >= len(buf) {
			return 0, false, nil
		}

		// End of headers: an empty line.
		if buf[pos] == '\r' {
			if pos+1 >= len(buf) {
				return 0, false, nil
			}
			if buf[pos+1] == '\n' {
				if hasCL && hasTE {
					return 0, true, ErrSmuggling
				}
				return pos + 2, true, nil
			}
			return 0, true, ErrMalformed
		}
		if buf[pos] == '\n' {
			if hasCL && hasTE {
				return 0, true, ErrSmuggling
			}
			return pos + 1, true, nil
		}

		// Reject obs-fold (line continuation starting with WS) — RFC 7230
		// deprecates this, and accepting it is a known smuggling vector.
		if buf[pos] == ' ' || buf[pos] == '\t' {
			return 0, true, ErrObsFold
		}

		eolH, skipH := findEOL(buf[pos:])
		if eolH < 0 {
			return 0, false, nil
		}
		if req.NHeaders >= MaxHeaders {
			return 0, true, ErrTooManyHeaders
		}
		if err := parseHeader(buf[pos:pos+eolH], req, &hasCL, &hasTE); err != nil {
			return 0, true, err
		}
		pos += eolH + skipH
	}
}

// findEOL locates the next line terminator. CRLF is canonical; bare LF is
// accepted for client lenience. Bare CR is treated as a regular byte.
// Returns (eolPos, skip) where eolPos points at the terminator's first byte
// and skip is how many bytes to advance past it (2 for CRLF, 1 for LF).
// Returns (-1, 0) if no terminator is present in b.
func findEOL(b []byte) (int, int) {
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			if i > 0 && b[i-1] == '\r' {
				return i - 1, 2
			}
			return i, 1
		}
	}
	return -1, 0
}

func parseRequestLine(line []byte, req *Req) error {
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return ErrMalformed
	}
	rest := line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return ErrMalformed
	}

	method := line[:sp1]
	target := rest[:sp2]
	version := rest[sp2+1:]

	req.Method = methodFromBytes(method)
	if req.Method == MethodUnknown {
		return ErrBadMethod
	}

	if len(target) == 0 {
		return ErrMalformed
	}
	if q := bytes.IndexByte(target, '?'); q >= 0 {
		req.Path = target[:q]
		req.Query = target[q+1:]
	} else {
		req.Path = target
	}

	if len(version) != 8 ||
		version[0] != 'H' || version[1] != 'T' || version[2] != 'T' || version[3] != 'P' ||
		version[4] != '/' || version[6] != '.' {
		return ErrBadVersion
	}
	if version[5] < '0' || version[5] > '9' || version[7] < '0' || version[7] > '9' {
		return ErrBadVersion
	}
	req.Major = version[5] - '0'
	req.Minor = version[7] - '0'

	return nil
}

func parseHeader(line []byte, req *Req, hasCL, hasTE *bool) error {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return ErrMalformed
	}
	name := line[:colon]
	for i := 0; i < len(name); i++ {
		if !isTokenChar[name[i]] {
			return ErrMalformed
		}
	}

	value := line[colon+1:]
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}

	// Length-bucketed dispatch for the framing-relevant headers. ASCII
	// case-fold compare against canonical names. All other headers are
	// stored verbatim and inspected (if at all) by the handler.
	switch len(name) {
	case 4:
		if eqFoldASCII(name, hostBytes) {
			req.Host = value
		}
	case 10:
		if eqFoldASCII(name, connectionBytes) {
			if eqFoldASCII(value, closeBytes) {
				req.KeepAlive = false
			} else if eqFoldASCII(value, keepAliveBytes) {
				req.KeepAlive = true
			}
		}
	case 14:
		if eqFoldASCII(name, contentLengthBytes) {
			if *hasCL {
				return ErrSmuggling
			}
			*hasCL = true
			n, ok := parseDecimal(value)
			if !ok {
				return ErrBadContentLen
			}
			req.ContentLength = n
		}
	case 17:
		if eqFoldASCII(name, transferEncodingBytes) {
			*hasTE = true
			if !eqFoldASCII(value, chunkedBytes) {
				return ErrUnsupportedTE
			}
			req.ContentLength = -2
		}
	}

	req.Headers[req.NHeaders].Name = name
	req.Headers[req.NHeaders].Value = value
	req.NHeaders++

	return nil
}

func methodFromBytes(b []byte) Method {
	switch len(b) {
	case 3:
		if string(b) == "GET" {
			return MethodGET
		}
		if string(b) == "PUT" {
			return MethodPUT
		}
	case 4:
		if string(b) == "POST" {
			return MethodPOST
		}
		if string(b) == "HEAD" {
			return MethodHEAD
		}
	case 5:
		if string(b) == "PATCH" {
			return MethodPATCH
		}
		if string(b) == "TRACE" {
			return MethodTRACE
		}
	case 6:
		if string(b) == "DELETE" {
			return MethodDELETE
		}
	case 7:
		if string(b) == "OPTIONS" {
			return MethodOPTIONS
		}
		if string(b) == "CONNECT" {
			return MethodCONNECT
		}
	}
	return MethodUnknown
}

// eqFoldASCII compares two byte slices case-insensitively under ASCII rules.
// HTTP header names and the framing-header values we examine here are all
// ASCII-only per spec, so we don't need Unicode folding.
func eqFoldASCII(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		cb := b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca |= 0x20
		}
		if cb >= 'A' && cb <= 'Z' {
			cb |= 0x20
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// parseDecimal parses a non-negative ASCII decimal integer with overflow
// guard. Empty input is invalid.
func parseDecimal(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if n > (1<<62-1-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

var (
	hostBytes             = []byte("Host")
	connectionBytes       = []byte("Connection")
	contentLengthBytes    = []byte("Content-Length")
	transferEncodingBytes = []byte("Transfer-Encoding")
	closeBytes            = []byte("close")
	keepAliveBytes        = []byte("keep-alive")
	chunkedBytes          = []byte("chunked")
)

var isTokenChar [256]bool

func init() {
	for c := 0; c < 256; c++ {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
			isTokenChar[c] = true
		}
		switch byte(c) {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-',
			'.', '^', '_', '`', '|', '~':
			isTokenChar[c] = true
		}
	}
}

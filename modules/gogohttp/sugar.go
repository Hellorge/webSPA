package gogohttp

// Built-in HeadIDs in DefaultRegistry. These cover only HTTP status codes —
// a fixed set defined by the HTTP spec, never grows with content. Anything
// content-type-specific (200 OK + text/html, 200 OK + image/png, etc.) is
// data-driven from the manifest at server startup via RegistryBuilder, so
// new file types don't require code edits.
//
// Applications append their own entries at HeadIDUserStart or above.
const (
	_ uint16 = iota // 0 reserved (sentinel for "unset")

	HeadID101_WSUpgrade
	HeadID204
	HeadID301
	HeadID302
	HeadID304
	HeadID400
	HeadID401
	HeadID403
	HeadID404
	HeadID405
	HeadID500
	HeadID503

	HeadIDUserStart uint16 = 32
)

// DefaultRegistry returns a HeaderRegistry pre-populated with status-only
// heads. Each entry is the status line plus baseline headers (Server, and
// Content-Type for entries that pair with a textual body), terminated with
// the last header's CRLF only — no empty terminator line, no Content-Length.
//
// Applications wanting per-mime "200 OK" heads should pipe their content
// through a RegistryBuilder seeded with this default.
func DefaultRegistry() [][]byte {
	r := make([][]byte, HeadIDUserStart)
	r[HeadID101_WSUpgrade] = []byte("HTTP/1.1 101 Switching Protocols\r\nServer: Gogogo\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n")
	r[HeadID204] = []byte("HTTP/1.1 204 No Content\r\nServer: Gogogo\r\n")
	r[HeadID301] = []byte("HTTP/1.1 301 Moved Permanently\r\nServer: Gogogo\r\n")
	r[HeadID302] = []byte("HTTP/1.1 302 Found\r\nServer: Gogogo\r\n")
	r[HeadID304] = []byte("HTTP/1.1 304 Not Modified\r\nServer: Gogogo\r\n")
	r[HeadID400] = []byte("HTTP/1.1 400 Bad Request\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID401] = []byte("HTTP/1.1 401 Unauthorized\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID403] = []byte("HTTP/1.1 403 Forbidden\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID404] = []byte("HTTP/1.1 404 Not Found\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID405] = []byte("HTTP/1.1 405 Method Not Allowed\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID500] = []byte("HTTP/1.1 500 Internal Server Error\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	r[HeadID503] = []byte("HTTP/1.1 503 Service Unavailable\r\nServer: Gogogo\r\nContent-Type: text/plain; charset=utf-8\r\n")
	return r
}

// RegistryBuilder accumulates entries into a HeaderRegistry. Use it to merge
// the default status heads with application-specific per-mime heads (typically
// derived from a build manifest). Call Build to get the final [][]byte to
// pass to Server.HeaderRegistry.
type RegistryBuilder struct {
	entries [][]byte
	mimeIDs map[string]uint16
}

func NewRegistryBuilder() *RegistryBuilder {
	return &RegistryBuilder{
		entries: DefaultRegistry(),
		mimeIDs: make(map[string]uint16),
	}
}

// AddMime adds (or returns an existing HeadID for) a "200 OK + Content-Type"
// head with no Content-Encoding (identity variant). Idempotent.
func (rb *RegistryBuilder) AddMime(mime string) uint16 {
	return rb.AddMimeWithEncoding(mime, "")
}

// AddMimeWithEncoding adds (or returns an existing HeadID for) a "200 OK +
// Content-Type [+ Content-Encoding]" head. Empty encoding means identity
// (no Content-Encoding line). Idempotent — calling with the same pair
// returns the same HeadID.
func (rb *RegistryBuilder) AddMimeWithEncoding(mime, encoding string) uint16 {
	key := mime
	if encoding != "" {
		key = mime + "|" + encoding
	}
	if id, ok := rb.mimeIDs[key]; ok {
		return id
	}
	var head []byte
	if encoding == "" {
		head = []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: " + mime + "\r\n")
	} else {
		head = []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: " + mime + "\r\nContent-Encoding: " + encoding + "\r\n")
	}
	id := uint16(len(rb.entries))
	rb.entries = append(rb.entries, head)
	rb.mimeIDs[key] = id
	return id
}

// Add registers an arbitrary pre-formatted head and returns its HeadID. Use
// for non-200 statuses or heads that need extra static headers (CSP, CORS,
// caching, etc.).
func (rb *RegistryBuilder) Add(head []byte) uint16 {
	id := uint16(len(rb.entries))
	rb.entries = append(rb.entries, head)
	return id
}

// MimeID returns the HeadID for a previously-registered mime, or 0 if absent.
func (rb *RegistryBuilder) MimeID(mime string) uint16 {
	return rb.mimeIDs[mime]
}

// Build returns the final registry. The builder is safe to discard after.
func (rb *RegistryBuilder) Build() [][]byte {
	return rb.entries
}

// Status sugar — these reference HeadIDs in DefaultRegistry that every app
// has. Per-mime sugar (HTML, JSON, etc.) is intentionally absent: those
// HeadIDs vary by application setup, so the handler picks them directly.

func (r *Resp) NotFound()         { r.HeadID = HeadID404 }
func (r *Resp) NoContent()        { r.HeadID = HeadID204 }
func (r *Resp) ServerError()      { r.HeadID = HeadID500 }
func (r *Resp) BadRequest()       { r.HeadID = HeadID400 }
func (r *Resp) Forbidden()        { r.HeadID = HeadID403 }
func (r *Resp) Unauthorized()     { r.HeadID = HeadID401 }
func (r *Resp) MethodNotAllowed() { r.HeadID = HeadID405 }

// Redirect sets a 301 (permanent=true) or 302 (permanent=false) redirect
// with the given Location URL. Uses the internal scratch buffer; no alloc.
func (r *Resp) Redirect(url string, permanent bool) {
	if permanent {
		r.HeadID = HeadID301
	} else {
		r.HeadID = HeadID302
	}
	s := r.extraBuf[:0]
	s = append(s, "Location: "...)
	s = append(s, url...)
	s = append(s, '\r', '\n')
	r.ExtraHeaders = s
}

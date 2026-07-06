package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"

	"gogogo/modules/gogohttp"
	"gogogo/modules/router"
)

// fileHeadIDs holds the registry HeadIDs for an ActionFile mime type:
// one for full responses (200 OK) and one for partial (206 Partial Content).
// Both bake "Accept-Ranges: bytes" so range-aware clients (browsers
// fetching media) know they can ask for byte ranges.
type fileHeadIDs struct {
	head200 uint16
	head206 uint16
}

// readFileBlob extracts the disk path from an ActionFile route's blob.
// Format: [pathLen u16][path bytes].
func readFileBlob(blob []byte) (string, error) {
	if len(blob) < 2 {
		return "", fmt.Errorf("file blob too short")
	}
	pathLen := int(binary.LittleEndian.Uint16(blob[:2]))
	if 2+pathLen > len(blob) {
		return "", fmt.Errorf("file blob path overflows blob")
	}
	return string(blob[2 : 2+pathLen]), nil
}

// registerFileMime adds 200 OK + 206 Partial Content heads for a mime type
// to the registry, deduped per appState. Idempotent; second call returns
// the cached IDs.
func (st *appState) registerFileMime(mimeStr string, rb *gogohttp.RegistryBuilder) fileHeadIDs {
	if h, ok := st.fileHeads[mimeStr]; ok {
		return h
	}
	head200 := []byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nContent-Type: " + mimeStr + "\r\nAccept-Ranges: bytes\r\n")
	head206 := []byte("HTTP/1.1 206 Partial Content\r\nServer: Gogogo\r\nContent-Type: " + mimeStr + "\r\nAccept-Ranges: bytes\r\n")
	h := fileHeadIDs{
		head200: rb.Add(head200),
		head206: rb.Add(head206),
	}
	if st.fileHeads == nil {
		st.fileHeads = map[string]fileHeadIDs{}
	}
	st.fileHeads[mimeStr] = h
	return h
}

// mimeForFile resolves a disk path's mime type. Falls back to
// application/octet-stream so the route always has a valid Content-Type.
func mimeForFile(path string) string {
	m := mime.TypeByExtension(filepath.Ext(path))
	if m == "" {
		return "application/octet-stream"
	}
	return m
}

// serveFile is the ActionFile handler: opens the disk file, parses any
// Range header, and streams via Resp.Stream — Go's runtime promotes
// io.Copy(*TCPConn, *os.File) into a sendfile/splice syscall on Linux,
// so the bytes go kernel→kernel without a userspace copy.
func serveFile(req *gogohttp.Req, resp *gogohttp.Resp, entry *router.RouteEntry, manifest *router.Manifest, st *appState) {
	blob := manifest.HoleBlobs[entry.HoleOffset:]
	path, err := readFileBlob(blob)
	if err != nil {
		resp.ServerError()
		return
	}

	file, err := os.Open(path)
	if err != nil {
		resp.NotFound()
		return
	}
	// Closed inside the Stream closure below — must outlive this function.

	fi, err := file.Stat()
	if err != nil {
		file.Close()
		resp.ServerError()
		return
	}

	heads, ok := st.fileHeads[mimeForFile(path)]
	if !ok {
		// Mime wasn't pre-registered (manifest changed underneath?). Bail
		// rather than serve with no Content-Type.
		file.Close()
		resp.ServerError()
		return
	}

	// HEAD: send headers only, skip the body. The Stream closure isn't
	// called by the server when isHead is set, but the file still needs
	// closing — do it now.
	if req.Method == gogohttp.MethodHEAD {
		file.Close()
		resp.HeadID = heads.head200
		resp.Body = nil
		// Stream isn't set, so server uses the buffered path with len(Body)=0
		// — but Content-Length must reflect the file size. Use ExtraHeaders
		// to override... actually our writeBuffered uses len(Body) for CL.
		// For HEAD we want the real file size in Content-Length. Sending
		// via Stream-with-noop instead, server will not call the closure
		// for HEAD anyway, so set Stream just to flag the chunked path.
		// Simpler fallback: serve via Stream and let writeStreamHEAD do it.
		resp.Stream = func(w io.Writer) error { return nil }
		return
	}

	rangeHdr := headerValue(req, "Range")
	if rangeHdr != "" {
		start, end, ok := parseByteRange(rangeHdr, fi.Size())
		if !ok {
			file.Close()
			// Malformed/unsatisfiable range: respond 416 if we had a head
			// for it; for now fall back to the whole file.
			resp.HeadID = heads.head200
			resp.Stream = func(w io.Writer) error {
				defer file.Close()
				_, err := io.Copy(w, file)
				return err
			}
			return
		}
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			file.Close()
			resp.ServerError()
			return
		}
		resp.HeadID = heads.head206
		resp.SetContentRange(start, end, fi.Size())
		length := end - start + 1
		resp.Stream = func(w io.Writer) error {
			defer file.Close()
			_, err := io.CopyN(w, file, length)
			return err
		}
		return
	}

	// No range — full file.
	resp.HeadID = heads.head200
	resp.Stream = func(w io.Writer) error {
		defer file.Close()
		_, err := io.Copy(w, file)
		return err
	}
}

// headerValue returns the first value of a header by name (case-insensitive),
// or "" if absent. Linear scan over req.Headers — fine for the small fixed
// header counts gogohttp parses.
func headerValue(req *gogohttp.Req, name string) string {
	target := []byte(name)
	for i := 0; i < req.NHeaders; i++ {
		h := &req.Headers[i]
		if eqASCIIFold(h.Name, target) {
			return string(h.Value)
		}
	}
	return ""
}

func eqASCIIFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
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

// parseByteRange parses a single-range "bytes=N-M" / "bytes=N-" / "bytes=-N"
// header value against a known total file size, returning inclusive start
// and end offsets. Multi-range requests (comma-separated) are not supported
// — caller falls back to a 200 full response.
func parseByteRange(header string, total int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return 0, 0, false
	}
	spec := header[len(prefix):]
	if commaIdx := indexByteStr(spec, ','); commaIdx >= 0 {
		// Multi-range — only honor the first for simplicity.
		spec = spec[:commaIdx]
	}
	dashIdx := indexByteStr(spec, '-')
	if dashIdx < 0 {
		return 0, 0, false
	}
	startStr := spec[:dashIdx]
	endStr := spec[dashIdx+1:]

	switch {
	case startStr == "" && endStr == "":
		return 0, 0, false
	case startStr == "":
		// bytes=-N → last N bytes
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, true
	case endStr == "":
		// bytes=N- → from N to end
		s, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || s < 0 || s >= total {
			return 0, 0, false
		}
		return s, total - 1, true
	default:
		// bytes=N-M
		s, err1 := strconv.ParseInt(startStr, 10, 64)
		e, err2 := strconv.ParseInt(endStr, 10, 64)
		if err1 != nil || err2 != nil || s < 0 || e < s || s >= total {
			return 0, 0, false
		}
		if e >= total {
			e = total - 1
		}
		return s, e, true
	}
}

func indexByteStr(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

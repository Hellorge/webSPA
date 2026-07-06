// Package compress holds the gzip and brotli helpers shared between the
// build pipeline (which pre-compresses static bodies into the manifest)
// and the runtime renderer (which compresses freshly-rendered hole
// output before caching it). Centralised here so both call sites use
// identical settings — same compression level, same library version —
// guaranteeing byte-for-byte equivalent variants regardless of which
// path produced them.
package compress

import (
	"bytes"
	"compress/gzip"

	"github.com/andybalholm/brotli"
)

// Gzip returns data compressed at BestCompression. Used for build-time
// variant baking AND runtime hole-output variant population. Both paths
// favour size over CPU; misses are rare in both contexts.
func Gzip(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// Brotli returns data compressed at BestCompression. Same rationale as
// Gzip — wire size matters more than encode CPU for our cache-then-
// serve-forever pattern.
func Brotli(data []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

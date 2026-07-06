package router

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

// MagicV4 is the file header for Gogogo V4 binaries. V4 adds:
//   - hole-based runtime rendering (HoleOffset on RouteEntry, HoleBlobs section)
// V3 backward-compat is dropped — V3 manifests must be rebuilt.
// V5 adds Method bitmask + middleware list offset/count to RouteEntry,
// growing it from 56 to 64 bytes (one cache line). V4 manifests must be
// rebuilt against V5; old manifests are rejected at load.
const MagicV5 = "GOGOV5\x00\x00"
const MagicV4 = "GOGOV4\x00\x00"

// MagicV3 retained as a constant only so older code paths recognize and
// reject pre-V4 files explicitly rather than silently mis-parsing.
const MagicV3 = "GOGOV3\x00\x00"

// Encoding indices into RouteEntry.Variants.
const (
	EncIdentity = 0
	EncGzip     = 1
	EncBrotli   = 2
)

// Variant is one content-encoded version of a route's body. DataLen == 0
// means this variant is absent. HeadID indexes into the runtime registry.
type Variant struct {
	HeadID     uint16
	_          uint16
	DataOffset uint32
	DataLen    uint32
}

// RouteEntry: 64 bytes per entry, 8-byte aligned (one cache line on x86-64).
//
// HoleOffset is 0 for routes with no runtime data (the common case);
// non-zero values index into the manifest's HoleBlobs section, where the
// per-route hole list lives.
//
// Methods is a bitmask declaring which HTTP methods this route handles.
// See MethodBit* constants. A request whose method bit is not set in this
// mask gets a 405 Method Not Allowed response, with a pre-baked Allow
// header derived from the bitmask.
//
// MwOffset/MwCount index into Manifest.MiddlewareBlobs — the per-route
// ordered list of middleware names. MwCount == 0 → no middleware.
type RouteEntry struct {
	PathHash   uint64
	PathOffset uint32
	PathLen    uint16
	ActionID   uint16
	Variants   [3]Variant
	HoleOffset uint32 // 0 = no holes
	Methods    uint16 // bitmask of allowed HTTP methods
	MwCount    uint8  // number of middleware IDs in the per-route list
	_          uint8  // padding
	MwOffset   uint32 // byte offset into MiddlewareBlobs (only valid if MwCount > 0)
}

const RouteEntrySize = 64

// HTTP method bits in RouteEntry.Methods. Sized as uint16 (16 bits)
// to cover HTTP/1.1 standard methods plus PATCH plus future verbs;
// 9 are spec'd today, 7 reserved for growth (WebDAV etc. if ever needed).
const (
	MethodBitGET     uint16 = 1 << 0
	MethodBitHEAD    uint16 = 1 << 1
	MethodBitPOST    uint16 = 1 << 2
	MethodBitPUT     uint16 = 1 << 3
	MethodBitPATCH   uint16 = 1 << 4
	MethodBitDELETE  uint16 = 1 << 5
	MethodBitOPTIONS uint16 = 1 << 6
	MethodBitCONNECT uint16 = 1 << 7
	MethodBitTRACE   uint16 = 1 << 8
	// 9..15 reserved
)

// ParseMethods turns a comma- or whitespace-separated list of method
// names ("GET", "POST", "GET, PUT") into a Methods bitmask. An empty
// string defaults to GET. Declaring GET implicitly enables HEAD —
// servers should auto-derive HEAD responses from their GET handler
// (writing only headers, no body), so requiring callers to declare
// HEAD redundantly is noise. Unknown method names are silently skipped.
func ParseMethods(s string) uint16 {
	if s == "" {
		return MethodBitGET | MethodBitHEAD
	}
	var bits uint16
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' || s[i] == ' ' || s[i] == '\t' {
			if start < i {
				bits |= methodBitOf(s[start:i])
			}
			start = i + 1
		}
	}
	if bits&MethodBitGET != 0 {
		bits |= MethodBitHEAD
	}
	return bits
}

func methodBitOf(name string) uint16 {
	switch name {
	case "GET", "get":
		return MethodBitGET
	case "HEAD", "head":
		return MethodBitHEAD
	case "POST", "post":
		return MethodBitPOST
	case "PUT", "put":
		return MethodBitPUT
	case "PATCH", "patch":
		return MethodBitPATCH
	case "DELETE", "delete":
		return MethodBitDELETE
	case "OPTIONS", "options":
		return MethodBitOPTIONS
	case "CONNECT", "connect":
		return MethodBitCONNECT
	case "TRACE", "trace":
		return MethodBitTRACE
	}
	return 0
}

// AllowHeaderValue returns the comma-separated list of method names
// implied by the bitmask, sorted in canonical order. Used at server
// load to pre-bake 405 Allow headers per unique bitmask.
func AllowHeaderValue(bits uint16) string {
	var out []byte
	add := func(name string, mask uint16) {
		if bits&mask == 0 {
			return
		}
		if len(out) > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, name...)
	}
	add("GET", MethodBitGET)
	add("HEAD", MethodBitHEAD)
	add("POST", MethodBitPOST)
	add("PUT", MethodBitPUT)
	add("PATCH", MethodBitPATCH)
	add("DELETE", MethodBitDELETE)
	add("OPTIONS", MethodBitOPTIONS)
	add("CONNECT", MethodBitCONNECT)
	add("TRACE", MethodBitTRACE)
	return string(out)
}

// HoleBlobs layout:
//   At each route's HoleOffset:
//     [N uint16] — number of holes for this route
//     repeat N times:
//       [Position    uint32] — splice byte offset within the static body
//       [SQLLen      uint16] — bytes of SQL that follow
//       [SQL bytes]
//       [TemplateLen uint16] — bytes of sub-template that follow
//       [Template bytes]
//
// Inline storage keeps the runtime resolver allocation-free for the lookup
// path; the only allocations happen when actually rendering a hole's
// query results, which is unavoidable.

type Manifest struct {
	Header          [8]byte
	Table           []RouteEntry
	Payloads        []byte
	Paths           []byte
	HoleBlobs       []byte
	MiddlewareBlobs []byte // packed [nameLen u8][name bytes] entries per route
	MimeTable       []string

	// Trie is the route-matching tree, built at Load from each
	// RouteEntry's Path bytes. Supports both static URLs and patterns
	// with ":id" / "*path" syntax. The runtime walks this instead of
	// binary-searching by PathHash.
	//
	// Built once per Manifest; immutable for its lifetime. Manifest
	// reload (SIGHUP) constructs a new Manifest with a fresh Trie.
	Trie *Trie

	mmaped []byte
}

// MiddlewareNamesAt returns the per-route middleware names given a
// RouteEntry's MwOffset and MwCount. Each name is decoded from a
// length-prefixed entry in MiddlewareBlobs. cmd/main resolves names
// to registered middleware functions at server load.
func (m *Manifest) MiddlewareNamesAt(off uint32, count uint8) []string {
	if count == 0 {
		return nil
	}
	out := make([]string, 0, count)
	pos := int(off)
	for i := 0; i < int(count); i++ {
		if pos >= len(m.MiddlewareBlobs) {
			return nil
		}
		n := int(m.MiddlewareBlobs[pos])
		pos++
		if pos+n > len(m.MiddlewareBlobs) {
			return nil
		}
		out = append(out, string(m.MiddlewareBlobs[pos:pos+n]))
		pos += n
	}
	return out
}

// Load parses a V5 manifest from disk. Layout:
//
//   [Magic 8 bytes]
//   [TableSize uint32]
//   [Table TableSize × RouteEntrySize bytes]
//   [Payloads bytes]
//   [Paths bytes]
//   [HoleBlobs bytes]
//   [MiddlewareBlobs bytes]  (NEW in V5)
//   [MimeTable JSON]
//   Trailer (read from end-of-file backwards):
//     [MwIDsLen     uint32]  (NEW in V5)
//     [HoleBlobsLen uint32]
//     [PathsLen     uint32]
//     [JSONLen      uint32]
//
// V4 manifests are rejected — the format change (RouteEntry grew from
// 56 to 64 bytes for the Methods + middleware fields) is incompatible
// at the byte level and silent acceptance would mis-decode entries.
func Load(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := int(fi.Size())
	if size < 12 {
		return nil, fmt.Errorf("manifest too small (%d bytes)", size)
	}

	mmaped, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	if string(mmaped[:8]) != MagicV5 {
		syscall.Munmap(mmaped)
		return nil, fmt.Errorf("invalid v5 binary header (got %q, want %q — older formats must be rebuilt)", string(mmaped[:8]), MagicV5)
	}

	tableSize := binary.LittleEndian.Uint32(mmaped[8:12])
	tableBytes := int(tableSize) * RouteEntrySize
	tableEnd := 12 + tableBytes
	if tableEnd > size {
		syscall.Munmap(mmaped)
		return nil, fmt.Errorf("table extends past file (table=%d size=%d)", tableEnd, size)
	}
	fmt.Printf("V5 Router: Loading Table with %d entries (mmap)\n", tableSize)

	table := make([]RouteEntry, tableSize)
	r := bytes.NewReader(mmaped[12:tableEnd])
	for i := uint32(0); i < tableSize; i++ {
		if err := binary.Read(r, binary.LittleEndian, &table[i]); err != nil {
			syscall.Munmap(mmaped)
			return nil, fmt.Errorf("decode entry %d: %w", i, err)
		}
	}

	if size < tableEnd+16 {
		syscall.Munmap(mmaped)
		return nil, fmt.Errorf("manifest trailer too short")
	}

	manifest := &Manifest{Header: [8]byte{}, Table: table, mmaped: mmaped}
	copy(manifest.Header[:], mmaped[:8])

	jsonLen := int(binary.LittleEndian.Uint32(mmaped[size-4:]))
	pathsLen := int(binary.LittleEndian.Uint32(mmaped[size-8 : size-4]))
	holeBlobsLen := int(binary.LittleEndian.Uint32(mmaped[size-12 : size-8]))
	mwBlobsLen := int(binary.LittleEndian.Uint32(mmaped[size-16 : size-12]))

	if tableEnd+jsonLen+pathsLen+holeBlobsLen+mwBlobsLen+16 > size {
		syscall.Munmap(mmaped)
		return nil, fmt.Errorf("trailer sizes invalid (json=%d paths=%d holes=%d mw=%d)", jsonLen, pathsLen, holeBlobsLen, mwBlobsLen)
	}

	jsonStart := size - 16 - jsonLen
	mwBlobsStart := jsonStart - mwBlobsLen
	holeBlobsStart := mwBlobsStart - holeBlobsLen
	pathsStart := holeBlobsStart - pathsLen

	if jsonLen > 0 {
		if err := json.Unmarshal(mmaped[jsonStart:jsonStart+jsonLen], &manifest.MimeTable); err != nil {
			syscall.Munmap(mmaped)
			return nil, fmt.Errorf("decode MimeTable: %w", err)
		}
	}
	manifest.MiddlewareBlobs = mmaped[mwBlobsStart:jsonStart]
	manifest.HoleBlobs = mmaped[holeBlobsStart:mwBlobsStart]
	manifest.Paths = mmaped[pathsStart:holeBlobsStart]
	manifest.Payloads = mmaped[tableEnd:pathsStart]

	// Build the route-matching trie. Each Table entry's path bytes
	// (from Paths blob) is treated as a pattern — static URLs work
	// unchanged; patterns with ":id" / "*path" become param/catchall
	// nodes. Trie is built in-process at load; it's not part of the
	// binary format so the manifest schema stays untouched.
	manifest.Trie = NewTrie()
	for i := range manifest.Table {
		entry := &manifest.Table[i]
		path := string(manifest.Paths[entry.PathOffset : entry.PathOffset+uint32(entry.PathLen)])
		if path == "" {
			path = "/"
		} else if path[0] != '/' {
			path = "/" + path
		}
		if err := manifest.Trie.Insert(path, i+1); err != nil {
			syscall.Munmap(mmaped)
			return nil, fmt.Errorf("trie insert %q at idx %d: %w", path, i, err)
		}
	}

	return manifest, nil
}

// LoadV3 / LoadV4 retained as aliases to Load (V5) so callers we haven't
// renamed still compile. They'll error at runtime if they encounter an
// older-format file because Load only accepts V5.
func LoadV3(path string) (*Manifest, error) { return Load(path) }
func LoadV4(path string) (*Manifest, error) { return Load(path) }

// Close munmaps the underlying file. Safe to call multiple times.
func (m *Manifest) Close() error {
	if m.mmaped == nil {
		return nil
	}
	err := syscall.Munmap(m.mmaped)
	m.mmaped = nil
	m.Payloads = nil
	m.Paths = nil
	m.HoleBlobs = nil
	return err
}

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"gogogo/modules/actions"
	"gogogo/modules/build"
	"gogogo/modules/compress"
	"gogogo/modules/router"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// V2Compiler builds a V4 manifest. Two ways to drive it:
//
//   - Compile(pipelineResults, outPath) — the main flow used for sites
//     whose content goes through the build pipeline (templates, minify,
//     etc.). Adapts pipeline ProcessResults into entries.
//
//   - Direct API (AddStaticRoute / AddActionRoute / Write) — for sites or
//     overlays where we already have raw bytes and want a manifest without
//     pipeline involvement (e.g., the studio admin site).
//
// Both paths share the same internal state and write the same trailer
// format, so the runtime treats them identically.
type V2Compiler struct {
	// SiteName is the optional site label included in compiler log lines.
	// Empty string is fine — used by the studio module's direct-build path.
	SiteName string

	nodes     []router.RouteEntry
	payloads  []byte
	paths     []byte
	holeBlobs []byte
	mwBlobs   []byte // packed [nameLen u8][name bytes] entries per route
	mimes     []string
	mimeIdx   map[string]uint16
}

// appendMiddleware emits a per-route middleware list into mwBlobs and
// returns (offset, count) for the route entry. names is the parsed
// middleware list; empty input returns (0, 0).
func (c *V2Compiler) appendMiddleware(names []string) (uint32, uint8) {
	if len(names) == 0 {
		return 0, 0
	}
	off := uint32(len(c.mwBlobs))
	count := 0
	for _, name := range names {
		if name == "" {
			continue
		}
		if len(name) > 255 {
			name = name[:255]
		}
		c.mwBlobs = append(c.mwBlobs, byte(len(name)))
		c.mwBlobs = append(c.mwBlobs, []byte(name)...)
		count++
	}
	if count == 0 {
		return 0, 0
	}
	return off, uint8(count)
}

// parseMiddleware splits a comma-separated middleware spec into names,
// trimming whitespace. "auth, rate:10/min, audit" → ["auth", "rate:10/min", "audit"].
func parseMiddleware(spec string) []string {
	if spec == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(spec); i++ {
		if i == len(spec) || spec[i] == ',' {
			seg := spec[start:i]
			// trim whitespace
			for len(seg) > 0 && (seg[0] == ' ' || seg[0] == '\t') {
				seg = seg[1:]
			}
			for len(seg) > 0 && (seg[len(seg)-1] == ' ' || seg[len(seg)-1] == '\t') {
				seg = seg[:len(seg)-1]
			}
			if seg != "" {
				out = append(out, seg)
			}
			start = i + 1
		}
	}
	return out
}

func NewV2Compiler() *V2Compiler {
	return &V2Compiler{
		mimes:   make([]string, 0),
		mimeIdx: make(map[string]uint16),
	}
}

// Compile drives the pipeline-results path: takes a map of relPath →
// ProcessResult and bakes each into the manifest, then appends the
// app-level dynamic action routes from actions.Routes.
func (c *V2Compiler) Compile(results map[string]ProcessResult, outPath string) error {
	fmt.Printf("Gogogo V4 [%s]: Commencing Manifest Compilation with %d results...\n", c.tagOrDefault(), len(results))

	c.nodes = make([]router.RouteEntry, 0, len(results))
	entryMap := make(map[uint64]string)

	for relPath, res := range results {
		ext := filepath.Ext(relPath)
		if ext == ".toml" || strings.HasSuffix(relPath, ".swp") {
			continue
		}

		target := strings.Trim(filepath.ToSlash(res.FileInfo.AliasedPath), "/")
		hash := hashPath(target)
		if existing, exists := entryMap[hash]; exists && existing != relPath {
			fmt.Printf(" [!] IGNORED COLLISION: URL '%s' (from %s) already claimed by %s\n", target, relPath, existing)
			continue
		}
		entryMap[hash] = relPath
		c.addPipelineEntry(target, relPath, res)
	}

	for _, route := range actions.Routes {
		hash := hashPath(route.Path)
		if existing, exists := entryMap[hash]; exists {
			return fmt.Errorf("dynamic action route %q (ID %d) collides with static path %q", route.Path, route.ActionID, existing)
		}
		entryMap[hash] = route.Path
		c.AddActionRoute(route.Path, route.Method, route.Middleware, route.ActionID)
	}

	c.printStats()
	return c.Write(outPath)
}

// AddStaticRoute bakes raw body bytes as a static manifest entry. Pre-
// compresses the body for compressible mime types (gzip + brotli).
// path is the URL path with no leading slash (use "" for the site root).
// Static routes are GET (and auto-HEAD) only.
func (c *V2Compiler) AddStaticRoute(path string, body []byte, mimeStr string) {
	entry := router.RouteEntry{
		PathHash:   hashPath(path),
		PathOffset: uint32(len(c.paths)),
		PathLen:    uint16(len(path)),
		ActionID:   actions.ActionStatic,
		Methods:    router.MethodBitGET | router.MethodBitHEAD,
	}
	c.paths = append(c.paths, []byte(path)...)
	if len(body) > 0 {
		c.fillStaticVariants(&entry, body, mimeStr)
	}
	c.nodes = append(c.nodes, entry)
}

// AddActionRoute registers a dynamic-action route. No body, no variants —
// the runtime dispatches to the handler keyed by ActionID. methodSpec is
// a comma-separated list ("GET", "POST", "GET, PUT"); empty defaults to
// GET (with HEAD auto-derived). middlewareSpec is the comma-separated
// list of middleware names (e.g. "auth, rate:10/min").
func (c *V2Compiler) AddActionRoute(path, methodSpec, middlewareSpec string, actionID uint16) {
	mwOff, mwCount := c.appendMiddleware(parseMiddleware(middlewareSpec))
	entry := router.RouteEntry{
		PathHash:   hashPath(path),
		PathOffset: uint32(len(c.paths)),
		PathLen:    uint16(len(path)),
		ActionID:   actionID,
		Methods:    router.ParseMethods(methodSpec),
		MwOffset:   mwOff,
		MwCount:    mwCount,
	}
	c.paths = append(c.paths, []byte(path)...)
	c.nodes = append(c.nodes, entry)
}

// Write sorts the route table by hash and serializes the manifest atomically
// (write to .tmp, then rename) so any process with the previous manifest
// mmap'd doesn't see partial bytes mid-write.
func (c *V2Compiler) Write(outPath string) error {
	sort.Slice(c.nodes, func(i, j int) bool {
		return c.nodes[i].PathHash < c.nodes[j].PathHash
	})

	tmpPath := outPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	f.Write([]byte(router.MagicV5))
	binary.Write(f, binary.LittleEndian, uint32(len(c.nodes)))
	for i := range c.nodes {
		binary.Write(f, binary.LittleEndian, &c.nodes[i])
	}
	f.Write(c.payloads)
	f.Write(c.paths)
	f.Write(c.holeBlobs)
	f.Write(c.mwBlobs)
	mimeJSON, _ := json.Marshal(c.mimes)
	f.Write(mimeJSON)
	// Trailer sizes — order matters: mwBlobs, holeBlobs, paths, json (read backward).
	binary.Write(f, binary.LittleEndian, uint32(len(c.mwBlobs)))
	binary.Write(f, binary.LittleEndian, uint32(len(c.holeBlobs)))
	binary.Write(f, binary.LittleEndian, uint32(len(c.paths)))
	binary.Write(f, binary.LittleEndian, uint32(len(mimeJSON)))

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// addPipelineEntry adapts a pipeline ProcessResult to an entry. Used by
// Compile(). Three shapes:
//   - ActionFile (large disk-served): blob holds disk path; no Variants.
//   - ActionStatic with holes: identity-only variant + hole blob.
//   - ActionStatic without holes: full identity/gzip/brotli variants.
func (c *V2Compiler) addPipelineEntry(path, sourcePath string, res ProcessResult) {
	entry := router.RouteEntry{
		PathHash:   hashPath(path),
		PathOffset: uint32(len(c.paths)),
		PathLen:    uint16(len(path)),
		ActionID:   res.FileInfo.ActionID,
		Methods:    router.MethodBitGET | router.MethodBitHEAD,
	}
	c.paths = append(c.paths, []byte(path)...)

	switch res.FileInfo.ActionID {
	case actions.ActionFile:
		entry.HoleOffset = c.appendFileBlob(res.FilePath)

	case actions.ActionStatic:
		if len(res.Holes) > 0 {
			mime := mimeFor(sourcePath)
			entry.Variants[router.EncIdentity] = router.Variant{
				HeadID:     c.internMime(mime, ""),
				DataOffset: uint32(len(c.payloads)),
				DataLen:    uint32(len(res.FileInfo.EmbeddedData)),
			}
			c.payloads = append(c.payloads, res.FileInfo.EmbeddedData...)
			entry.HoleOffset = c.appendHoleBlob(res.Holes)
		} else if res.FileInfo.EmbeddedData != nil {
			c.fillStaticVariants(&entry, res.FileInfo.EmbeddedData, mimeFor(sourcePath))
		}
	}

	c.nodes = append(c.nodes, entry)
}

// appendFileBlob writes a file-route blob: [pathLen u16][path bytes].
// Returned offset goes into RouteEntry.HoleOffset (the field is overloaded
// for any per-route metadata blob — name retained for ABI stability).
//
// Mime type and head IDs are NOT baked here. The runtime derives mime from
// the file extension at server load and registers heads in the registry
// — keeps blobs small and avoids head-ID remapping on mmap'd memory.
func (c *V2Compiler) appendFileBlob(path string) uint32 {
	if len(c.holeBlobs) == 0 {
		c.holeBlobs = append(c.holeBlobs, 0)
	}
	off := uint32(len(c.holeBlobs))

	var pathLen [2]byte
	binary.LittleEndian.PutUint16(pathLen[:], uint16(len(path)))
	c.holeBlobs = append(c.holeBlobs, pathLen[:]...)
	c.holeBlobs = append(c.holeBlobs, []byte(path)...)
	return off
}

// appendHoleBlob serializes one route's hole list into c.holeBlobs and
// returns the byte offset where it begins. HoleOffset 0 is reserved for
// "no holes," so we ensure HoleBlobs starts with at least one byte and
// real entries land at offset 1+.
func (c *V2Compiler) appendHoleBlob(holes []build.Hole) uint32 {
	if len(c.holeBlobs) == 0 {
		// Reserve offset 0 as the "no holes" sentinel.
		c.holeBlobs = append(c.holeBlobs, 0)
	}
	off := uint32(len(c.holeBlobs))

	var nbuf [2]byte
	binary.LittleEndian.PutUint16(nbuf[:], uint16(len(holes)))
	c.holeBlobs = append(c.holeBlobs, nbuf[:]...)

	for _, h := range holes {
		var posBuf [4]byte
		binary.LittleEndian.PutUint32(posBuf[:], h.Position)
		c.holeBlobs = append(c.holeBlobs, posBuf[:]...)

		var sqlLenBuf [2]byte
		binary.LittleEndian.PutUint16(sqlLenBuf[:], uint16(len(h.SQL)))
		c.holeBlobs = append(c.holeBlobs, sqlLenBuf[:]...)
		c.holeBlobs = append(c.holeBlobs, []byte(h.SQL)...)

		var tplLenBuf [2]byte
		binary.LittleEndian.PutUint16(tplLenBuf[:], uint16(len(h.Template)))
		c.holeBlobs = append(c.holeBlobs, tplLenBuf[:]...)
		c.holeBlobs = append(c.holeBlobs, h.Template...)
	}
	return off
}

// fillStaticVariants is shared between the pipeline-results path and the
// direct API. It populates [identity, gzip, brotli] variants on entry and
// appends the bodies into the payloads section.
func (c *V2Compiler) fillStaticVariants(entry *router.RouteEntry, body []byte, mimeStr string) {
	entry.Variants[router.EncIdentity] = router.Variant{
		HeadID:     c.internMime(mimeStr, ""),
		DataOffset: uint32(len(c.payloads)),
		DataLen:    uint32(len(body)),
	}
	c.payloads = append(c.payloads, body...)

	if isCompressible(mimeStr) && len(body) > 0 {
		gz := compress.Gzip(body)
		entry.Variants[router.EncGzip] = router.Variant{
			HeadID:     c.internMime(mimeStr, "gzip"),
			DataOffset: uint32(len(c.payloads)),
			DataLen:    uint32(len(gz)),
		}
		c.payloads = append(c.payloads, gz...)

		br := compress.Brotli(body)
		entry.Variants[router.EncBrotli] = router.Variant{
			HeadID:     c.internMime(mimeStr, "br"),
			DataOffset: uint32(len(c.payloads)),
			DataLen:    uint32(len(br)),
		}
		c.payloads = append(c.payloads, br...)
	}
}

func (c *V2Compiler) internMime(m, encoding string) uint16 {
	key := m
	if encoding != "" {
		key = m + "|" + encoding
	}
	if id, ok := c.mimeIdx[key]; ok {
		return id
	}
	id := uint16(len(c.mimes))
	c.mimes = append(c.mimes, key)
	c.mimeIdx[key] = id
	return id
}

func (c *V2Compiler) printStats() {
	totalRaw := uint64(0)
	totalCompressed := uint64(0)
	for _, n := range c.nodes {
		raw := uint64(n.Variants[router.EncIdentity].DataLen)
		totalRaw += raw
		best := raw
		if v := n.Variants[router.EncBrotli]; v.DataLen > 0 && uint64(v.DataLen) < best {
			best = uint64(v.DataLen)
		} else if v := n.Variants[router.EncGzip]; v.DataLen > 0 && uint64(v.DataLen) < best {
			best = uint64(v.DataLen)
		}
		totalCompressed += best
	}
	savings := 0
	if totalRaw > 0 {
		savings = int(100 - (totalCompressed * 100 / totalRaw))
	}
	holes, files := 0, 0
	for _, n := range c.nodes {
		switch n.ActionID {
		case actions.ActionStatic:
			if n.HoleOffset != 0 {
				holes++
			}
		case actions.ActionFile:
			files++
		}
	}
	fmt.Printf("Gogogo V4 [%s]: %d routes (%d with holes, %d disk-served), %d unique mime+encoding entries, raw=%d compressed=%d (%d%% saved)\n",
		c.tagOrDefault(), len(c.nodes), holes, files, len(c.mimes), totalRaw, totalCompressed, savings)
}

func (c *V2Compiler) tagOrDefault() string {
	if c.SiteName == "" {
		return "site"
	}
	return c.SiteName
}

func mimeFor(path string) string {
	m := mime.TypeByExtension(filepath.Ext(path))
	if m == "" {
		return "application/octet-stream"
	}
	return m
}

// isCompressible decides whether to spend build-time CPU producing gzip and
// brotli variants. Pre-compressed binary formats (images, video, fonts) are
// already entropy-maxed; compression would inflate them and waste storage.
func isCompressible(mimeStr string) bool {
	return strings.HasPrefix(mimeStr, "text/") ||
		strings.HasPrefix(mimeStr, "application/json") ||
		strings.HasPrefix(mimeStr, "application/javascript") ||
		strings.HasPrefix(mimeStr, "application/xml") ||
		strings.HasPrefix(mimeStr, "application/wasm") ||
		strings.HasPrefix(mimeStr, "image/svg+xml")
}

func hashPath(path string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= 1099511628211
	}
	return h
}

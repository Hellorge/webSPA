package main

import (
	"hash/fnv"
	"sync"

	"gogogo/modules/compress"
	"gogogo/modules/gogohttp"
	"gogogo/modules/router"
)

// outputCache stores fully-rendered hole-page output. Cache hits skip
// SQL, per-row template execution, the splice, AND compression — they
// just hand back finished bytes for writev.
//
// Key shape: outputKey{manifest, holeOffset, paramsHash}. The
// paramsHash is FNV-64 of the URL bindings; routes with no bindings
// have paramsHash == 0. This means:
//
//   - /static/page → one entry per page
//   - /users/:id → one entry per distinct id requested (bounded by LRU)
//   - /catchall/*rest → one entry per distinct rest captured
//
// LRU + byte cap manage cardinality. High-churn unique URLs (e.g.
// per-session UUIDs) churn LRU and effectively don't cache; repeated
// dynamic URLs (popular product pages, common user profiles) hit the
// cache like static.
//
// Invalidation is by referenced table set, not by key — when fastdb
// signals a write to table T, every entry whose holes read from T is
// dropped (across all paramsHash values for the same route).
type outputCache struct {
	mu      sync.Mutex
	entries map[outputKey]*outputEntry
	reverse map[string]map[outputKey]struct{}

	head, tail *outputEntry // LRU: head = most-recently-used

	maxEntries int
	maxBytes   int
	curBytes   int

	hits, misses, evictions uint64
}

// outputKey identifies one cached page variant by (manifest, hole
// offset, params hash). paramsHash == 0 means "no URL bindings,"
// distinguishing static-URL routes from /any-route/:noparam routes.
type outputKey struct {
	manifest *router.Manifest
	off      uint32
	params   uint64
}

// outputEntry is one cached hole page. variants is indexed by encoding
// (router.EncIdentity / EncGzip / EncBrotli). The LRU links are
// intrusive — the entry IS a node in the doubly-linked list, no
// separate list node allocation.
//
// body slices and head IDs are owned by the cache; handlers slice them
// into resp and must not mutate. Concurrent invalidations only unlink
// the entry from the map; in-flight handlers reading body finish
// safely (Go's GC retains the entry until the last reference drops).
type outputEntry struct {
	key      outputKey
	variants [3]variantBody
	tables   []string
	bytes    int

	prev, next *outputEntry
}

type variantBody struct {
	body []byte
	head uint16
}

const (
	outputCacheMaxEntries = 4096
	outputCacheMaxBytes   = 32 << 20 // 32 MiB
)

func newOutputCache() *outputCache {
	return &outputCache{
		entries:    make(map[outputKey]*outputEntry, 32),
		reverse:    make(map[string]map[outputKey]struct{}, 8),
		maxEntries: outputCacheMaxEntries,
		maxBytes:   outputCacheMaxBytes,
	}
}

// Get returns the cached entry for key, or (nil, false) on miss. On
// hit the entry is promoted to MRU position in the LRU list.
func (oc *outputCache) Get(key outputKey) (*outputEntry, bool) {
	oc.mu.Lock()
	e, ok := oc.entries[key]
	if ok {
		oc.moveToFrontLocked(e)
		oc.hits++
	} else {
		oc.misses++
	}
	oc.mu.Unlock()
	return e, ok
}

// Put stores entry under key, attributing it to its tables for
// invalidation. tables == nil means "uncacheable" — Put is a no-op
// (caller serves fresh each request without populating).
//
// If an entry already exists for key, it's replaced. Eviction runs
// until the cache is back under maxEntries and maxBytes.
func (oc *outputCache) Put(key outputKey, entry *outputEntry) {
	if entry == nil || entry.tables == nil {
		return
	}
	entry.key = key
	entry.bytes = entrySize(entry)

	oc.mu.Lock()
	defer oc.mu.Unlock()

	if existing, ok := oc.entries[key]; ok {
		oc.removeLocked(existing)
	}

	oc.entries[key] = entry
	for _, t := range entry.tables {
		s, ok := oc.reverse[t]
		if !ok {
			s = make(map[outputKey]struct{}, 1)
			oc.reverse[t] = s
		}
		s[key] = struct{}{}
	}
	oc.pushFrontLocked(entry)
	oc.curBytes += entry.bytes

	for len(oc.entries) > oc.maxEntries || oc.curBytes > oc.maxBytes {
		if oc.tail == nil {
			break
		}
		oc.removeLocked(oc.tail)
		oc.evictions++
	}
}

// Invalidate drops every cached entry that references table. table == ""
// is a sentinel meaning "all tables changed" (used when fastdb's
// data_version watcher detects an out-of-pool write) — flushes
// everything.
//
// For a named table, the work is O(entries-touching-this-table).
// Different (manifest, off, params) entries that all hit the same SQL
// table all get dropped in one pass.
func (oc *outputCache) Invalidate(table string) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	if table == "" {
		oc.entries = make(map[outputKey]*outputEntry, 32)
		oc.reverse = make(map[string]map[outputKey]struct{}, 8)
		oc.head, oc.tail = nil, nil
		oc.curBytes = 0
		return
	}

	set, ok := oc.reverse[table]
	if !ok {
		return
	}
	for key := range set {
		e, exists := oc.entries[key]
		if !exists {
			continue
		}
		oc.removeLocked(e)
	}
	delete(oc.reverse, table)
}

// --- LRU helpers (caller holds oc.mu) ---

func (oc *outputCache) pushFrontLocked(e *outputEntry) {
	e.prev = nil
	e.next = oc.head
	if oc.head != nil {
		oc.head.prev = e
	}
	oc.head = e
	if oc.tail == nil {
		oc.tail = e
	}
}

func (oc *outputCache) moveToFrontLocked(e *outputEntry) {
	if e == oc.head {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if e == oc.tail {
		oc.tail = e.prev
	}
	e.prev = nil
	e.next = oc.head
	if oc.head != nil {
		oc.head.prev = e
	}
	oc.head = e
}

func (oc *outputCache) removeLocked(e *outputEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		oc.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		oc.tail = e.prev
	}
	delete(oc.entries, e.key)
	for _, t := range e.tables {
		if s, ok := oc.reverse[t]; ok {
			delete(s, e.key)
			if len(s) == 0 {
				delete(oc.reverse, t)
			}
		}
	}
	oc.curBytes -= e.bytes
}

// entrySize is a rough sizer for the byte budget. Counts each variant
// body's length plus the tables slice's footprint. Doesn't account for
// map/struct overhead — close enough for proportional eviction.
func entrySize(e *outputEntry) int {
	n := 0
	for i := range e.variants {
		n += len(e.variants[i].body)
	}
	for _, t := range e.tables {
		n += len(t)
	}
	return n
}

// hashBindings computes a stable hash of the URL bindings. Bindings
// come from the trie in pattern order (the order params appear in the
// alias), so the result is deterministic without sorting. Returns 0
// for empty bindings, matching the "no params" entry shape.
func hashBindings(bindings []router.Param) uint64 {
	if len(bindings) == 0 {
		return 0
	}
	h := fnv.New64a()
	for i := range bindings {
		h.Write([]byte(bindings[i].Name))
		h.Write([]byte{'='})
		h.Write([]byte(bindings[i].Value))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// unionTables returns the de-duplicated union of all tables referenced
// across holes.
//
// Returns nil only if ANY hole has a nil Tables — meaning at least one
// SQL was unattributable and the page must not be cached.
//
// Returns []string{} (empty, non-nil) when every hole has an empty-but-
// valid Tables list (e.g. holes that are param-only or queries that
// genuinely touch no tables). The page is cacheable; no table-write
// signal can invalidate it (data_version flush and SIGHUP still do).
func unionTables(holes []holeRecord) []string {
	for i := range holes {
		if holes[i].Tables == nil {
			return nil
		}
	}
	out := []string{}
	seen := make(map[string]struct{}, 4)
	for i := range holes {
		for _, t := range holes[i].Tables {
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// pickHoleVariant chooses the best variant for the request. Same
// preference order as the static side's pickVariant (brotli > gzip >
// identity). Falls back to identity if the client's preferred encoding
// isn't present.
func pickHoleVariant(e *outputEntry, req *gogohttp.Req) variantBody {
	br, gz := acceptsCompressedEncodings(req)
	if br && len(e.variants[router.EncBrotli].body) > 0 {
		return e.variants[router.EncBrotli]
	}
	if gz && len(e.variants[router.EncGzip].body) > 0 {
		return e.variants[router.EncGzip]
	}
	return e.variants[router.EncIdentity]
}

// acceptsCompressedEncodings parses Accept-Encoding once and returns
// whether the client signalled support for brotli and/or gzip. Same
// header scan pickVariant does — kept in one place so future tweaks
// (zstd, q-values) land in a single spot.
func acceptsCompressedEncodings(req *gogohttp.Req) (br, gz bool) {
	for i := 0; i < req.NHeaders; i++ {
		if !headerNameEqual(req.Headers[i].Name, []byte("Accept-Encoding")) {
			continue
		}
		v := req.Headers[i].Value
		br = containsToken(v, []byte("br"))
		gz = containsToken(v, []byte("gzip"))
		return
	}
	return
}

func headerNameEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func containsToken(v, tok []byte) bool {
	n := len(tok)
	for i := 0; i+n <= len(v); i++ {
		match := true
		for j := 0; j < n; j++ {
			if v[i+j] != tok[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if i > 0 {
			pc := v[i-1]
			if pc != ',' && pc != ' ' && pc != '\t' {
				continue
			}
		}
		if i+n < len(v) {
			nc := v[i+n]
			if nc != ',' && nc != ' ' && nc != '\t' && nc != ';' {
				continue
			}
		}
		return true
	}
	return false
}

// composeHoleBodyCached returns the appropriate encoded body and head
// for the request. The cache key includes a hash of the URL bindings,
// so /users/1 and /users/2 are distinct cache entries — each
// renders-once-serves-forever (until invalidation), bounded by the LRU.
//
// Routes whose holes include unattributable SQL still bypass the
// cache (we can't know which table-write should invalidate them). For
// those, every request renders fresh.
//
// The returned body is owned by the cache. composeHoleBody returns a
// fresh copy independent of any pooled buffer, so caching it directly
// is safe.
func composeHoleBodyCached(st *appState, manifest *router.Manifest, entry *router.RouteEntry, req *gogohttp.Req, bindings []router.Param) ([]byte, uint16, error) {
	parseKey := holeKey{manifest, entry.HoleOffset}
	key := outputKey{manifest: manifest, off: entry.HoleOffset, params: hashBindings(bindings)}

	if st.outputCache != nil {
		if e, hit := st.outputCache.Get(key); hit {
			v := pickHoleVariant(e, req)
			return v.body, v.head, nil
		}
	}

	body, err := composeHoleBody(st, manifest, entry, bindings)
	if err != nil {
		return nil, 0, err
	}

	identityHead := entry.Variants[router.EncIdentity].HeadID
	if st.outputCache == nil {
		return body, identityHead, nil
	}

	holes, ok := st.holeCache[parseKey]
	if !ok {
		return body, identityHead, nil
	}
	tables := unionTables(holes)
	if tables == nil {
		return body, identityHead, nil
	}

	gz := compress.Gzip(body)
	br := compress.Brotli(body)
	e := &outputEntry{
		variants: [3]variantBody{
			router.EncIdentity: {body: body, head: identityHead},
			router.EncGzip:     {body: gz, head: st.holeHeadGzip},
			router.EncBrotli:   {body: br, head: st.holeHeadBrotli},
		},
		tables: tables,
	}
	st.outputCache.Put(key, e)

	v := pickHoleVariant(e, req)
	return v.body, v.head, nil
}

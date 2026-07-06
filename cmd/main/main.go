package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	stdhttp "net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gogogo/modules/actions"
	"gogogo/modules/config"
	"gogogo/modules/db"
	"gogogo/modules/fastdb"
	"gogogo/modules/gogohttp"
	"gogogo/modules/metrics"
	"gogogo/modules/router"
	"gogogo/modules/server"
	"gogogo/modules/studio"
)

// appState holds all sites' manifests plus the shared registry derived
// from their combined mime tables. Each request resolves its host to a
// site manifest, then runs the unified hash → ActionID dispatch on it.
//
// Sharing the registry across sites means a "text/html; charset=utf-8"
// head is one entry, regardless of how many sites use it. Each manifest's
// Variant.HeadID values are remapped at load time to point into the
// combined registry.
//
// headJSON/HTML/Text are app-level HeadIDs reserved for dynamic action
// handlers, registered into the same registry.
type appState struct {
	sites    map[string]*router.Manifest // Host header (lower-cased, port stripped) → manifest
	mainSite *router.Manifest            // fallback when no host matches
	registry [][]byte

	headJSON uint16
	headHTML uint16
	headText uint16

	// fileHeads caches HeadIDs per mime type for ActionFile entries —
	// populated at loadState by scanning ActionFile routes across all
	// sites. Looked up at request time by the file resolver.
	fileHeads map[string]fileHeadIDs

	// holeCache stores parsed hole records per (manifest, holeOffset) so
	// the runtime never re-parses blob bytes on the hot path. Populated
	// at loadState; immutable for the lifetime of this state. Manifest
	// reload builds a fresh appState with a fresh map.
	holeCache map[holeKey][]holeRecord

	// allowHeads maps a route's Methods bitmask to a pre-baked 405
	// response head with the canonical "Allow: GET, POST" header. One
	// entry per unique bitmask across all sites. Looked up at request
	// time when a method check fails.
	allowHeads map[uint16]uint16

	// mwChain holds per-(manifest, route-index) the resolved middleware
	// function list. Populated at loadState by walking each route's
	// declared middleware names and looking them up in middlewareRegistry.
	// nil for routes with no middleware (the common case).
	mwChain map[*router.Manifest][][]actions.Middleware

	// outputCache stores fully-rendered hole-page output, invalidated
	// by fastdb table-write notifications. Cache hits skip SQL queries,
	// per-row template execution, and the splice — handlers just slice
	// the cached bytes into resp.Body.
	//
	// Lifecycle: created per appState; subscription lives on the fastdb
	// pool and is detached when the state is replaced (SIGHUP reload).
	outputCache    *outputCache
	outputCacheSub *fastdb.Subscription

	// holeHeads holds pre-registered HeadIDs for hole-page encodings,
	// one per encoding variant the renderer can produce. Identity is
	// taken from the route entry's manifest variant; gzip and brotli
	// are pre-registered once at loadState since the build doesn't bake
	// compressed variants for hole-bearing routes.
	holeHeadGzip   uint16
	holeHeadBrotli uint16
}

// HeadJSON / HeadHTML / HeadText — implements actions.HandlerCtx so module
// handlers (studio, future modules) can resolve their HeadIDs without
// reaching into appState's unexported fields.
func (s *appState) HeadJSON() uint16 { return s.headJSON }
func (s *appState) HeadHTML() uint16 { return s.headHTML }
func (s *appState) HeadText() uint16 { return s.headText }

// state is the live application state. atomic.Pointer means readers (every
// request) take a single atomic load with no lock contention; reloads
// (SIGHUP) atomically swap a fresh state in.
//
// Old states are not freed/munmapped on swap — in-flight requests may still
// hold slices into the old manifest's mmap region. They become eligible for
// GC once nothing references them, but Go's GC does not unmap mmap pages.
// Address-space "leaks" across many reloads; RSS does not grow because cold
// pages are evictable. Process restart cleans up.
var state atomic.Pointer[appState]

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	cfg, err := config.LoadConfig("web/config.toml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if portStr := os.Getenv("PORT"); portStr != "" {
		fmt.Sscanf(portStr, "%d", &cfg.Server.Port)
	}

	dbPath := filepath.Join(cfg.Directories.Meta, "gogogo.db")
	if err := db.Init(dbPath); err != nil {
		log.Fatalf("Failed to init DB: %v", err)
	}
	// fastdb is the read-only hot path used by hole rendering. Pool size
	// = NumCPU * 4 lets concurrent hole renders proceed without queueing
	// even at high QPS; SQLite WAL allows many parallel readers, and each
	// fastdb conn carries its own libc TLS + stmt cache.
	if err := fastdb.Init(dbPath, runtime.NumCPU()*4); err != nil {
		log.Fatalf("Failed to init fastdb: %v", err)
	}

	// metrics.Init must run before any handler that calls metrics.Get(),
	// since the package singleton is nil until then.
	metrics.Init(runtime.NumCPU())

	studioManifestPath := filepath.Join(cfg.Directories.Meta, "studio_router.bin")

	initial, err := loadState(&cfg, studioManifestPath)
	if err != nil {
		log.Fatalf("Failed to load initial state: %v", err)
	}
	state.Store(initial)
	if initial.mainSite != nil {
		log.Printf("Default site: %d routes", len(initial.mainSite.Table))
	}
	for host, m := range initial.sites {
		log.Printf("Site %q: %d routes", host, len(m.Table))
	}

	srv := &gogohttp.Server{
		Handler:           buildHandler(),
		HeaderRegistry:    initial.registry,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	// SIGHUP triggers a reload — re-read the manifest, rebuild the registry,
	// atomically swap state and registry. If the new manifest fails to load,
	// log and continue serving the old one.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			next, err := loadState(&cfg, studioManifestPath)
			if err != nil {
				log.Printf("Reload failed: %v (keeping previous state)", err)
				continue
			}
			prev := state.Swap(next)
			srv.ReplaceRegistry(next.registry)
			// Detach the previous state's fastdb subscription so its
			// (now-orphaned) outputCache stops receiving invalidation
			// fan-out. In-flight requests holding a pointer into the
			// previous state's cache.body slice still complete safely;
			// nothing mutates those bytes.
			if prev != nil && prev.outputCacheSub != nil {
				prev.outputCacheSub.Close()
			}
			total := 0
			for _, m := range next.sites {
				total += len(m.Table)
			}
			log.Printf("Reloaded: %d sites, %d routes total", len(next.sites), total)
		}
	}()

	// pprof side-channel on :6060. net/http/pprof registers handlers on
	// the default mux at import time. Off-port so it never interferes
	// with the main hot path.
	go func() {
		log.Println("pprof on http://localhost:6060/debug/pprof/")
		if err := stdhttp.ListenAndServe("localhost:6060", nil); err != nil {
			log.Printf("pprof listener: %v", err)
		}
	}()

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	numBouncers := runtime.NumCPU()
	fmt.Printf("Gogogo Extreme: Spawning %d SO_REUSEPORT bouncers on %s\n", numBouncers, addr)
	log.Printf("Server starting on %s — send SIGHUP to reload manifest\n", addr)

	listeners := make([]net.Listener, numBouncers)
	for i := 0; i < numBouncers; i++ {
		ln, err := server.NewReusePortListener("tcp", addr)
		if err != nil {
			log.Fatalf("listener %d: %v", i, err)
		}
		listeners[i] = ln
		go srv.Serve(ln)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Server is shutting down...")
	for _, ln := range listeners {
		ln.Close()
	}
	srv.Shutdown()
	log.Println("Server stopped.")
}

// loadState walks cfg.Sites + studio's manifest, remaps every site's
// HeadIDs into a single combined registry, and returns a fresh appState.
// On any failure the existing state is left untouched (atomic-pointer
// swap is the caller's job; loadState only constructs).
//
// A site listed with host "*" becomes the default fallback used when no
// other host matches. Exactly one site should claim "*"; if multiple do,
// the last wins. If none do, the first site in cfg.Sites is treated as
// default.
//
// Per-site manifest path: meta/<site.Name>_router.bin. The studio
// manifest is loaded separately because studio is module-managed
// (modules/studio); its manifest path is passed in.
func loadState(cfg *config.Config, studioManifestPath string) (*appState, error) {
	if len(cfg.Sites) == 0 {
		return nil, fmt.Errorf("config has no [[site]] entries")
	}

	rb := gogohttp.NewRegistryBuilder()

	remap := func(m *router.Manifest) {
		mimeRemap := make([]uint16, len(m.MimeTable))
		for i, key := range m.MimeTable {
			mime, enc := splitMimeKey(key)
			mimeRemap[i] = rb.AddMimeWithEncoding(mime, enc)
		}
		for ti := range m.Table {
			for vi := range m.Table[ti].Variants {
				v := &m.Table[ti].Variants[vi]
				if v.DataLen > 0 {
					v.HeadID = mimeRemap[v.HeadID]
				}
			}
		}
	}

	sites := map[string]*router.Manifest{}
	var mainSite *router.Manifest
	allManifests := []*router.Manifest{}

	for _, siteCfg := range cfg.Sites {
		manifestPath := filepath.Join(cfg.Directories.Meta, siteCfg.Name+"_router.bin")
		m, err := router.LoadV3(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("load site %q: %w", siteCfg.Name, err)
		}
		remap(m)
		allManifests = append(allManifests, m)

		isDefault := false
		for _, host := range siteCfg.Hosts {
			if host == "*" {
				isDefault = true
				continue
			}
			sites[host] = m
		}
		if isDefault || mainSite == nil {
			mainSite = m
		}
	}

	studioManifest, sErr := router.LoadV3(studioManifestPath)
	if sErr != nil {
		log.Printf("Studio manifest not loaded (%v) — admin site disabled until next build", sErr)
	} else {
		remap(studioManifest)
		sites[studio.Host] = studioManifest
		allManifests = append(allManifests, studioManifest)
	}

	headJSON := rb.AddMime("application/json")
	headHTML := rb.AddMime("text/html; charset=utf-8")
	headText := rb.AddMime("text/plain; charset=utf-8")

	// Hole pages render text/html and don't have build-time compressed
	// variants (compression happens lazily on first cache miss). Pre-
	// register gzip+brotli heads here so the output cache has stable
	// IDs to attach to its compressed variant bodies.
	holeHeadGzip := rb.AddMimeWithEncoding("text/html; charset=utf-8", "gzip")
	holeHeadBrotli := rb.AddMimeWithEncoding("text/html; charset=utf-8", "br")

	st := &appState{
		sites:          sites,
		mainSite:       mainSite,
		headJSON:       headJSON,
		headHTML:       headHTML,
		headText:       headText,
		holeHeadGzip:   holeHeadGzip,
		holeHeadBrotli: holeHeadBrotli,
		fileHeads:      map[string]fileHeadIDs{},
		holeCache:      map[holeKey][]holeRecord{},
		allowHeads:     map[uint16]uint16{},
		mwChain:        map[*router.Manifest][][]actions.Middleware{},
		outputCache:    newOutputCache(),
	}
	// Subscribe to fastdb write events so the output cache evicts pages
	// touching modified tables. The subscription is detached when this
	// state is replaced (see swapState in the reload path).
	st.outputCacheSub = fastdb.Subscribe(st.outputCache.Invalidate)

	// Pre-bake one 405 response head per unique Methods bitmask seen in
	// any route. The head carries the canonical "Allow: <list>" header.
	// At request time we just look up by the route's bitmask — no string
	// assembly on the hot path.
	for _, m := range allManifests {
		for ti := range m.Table {
			bits := m.Table[ti].Methods
			if _, ok := st.allowHeads[bits]; ok {
				continue
			}
			head := []byte("HTTP/1.1 405 Method Not Allowed\r\nServer: Gogogo\r\nAllow: " +
				router.AllowHeaderValue(bits) + "\r\nContent-Type: text/plain; charset=utf-8\r\n")
			st.allowHeads[bits] = rb.Add(head)
		}
	}

	// Resolve per-route middleware names to function pointers. One pass
	// per manifest at load; runtime dispatch then just walks a slice of
	// function pointers. Unknown names fail load — better than silent
	// no-op at request time.
	for _, m := range allManifests {
		chains := make([][]actions.Middleware, len(m.Table))
		for ti := range m.Table {
			e := &m.Table[ti]
			if e.MwCount == 0 {
				continue
			}
			names := m.MiddlewareNamesAt(e.MwOffset, e.MwCount)
			fns := make([]actions.Middleware, 0, len(names))
			for _, name := range names {
				fn, ok := actions.MiddlewareRegistry[name]
				if !ok {
					return nil, fmt.Errorf("route %q references unregistered middleware %q",
						string(m.Paths[e.PathOffset:e.PathOffset+uint32(e.PathLen)]), name)
				}
				fns = append(fns, fn)
			}
			chains[ti] = fns
		}
		st.mwChain[m] = chains
	}

	// Walk every site's table once for two pre-builds:
	//   - ActionFile: register mime heads (200 + 206) so the file handler
	//     can resolve them with one map lookup.
	//   - ActionStatic with HoleOffset != 0: parse the hole blob and cache
	//     the parsed records so composeHoleBody never re-parses on the hot
	//     path. Pongo2 sub-templates are also already memoized inside
	//     getOrParseTemplate so a manifest reload re-uses identical bodies.
	for _, m := range allManifests {
		for ti := range m.Table {
			e := &m.Table[ti]
			switch e.ActionID {
			case actions.ActionFile:
				path, err := readFileBlob(m.HoleBlobs[e.HoleOffset:])
				if err != nil {
					continue
				}
				st.registerFileMime(mimeForFile(path), rb)
			case actions.ActionStatic:
				if e.HoleOffset == 0 {
					continue
				}
				holes, err := readHoles(m.HoleBlobs[e.HoleOffset:])
				if err != nil {
					log.Printf("hole pre-parse failed for route hash %x: %v", e.PathHash, err)
					continue
				}
				st.holeCache[holeKey{m, e.HoleOffset}] = holes
			}
		}
	}

	st.registry = rb.Build()
	return st, nil
}

// actionTable maps ActionID → handler. ActionID 0 (ActionStatic) is handled
// inline in the resolver and never indexed here. App-level entries are
// declared inline; module-owned entries are merged in via init() so each
// module owns its own routes + handlers and stays self-contained.
var actionTable [256]actions.Func

func init() {
	actionTable[actions.ActionMetrics] = actionMetrics
	for id, fn := range studio.Handlers {
		actionTable[id] = fn
	}
}

// paramSlabPool supplies the per-request scratch slice for the trie's
// param captures. Reused across requests so the trie walker doesn't
// allocate for the typical 0–2 param case.
var paramSlabPool = sync.Pool{
	New: func() any { s := make([]router.Param, 0, 4); return &s },
}

// routeCtxPool supplies the per-request RouteCtx that handlers receive.
// One alloc per pool-miss; reused indefinitely. Bindings slice is reused
// across requests too (the trie walker fills the inner slab from
// paramSlabPool, the RouteCtx just points at it).
var routeCtxPool = sync.Pool{
	New: func() any { return &actions.RouteCtx{} },
}

// buildHandler returns the unified resolver. Per-request flow:
//
//   1. Strip port from Host header → site lookup. If no match, fall back
//      to the main site. (One Go map lookup; site selection is per-request
//      so SIGHUP can swap sites in/out atomically.)
//   2. Walk the manifest's trie with the URL path. Captured params land
//      in a pooled slab. -1 → 404.
//   3. Method check against the route's Methods bitmask. Mismatch → 405
//      with a pre-baked Allow header.
//   4. Run any middleware chain in declaration order; short-circuit on
//      false.
//   5. Dispatch on ActionID — ActionStatic serves the baked variant;
//      ActionFile sendfile-streams; otherwise actionTable[id] runs.
//
// Pool releases (params slab + RouteCtx) happen via defer so every
// return path stays single-line.
func buildHandler() gogohttp.Handler {
	return gogohttp.HandlerFunc(func(req *gogohttp.Req, resp *gogohttp.Resp) {
		st := state.Load()
		st.serve(req, resp)
	})
}

func (st *appState) serve(req *gogohttp.Req, resp *gogohttp.Resp) {
	manifest := st.mainSite
	if len(st.sites) > 0 && len(req.Host) > 0 {
		host := stripPort(req.Host)
		if m, ok := st.sites[string(host)]; ok {
			manifest = m
		}
	}

	paramsPtr := paramSlabPool.Get().(*[]router.Param)
	params := (*paramsPtr)[:0]
	defer func() {
		*paramsPtr = params
		paramSlabPool.Put(paramsPtr)
	}()

	idx := manifest.Trie.Match(string(req.Path), &params)
	if idx < 0 {
		resp.NotFound()
		return
	}
	entry := &manifest.Table[idx]

	// Method check.
	if entry.Methods&methodBitFor(req.Method) == 0 {
		resp.HeadID = st.allowHeads[entry.Methods]
		resp.Body = []byte("Method Not Allowed\n")
		return
	}

	// Resolve the per-route middleware chain (nil if none).
	var chain []actions.Middleware
	if entry.MwCount > 0 {
		if chains := st.mwChain[manifest]; chains != nil && idx < len(chains) {
			chain = chains[idx]
		}
	}

	// RouteCtx is needed if any middleware runs or if this is an action
	// handler. Static / file paths skip the alloc.
	needsRC := len(chain) > 0 ||
		(entry.ActionID != actions.ActionStatic && entry.ActionID != actions.ActionFile)
	var rc *actions.RouteCtx
	if needsRC {
		rc = routeCtxPool.Get().(*actions.RouteCtx)
		rc.Req, rc.Resp, rc.App, rc.Bindings = req, resp, st, params
		defer func() {
			rc.Req, rc.Resp, rc.App, rc.Bindings = nil, nil, nil, nil
			routeCtxPool.Put(rc)
		}()
	}

	// Middleware chain.
	for _, mw := range chain {
		if !mw(rc) {
			return
		}
	}

	// Dispatch.
	switch entry.ActionID {
	case actions.ActionStatic:
		if entry.HoleOffset != 0 {
			body, head, err := composeHoleBodyCached(st, manifest, entry, req, params)
			if err != nil {
				log.Printf("hole render failed for %q: %v", string(req.Path), err)
				resp.ServerError()
				return
			}
			resp.HeadID = head
			resp.Body = body
			return
		}
		v := pickVariant(entry, req)
		resp.HeadID = v.HeadID
		resp.Body = manifest.Payloads[v.DataOffset : v.DataOffset+v.DataLen]

	case actions.ActionFile:
		serveFile(req, resp, entry, manifest, st)

	default:
		if int(entry.ActionID) < len(actionTable) && actionTable[entry.ActionID] != nil {
			actionTable[entry.ActionID](rc)
			return
		}
		resp.ServerError()
	}
}

// methodBitFor maps a parsed gogohttp.Method to the corresponding bit
// in router.RouteEntry.Methods. Inline switch — branch-predicted, ~1ns.
func methodBitFor(m gogohttp.Method) uint16 {
	switch m {
	case gogohttp.MethodGET:
		return router.MethodBitGET
	case gogohttp.MethodHEAD:
		return router.MethodBitHEAD
	case gogohttp.MethodPOST:
		return router.MethodBitPOST
	case gogohttp.MethodPUT:
		return router.MethodBitPUT
	case gogohttp.MethodPATCH:
		return router.MethodBitPATCH
	case gogohttp.MethodDELETE:
		return router.MethodBitDELETE
	case gogohttp.MethodOPTIONS:
		return router.MethodBitOPTIONS
	case gogohttp.MethodCONNECT:
		return router.MethodBitCONNECT
	case gogohttp.MethodTRACE:
		return router.MethodBitTRACE
	}
	return 0
}

// stripPort returns host bytes with any trailing ":port" removed.
// Lower-cases ASCII letters so site lookups are host-case-insensitive.
func stripPort(host []byte) []byte {
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			host = host[:i]
			break
		}
	}
	out := make([]byte, len(host))
	for i, c := range host {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// Compile-time check: appState must implement actions.HandlerCtx so module
// handlers can take the interface and not import this package back.
var _ actions.HandlerCtx = (*appState)(nil)

// pickVariant chooses the best-encoded variant the client supports. Order
// of preference: brotli > gzip > identity.
func pickVariant(entry *router.RouteEntry, req *gogohttp.Req) *router.Variant {
	for i := 0; i < req.NHeaders; i++ {
		if !bytes.EqualFold(req.Headers[i].Name, []byte("Accept-Encoding")) {
			continue
		}
		v := req.Headers[i].Value
		if entry.Variants[router.EncBrotli].DataLen > 0 && bytes.Contains(v, []byte("br")) {
			return &entry.Variants[router.EncBrotli]
		}
		if entry.Variants[router.EncGzip].DataLen > 0 && bytes.Contains(v, []byte("gzip")) {
			return &entry.Variants[router.EncGzip]
		}
		break
	}
	return &entry.Variants[router.EncIdentity]
}

func splitMimeKey(key string) (string, string) {
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

func trimSlashes(b []byte) []byte {
	for len(b) > 0 && b[0] == '/' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == '/' {
		b = b[:len(b)-1]
	}
	return b
}

func hashPath(path []byte) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= 1099511628211
	}
	return h
}

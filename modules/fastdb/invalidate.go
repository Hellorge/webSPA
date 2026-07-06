package fastdb

import (
	"strings"
	"sync"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// updateHookHandlers maps each connection's db handle to the Pool that
// owns it. The trampoline below uses this to find both the Pool's cache
// (to invalidate) and its subscriber list (to broadcast). Lookups happen
// from a C-ABI callback, so the map lives at package scope and is
// guarded by a read-mostly RWMutex.
var updateHookHandlers = struct {
	mu sync.RWMutex
	m  map[uintptr]*Pool
}{m: make(map[uintptr]*Pool)}

// registerUpdateHook wires sqlite3_update_hook on conn so writes through
// it both invalidate the pool's cache AND fire any subscribers. The hook
// fires once per row INSERT/UPDATE/DELETE, after the change is applied
// but before sqlite3_step returns to the caller — so writes are
// reflected in cache (and visible to subscribers) before the calling
// goroutine sees the write's return.
func registerUpdateHook(conn *Conn, pool *Pool) {
	updateHookHandlers.mu.Lock()
	updateHookHandlers.m[conn.db] = pool
	updateHookHandlers.mu.Unlock()
	sqlite3.Xsqlite3_update_hook(conn.tls, conn.db, cFuncPointer(updateHookTrampoline), conn.db)
}

// unregisterUpdateHook drops the conn's mapping and clears the SQLite-
// side callback. Called from Conn.close so the package map doesn't leak
// stale db handles after pool teardown.
func unregisterUpdateHook(conn *Conn) {
	updateHookHandlers.mu.Lock()
	delete(updateHookHandlers.m, conn.db)
	updateHookHandlers.mu.Unlock()
	sqlite3.Xsqlite3_update_hook(conn.tls, conn.db, 0, 0)
}

// updateHookTrampoline matches the C ABI of SQLite's xCallback for
// sqlite3_update_hook:
//
//	void (*xCallback)(void *pArg, int op,
//	                  char const *zDb, char const *zTab,
//	                  sqlite3_int64 rowid)
//
// In ccgo's synthesised signature (linux_amd64) the callback type is
// `func(*libc.TLS, uintptr, int32, uintptr, uintptr, int64)`. pArg is
// the db handle we passed at registration; we use it to find the Pool.
// op/rowid are unused — we invalidate at the table level, not the row
// level, so INSERT/UPDATE/DELETE all flow through the same path.
func updateHookTrampoline(tls *libc.TLS, pArg uintptr, op int32, zDb uintptr, zTab uintptr, rowid int64) {
	_ = tls
	_ = op
	_ = zDb
	_ = rowid
	updateHookHandlers.mu.RLock()
	pool := updateHookHandlers.m[pArg]
	updateHookHandlers.mu.RUnlock()
	if pool == nil {
		return
	}
	table := libc.GoString(zTab)
	if pool.cache != nil {
		pool.cache.invalidateTable(table)
	}
	pool.notifySubscribers(table)
}

// cFuncPointer converts a top-level Go function to a C function pointer
// (uintptr) suitable for passing into sqlite3_update_hook. Lifted from
// modernc.org/sqlite — the trick relies on Go's stable in-memory layout
// for function values described at https://golang.org/s/go11func.
//
// Closures are NOT safe to pass through this — pass package-level funcs
// only.
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f}))
}

// TablesIn returns the table names a SELECT statement reads from. Same
// semantics and limitations as the internal table extractor used by the
// cache — nil for queries too complex to parse safely (subqueries in
// FROM, CTEs, quoted-identifier tables). Exposed for downstream code
// (the renderer's output cache) that needs the same attribution.
func TablesIn(sql string) []string { return extractTables(sql) }

// extractTables scans sql for every `FROM <ident>` and `JOIN <ident>`
// position and returns the unique table names. Schema-qualified names
// (`main.news`, `attached.t`) have the schema stripped — invalidation
// matches the bare name SQLite passes to update_hook.
//
// Subqueries in FROM (e.g. `FROM (SELECT ...) sub`) and CTEs are
// detected as too complex and cause the function to return nil — the
// caller treats nil as "don't cache" rather than risk invalidating the
// wrong (or no) set on writes.
//
// Quoted identifiers (`"weird name"`, `[bracket]`) and identifiers
// containing dots beyond `schema.table` are not handled; those queries
// fall through to nil too. Common SELECT shapes work without quoting.
func extractTables(sql string) []string {
	if containsCTE(sql) {
		return nil
	}
	s := strings.ToLower(sql)
	var out []string
	i := 0
	for i < len(s) {
		idx, kwLen := findFromOrJoin(s, i)
		if idx < 0 {
			break
		}
		i = idx + kwLen
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		// Subquery — bail.
		if s[i] == '(' {
			return nil
		}
		// Quoted identifier — bail (we don't parse quotes).
		if s[i] == '"' || s[i] == '[' || s[i] == '`' {
			return nil
		}
		start := i
		for i < len(s) && isIdentByte(s[i]) {
			i++
		}
		if i == start {
			continue
		}
		name := sql[start:i]
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[dot+1:]
		}
		if name != "" && !containsString(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// containsCTE looks for a leading WITH clause. CTEs require dataflow
// analysis to know which underlying tables they read; we don't do that,
// so we bail and skip caching.
func containsCTE(sql string) bool {
	s := strings.TrimLeft(sql, " \t\n\r")
	if len(s) < 5 {
		return false
	}
	return strings.EqualFold(s[:5], "with ") || strings.EqualFold(s[:5], "with\t") ||
		strings.EqualFold(s[:5], "with\n")
}

// findFromOrJoin returns the index of the next standalone "from" or
// "join" keyword in s starting at offset off, plus the keyword's length.
// "Standalone" means the keyword is bounded on both sides by either
// whitespace, string-start, or string-end — not part of a larger
// identifier like `transform` or `joined`.
func findFromOrJoin(s string, off int) (int, int) {
	a, alen := nextKeyword(s, off, "from")
	b, blen := nextKeyword(s, off, "join")
	switch {
	case a < 0 && b < 0:
		return -1, 0
	case a < 0:
		return b, blen
	case b < 0:
		return a, alen
	case a < b:
		return a, alen
	default:
		return b, blen
	}
}

// nextKeyword finds the next standalone occurrence of word in s at or
// after off. Returns (index, len(word)) or (-1, 0).
func nextKeyword(s string, off int, word string) (int, int) {
	n := len(word)
	for i := off; i <= len(s)-n; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return -1, 0
		}
		k := i + j
		if k > 0 && isIdentByte(s[k-1]) {
			i = k + 1
			continue
		}
		if k+n < len(s) && isIdentByte(s[k+n]) {
			i = k + 1
			continue
		}
		return k, n
	}
	return -1, 0
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func containsString(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}

// dataVersionWatcher periodically reads `PRAGMA data_version` on a
// pool connection. The pragma's value increments whenever the database
// is modified by ANY connection other than the one running the pragma —
// so it catches writes that bypass our update_hook entirely (writes via
// modules/db, writes from a `sqlite3` CLI session, writes from any
// other process attached to the same file).
//
// On a version bump, the cache is flushed wholesale. This is a safety
// net, not the primary invalidation mechanism. Polling at 1 Hz means up
// to ~1 second of staleness for external writes — acceptable for our
// use cases; tunable per-pool if not.
type dataVersionWatcher struct {
	pool   *Pool
	cache  *Cache
	period time.Duration
	stop   chan struct{}
	done   chan struct{}
}

func newDataVersionWatcher(p *Pool, c *Cache, period time.Duration) *dataVersionWatcher {
	if period <= 0 {
		period = time.Second
	}
	return &dataVersionWatcher{
		pool:   p,
		cache:  c,
		period: period,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

func (w *dataVersionWatcher) start() { go w.run() }

func (w *dataVersionWatcher) Stop() {
	close(w.stop)
	<-w.done
}

func (w *dataVersionWatcher) run() {
	defer close(w.done)
	t := time.NewTicker(w.period)
	defer t.Stop()

	var lastVer int64 = -1
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			ver, ok := w.readDataVersion()
			if !ok {
				continue
			}
			if lastVer != -1 && ver != lastVer {
				// External write detected. Flush the SQL cache and
				// notify subscribers — table == "" signals "all
				// tables, treat conservatively."
				w.cache.FlushAll()
				w.pool.notifySubscribers("")
			}
			lastVer = ver
		}
	}
}

func (w *dataVersionWatcher) readDataVersion() (int64, bool) {
	var ver int64
	var found bool
	_, err := w.pool.queryDirect("PRAGMA data_version", func(row map[string]any) error {
		if v, ok := row["data_version"].(int64); ok {
			ver = v
			found = true
		}
		return nil
	})
	if err != nil {
		return 0, false
	}
	return ver, found
}

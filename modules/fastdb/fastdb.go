// Package fastdb is a minimal SQLite read layer for the hole-render hot
// path. It calls modernc.org/sqlite/lib directly — no database/sql,
// no driver wrapping, no per-request prepare. Connections are pooled,
// prepared statements are cached per-conn, and the per-row scan visits
// a reusable map without allocating slabs each iteration.
//
// Scope is intentionally narrow: read-only SELECTs returning text/int
// columns. Writes still go through modules/db (the database/sql path).
// Both stacks share the same physical sqlite database file in WAL mode.
package fastdb

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// SQLite return codes. Mirror modernc/sqlite/lib's exports so callers
	// don't need to import lib directly.
	rowCode  = sqlite3.SQLITE_ROW
	doneCode = sqlite3.SQLITE_DONE
	okCode   = sqlite3.SQLITE_OK
)

// Conn wraps a single sqlite3 db handle plus its libc thread-local
// state and a cache of prepared statements keyed by SQL text.
//
// SQLite stmts are not safe for concurrent use across goroutines; one
// Conn is exclusive to its current borrower. The Pool serializes that.
type Conn struct {
	db    uintptr
	tls   *libc.TLS
	stmts map[string]*Stmt
}

// Stmt is a prepared sqlite3_stmt with its column metadata cached.
// Always belongs to one Conn; never used outside that Conn's borrower.
type Stmt struct {
	pstmt uintptr
	cols  []string
	conn  *Conn
}

// Pool is a fixed-size pool of Conns. Each goroutine acquires one,
// runs its query, and returns it. Size should match expected concurrent
// hole-render load (start with 2× NumCPU).
//
// A Pool owns a Cache (query-result cache with table-level invalidation)
// and a data_version watcher (safety net for out-of-pool writes). Both
// are created in NewPool and torn down in Close.
//
// Pools also broadcast write events to Subscribe()d callbacks — the
// renderer's output cache uses this to invalidate its own state when a
// table fastdb wrote is one its rendered pages depend on. See
// subscribe.go for the subscription machinery; the subs/subMu fields
// live on Pool so both files can manipulate them.
type Pool struct {
	conns   chan *Conn
	size    int
	cache   *Cache
	watcher *dataVersionWatcher

	subMu sync.Mutex
	subs  atomic.Pointer[[]*Subscription]
}

var (
	defaultPool *Pool
	initOnce    sync.Once
	initErr     error
)

// Init opens `size` connections to dbPath in WAL mode. Subsequent calls
// to QueryEach use the package-level pool. Idempotent — second call is
// a no-op.
func Init(dbPath string, size int) error {
	initOnce.Do(func() {
		p, err := NewPool(dbPath, size)
		if err != nil {
			initErr = err
			return
		}
		defaultPool = p
	})
	return initErr
}

// NewPool opens a fresh pool. Init uses this internally; exposed for
// tests and for the rare caller wanting their own pool.
//
// Every connection in the pool gets sqlite3_update_hook registered so
// writes through any of them invalidate the cache. The data_version
// watcher runs on top as a safety net for writes that bypass the pool
// entirely.
func NewPool(dbPath string, size int) (*Pool, error) {
	if size <= 0 {
		size = 8
	}
	p := &Pool{
		conns: make(chan *Conn, size),
		size:  size,
		cache: NewCache(CacheConfig{}),
	}
	for i := 0; i < size; i++ {
		c, err := openConn(dbPath)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("fastdb: open conn %d: %w", i, err)
		}
		registerUpdateHook(c, p)
		p.conns <- c
	}
	p.watcher = newDataVersionWatcher(p, p.cache, time.Second)
	p.watcher.start()
	return p, nil
}

// Cache returns the pool's underlying cache. Exposed for callers that
// want to surface stats or trigger an explicit flush (e.g. on SIGHUP).
func (p *Pool) Cache() *Cache { return p.cache }

// Close finalizes every cached stmt and closes every conn in the pool.
// Idempotent if called once; subsequent calls no-op on an empty pool.
func (p *Pool) Close() {
	if p.watcher != nil {
		p.watcher.Stop()
		p.watcher = nil
	}
	for {
		select {
		case c := <-p.conns:
			unregisterUpdateHook(c)
			c.close()
		default:
			return
		}
	}
}

func openConn(dbPath string) (*Conn, error) {
	tls := libc.NewTLS()

	// Allocate space for the sqlite3* output param.
	ppDb := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(uintptr(0))))
	if ppDb == 0 {
		tls.Close()
		return nil, errors.New("fastdb: malloc ppDb failed")
	}
	defer libc.Xfree(tls, ppDb)

	cPath, err := libc.CString(dbPath)
	if err != nil {
		tls.Close()
		return nil, fmt.Errorf("fastdb: CString path: %w", err)
	}
	defer libc.Xfree(tls, cPath)

	flags := int32(sqlite3.SQLITE_OPEN_READWRITE | sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_NOMUTEX)
	if rc := sqlite3.Xsqlite3_open_v2(tls, cPath, ppDb, flags, 0); rc != okCode {
		tls.Close()
		return nil, fmt.Errorf("fastdb: open_v2 rc=%d", rc)
	}

	db := *(*uintptr)(unsafe.Pointer(ppDb))

	c := &Conn{db: db, tls: tls, stmts: make(map[string]*Stmt, 8)}

	// Pragmas: WAL mode + balanced sync. Same shape as the database/sql
	// path (?_journal_mode=WAL&_synchronous=NORMAL).
	if err := c.exec("PRAGMA journal_mode=WAL"); err != nil {
		c.close()
		return nil, fmt.Errorf("fastdb: pragma WAL: %w", err)
	}
	if err := c.exec("PRAGMA synchronous=NORMAL"); err != nil {
		c.close()
		return nil, fmt.Errorf("fastdb: pragma synchronous: %w", err)
	}

	return c, nil
}

// exec runs a SQL statement with no result rows. Used for pragma setup.
func (c *Conn) exec(sql string) error {
	stmt, err := c.prepare(sql)
	if err != nil {
		return err
	}
	defer stmt.reset()
	for {
		rc := sqlite3.Xsqlite3_step(c.tls, stmt.pstmt)
		if rc == doneCode {
			return nil
		}
		if rc != rowCode {
			return fmt.Errorf("fastdb: step rc=%d", rc)
		}
	}
}

// prepare returns a cached Stmt for sql, preparing on first use.
// The Stmt is owned by the Conn; never use it from another goroutine
// while the Conn is borrowed by the current one.
func (c *Conn) prepare(sql string) (*Stmt, error) {
	if s, ok := c.stmts[sql]; ok {
		return s, nil
	}

	cSQL, err := libc.CString(sql)
	if err != nil {
		return nil, fmt.Errorf("fastdb: CString SQL: %w", err)
	}
	defer libc.Xfree(c.tls, cSQL)

	ppStmt := libc.Xmalloc(c.tls, libc.Tsize_t(unsafe.Sizeof(uintptr(0))))
	if ppStmt == 0 {
		return nil, errors.New("fastdb: malloc ppStmt failed")
	}
	defer libc.Xfree(c.tls, ppStmt)

	if rc := sqlite3.Xsqlite3_prepare_v2(c.tls, c.db, cSQL, -1, ppStmt, 0); rc != okCode {
		return nil, fmt.Errorf("fastdb: prepare rc=%d sql=%q", rc, sql)
	}

	pstmt := *(*uintptr)(unsafe.Pointer(ppStmt))
	if pstmt == 0 {
		return nil, fmt.Errorf("fastdb: prepare returned NULL stmt for sql=%q", sql)
	}

	// Cache column names — they don't change per execution and the
	// per-row visit() needs to populate the map by name.
	n := int(sqlite3.Xsqlite3_column_count(c.tls, pstmt))
	cols := make([]string, n)
	for i := 0; i < n; i++ {
		nameP := sqlite3.Xsqlite3_column_name(c.tls, pstmt, int32(i))
		cols[i] = libc.GoString(nameP)
	}

	s := &Stmt{pstmt: pstmt, cols: cols, conn: c}
	c.stmts[sql] = s
	return s, nil
}

func (s *Stmt) reset() {
	sqlite3.Xsqlite3_reset(s.conn.tls, s.pstmt)
	sqlite3.Xsqlite3_clear_bindings(s.conn.tls, s.pstmt)
}

func (c *Conn) close() {
	for _, s := range c.stmts {
		sqlite3.Xsqlite3_finalize(c.tls, s.pstmt)
	}
	if c.db != 0 {
		sqlite3.Xsqlite3_close_v2(c.tls, c.db)
	}
	if c.tls != nil {
		c.tls.Close()
	}
}

// QueryEach acquires a conn from the default pool, prepares (or reuses)
// the statement for query, and visits every row. The map handed to
// visit is reused across rows — visit must read what it needs before
// returning. The map's keys are column names; values are Go strings for
// TEXT, int64 for INTEGER, float64 for REAL, []byte for BLOB, or nil
// for NULL.
//
// Returns the number of rows visited and any error encountered.
func QueryEach(query string, visit func(row map[string]any) error) (int, error) {
	if defaultPool == nil {
		return 0, errors.New("fastdb: not initialized — call fastdb.Init first")
	}
	return defaultPool.QueryEach(query, visit)
}

// QueryEach is the pool-method form. Cache-aware: checks the per-pool
// query cache before hitting SQLite, and populates the cache on miss.
// Queries whose table set can't be safely extracted (subqueries, CTEs,
// quoted-identifier tables) skip the cache entirely.
func (p *Pool) QueryEach(query string, visit func(row map[string]any) error) (int, error) {
	if p.cache == nil {
		return p.queryDirect(query, visit)
	}
	tables := p.cache.tablesFor(query)
	if tables == nil {
		// SQL shape too complex to attribute writes to — never cache.
		return p.queryDirect(query, visit)
	}
	if rows, hit := p.cache.Get(query, tables); hit {
		for i := range rows {
			if err := visit(rows[i]); err != nil {
				return i, err
			}
		}
		return len(rows), nil
	}
	// Miss: run SQLite, snapshot each row into a fresh map (so cache
	// retains stable copies separate from the reused rowMap), call
	// visit on the snapshot, then Put on completion.
	var collected []map[string]any
	n, err := p.queryDirect(query, func(row map[string]any) error {
		cp := make(map[string]any, len(row))
		for k, v := range row {
			cp[k] = v
		}
		collected = append(collected, cp)
		return visit(cp)
	})
	if err == nil {
		p.cache.Put(query, tables, collected)
	}
	return n, err
}

// queryDirect runs the query against SQLite without consulting the
// cache. Used internally by QueryEach (the miss path) and the data
// version watcher (which would loop if it cached the pragma read).
func (p *Pool) queryDirect(query string, visit func(row map[string]any) error) (int, error) {
	c := <-p.conns
	defer func() { p.conns <- c }()

	stmt, err := c.prepare(query)
	if err != nil {
		return 0, err
	}
	defer stmt.reset()

	tls := c.tls
	pstmt := stmt.pstmt
	cols := stmt.cols
	rowMap := getRowMap()
	defer putRowMap(rowMap)

	// Clear without realloc.
	for k := range rowMap {
		delete(rowMap, k)
	}

	rowsVisited := 0
	for {
		rc := sqlite3.Xsqlite3_step(tls, pstmt)
		if rc == doneCode {
			return rowsVisited, nil
		}
		if rc != rowCode {
			return rowsVisited, fmt.Errorf("fastdb: step rc=%d", rc)
		}

		// Materialize the row into rowMap. Type dispatch by column type
		// — text/int/real/blob/null.
		for i, name := range cols {
			ct := sqlite3.Xsqlite3_column_type(tls, pstmt, int32(i))
			switch ct {
			case sqlite3.SQLITE_TEXT:
				p := sqlite3.Xsqlite3_column_text(tls, pstmt, int32(i))
				rowMap[name] = libc.GoString(p)
			case sqlite3.SQLITE_INTEGER:
				rowMap[name] = int64(sqlite3.Xsqlite3_column_int64(tls, pstmt, int32(i)))
			case sqlite3.SQLITE_FLOAT:
				rowMap[name] = float64(sqlite3.Xsqlite3_column_double(tls, pstmt, int32(i)))
			case sqlite3.SQLITE_BLOB:
				n := sqlite3.Xsqlite3_column_bytes(tls, pstmt, int32(i))
				p := sqlite3.Xsqlite3_column_blob(tls, pstmt, int32(i))
				rowMap[name] = libc.GoBytes(p, int(n))
			default: // NULL
				rowMap[name] = nil
			}
		}

		if err := visit(rowMap); err != nil {
			return rowsVisited, err
		}
		rowsVisited++
	}
}

// rowMap pool. Reusing the map across requests dodges the bucket alloc
// cost; rowMap is cleared at the start of each query.
var rowMapPool = sync.Pool{
	New: func() any { return make(map[string]any, 8) },
}

func getRowMap() map[string]any { return rowMapPool.Get().(map[string]any) }
func putRowMap(m map[string]any) { rowMapPool.Put(m) }

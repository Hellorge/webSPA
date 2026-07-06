package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

var instance *sql.DB

// stmtCache memoizes *sql.Stmt by query text. modernc.org/sqlite's
// database/sql wrapper re-prepares on every Query() call — the prepare
// step alone was ~17% of CPU under hole-page load. Caching wins the bulk
// of that back; ratio of distinct SQLs to requests is tiny so this is a
// pure positive trade.
var stmtCache = struct {
	sync.RWMutex
	m map[string]*sql.Stmt
}{m: map[string]*sql.Stmt{}}

// Init initializes the Absolute Performance SQLite pool.
func Init(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.MkdirAll(dir, 0755)
	}

	// 1. Open Connection with High-Performance Options
	// _journal_mode=WAL: Absolute Concurrency (multi-reader, 1-writer)
	// _synchronous=NORMAL: Balanced Speed/Safety
	// _cache_size=-2000: 2MB Cache
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=-2000", dbPath)
	
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// 2. Configure Pool for Mechanical Sympathy
	// SQLite WAL allows many concurrent readers. database/sql serializes
	// all calls per *sql.Conn through a mutex, so the pool size *is* the
	// max concurrent SQL execution. 10 was an arbitrary low default; set
	// it to NumCPU*2 so hole-rendering at high QPS doesn't queue on conns.
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(64)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	instance = db
	fmt.Println("Gogogo DB: SQLite WAL Mode Initialized.")
	return nil
}

// Get returns the global database instance.
func Get() *sql.DB {
	return instance
}

// SQL Query helper for bone-dry logic.
func Query(query string, args ...interface{}) (*sql.Rows, error) {
	return instance.Query(query, args...)
}

func Exec(query string, args ...interface{}) (sql.Result, error) {
	return instance.Exec(query, args...)
}

// scanBuf holds the per-query scan slabs. Reused across calls via
// scanBufPool — only one alloc on a cold pool, zero in steady state.
//
// values + ptrs are the targets passed to rows.Scan. rowMap is the
// caller's view; it's cleared (not re-allocated) at the start of every
// QueryEach call so the map's underlying buckets survive across calls.
type scanBuf struct {
	values []any
	ptrs   []any
	rowMap map[string]any
}

var scanBufPool = sync.Pool{
	New: func() any { return &scanBuf{} },
}

func (b *scanBuf) ensure(n int) {
	if cap(b.values) < n {
		b.values = make([]any, n)
		b.ptrs = make([]any, n)
		for i := range b.ptrs {
			b.ptrs[i] = &b.values[i]
		}
	} else {
		b.values = b.values[:n]
		b.ptrs = b.ptrs[:n]
	}
	if b.rowMap == nil {
		b.rowMap = make(map[string]any, n)
	}
}

// QueryEach runs query and invokes visit for each row. The map passed to
// visit is *reused* across rows — visit must read what it needs before
// returning (do not retain row across calls). Saves a per-row map +
// slice alloc compared to QueryRowsAsMaps; also reuses the scan buffer
// across requests via sync.Pool.
//
// Args are passed to the prepared statement. Returns the number of rows
// visited and any error encountered (visit's error short-circuits).
func QueryEach(query string, visit func(row map[string]any) error, args ...any) (int, error) {
	stmt, err := PrepareCached(query)
	if err != nil {
		return 0, err
	}
	rows, err := stmt.Query(args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}

	sb := scanBufPool.Get().(*scanBuf)
	defer scanBufPool.Put(sb)
	sb.ensure(len(cols))

	// Clear without realloc — preserves the map's bucket array.
	for k := range sb.rowMap {
		delete(sb.rowMap, k)
	}

	n := 0
	for rows.Next() {
		if err := rows.Scan(sb.ptrs...); err != nil {
			return n, err
		}
		for i, name := range cols {
			v := sb.values[i]
			if b, ok := v.([]byte); ok {
				sb.rowMap[name] = string(b)
			} else {
				sb.rowMap[name] = v
			}
		}
		if err := visit(sb.rowMap); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// PrepareCached returns a cached *sql.Stmt for the given query, preparing
// it on first use. The same stmt is shared across all callers — sql.Stmt
// is goroutine-safe.
func PrepareCached(query string) (*sql.Stmt, error) {
	stmtCache.RLock()
	s, ok := stmtCache.m[query]
	stmtCache.RUnlock()
	if ok {
		return s, nil
	}

	s, err := instance.Prepare(query)
	if err != nil {
		return nil, err
	}

	stmtCache.Lock()
	if existing, ok := stmtCache.m[query]; ok {
		// Lost the race; close ours and use theirs.
		s.Close()
		s = existing
	} else {
		stmtCache.m[query] = s
	}
	stmtCache.Unlock()
	return s, nil
}

// QueryRowsAsMaps runs a query and returns results as []map[string]interface{}
// — the row shape text/template iterates over via `{{ .field }}`. Used by
// the build pipeline (template-time queries) and the runtime hole renderer;
// centralized here to avoid drift.
//
// Uses PrepareCached internally — repeated calls with the same query
// reuse a single prepared statement, eliminating SQLite's prepare-parse
// cost on the hot path.
//
// []byte values are converted to strings so JSON marshaling and template
// rendering produce readable text (modernc.org/sqlite returns TEXT columns
// as []byte by default).
func QueryRowsAsMaps(query string, args ...interface{}) ([]map[string]interface{}, error) {
	stmt, err := PrepareCached(query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.Query(args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var results []map[string]interface{}
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]interface{}, len(cols))
		for i, name := range cols {
			if b, ok := values[i].([]byte); ok {
				m[name] = string(b)
			} else {
				m[name] = values[i]
			}
		}
		results = append(results, m)
	}
	return results, nil
}

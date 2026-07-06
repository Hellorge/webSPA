package fastdb

import (
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// Cache stores materialised query rows keyed by SQL text, invalidated
// per-table via sqlite3_update_hook. Designed to work correctly under
// any workload (read-heavy or write-heavy) without per-installation
// tuning: tables that thrash via writes flip to a "bypass" window where
// their queries skip the cache entirely until the write rate cools off.
//
// Storage shape — one mutex, one map, one intrusive LRU list.
// Wins from sharding only show up under measurable mutex contention,
// which we haven't seen at gogogo's request volume. Add when measured.
//
// Lookups are O(1). Invalidation is O(queries-referencing-the-written-
// table), thanks to a reverse `map[table]→set[key]` index — not
// O(cache-size).
type Cache struct {
	mu sync.Mutex

	entries    map[uint64]*cacheEntry
	reverse    map[string]map[uint64]struct{}
	head, tail *cacheEntry // LRU: head = most-recently-used

	maxEntries int
	maxBytes   int
	curBytes   int

	tables map[string]*tableStats

	// Bypass parameters. A table receiving more than bypassWriteThreshold
	// hook fires within bypassWindow flips to "bypassed"; queries
	// touching that table skip the cache (Get returns miss, Put no-ops)
	// until bypassCooldown elapses with the rate below threshold.
	bypassWriteThreshold int64
	bypassWindow         time.Duration
	bypassCooldown       time.Duration

	// Memoised SQL→tables results. extractTables is called once per
	// distinct query; the result is reused for every subsequent
	// Get/Put on the same SQL string.
	sqlTables sync.Map // map[string][]string ([]string{} sentinel for "uncacheable")

	hits, misses, evictions atomic.Int64
}

type cacheEntry struct {
	key        uint64
	sql        string
	rows       []map[string]any
	tables     []string
	bytes      int
	prev, next *cacheEntry
}

type tableStats struct {
	windowStart   time.Time
	writes        int64
	bypassedUntil time.Time
}

// CacheConfig knobs are exposed as a value type so callers can override
// individual fields and accept defaults for the rest. Zero values fall
// back to package defaults.
type CacheConfig struct {
	MaxEntries           int
	MaxBytes             int
	BypassWriteThreshold int64
	BypassWindow         time.Duration
	BypassCooldown       time.Duration
}

const (
	defaultMaxEntries           = 4096
	defaultMaxBytes             = 8 << 20 // 8 MiB
	defaultBypassWriteThreshold = 200     // hook fires per window before bypass
	defaultBypassWindow         = time.Second
	defaultBypassCooldown       = 5 * time.Second
)

// NewCache returns an empty cache initialised with the given config (or
// defaults for zero-value fields).
func NewCache(cfg CacheConfig) *Cache {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultMaxEntries
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.BypassWriteThreshold <= 0 {
		cfg.BypassWriteThreshold = defaultBypassWriteThreshold
	}
	if cfg.BypassWindow <= 0 {
		cfg.BypassWindow = defaultBypassWindow
	}
	if cfg.BypassCooldown <= 0 {
		cfg.BypassCooldown = defaultBypassCooldown
	}
	return &Cache{
		entries:              make(map[uint64]*cacheEntry, 64),
		reverse:              make(map[string]map[uint64]struct{}, 8),
		tables:               make(map[string]*tableStats, 8),
		maxEntries:           cfg.MaxEntries,
		maxBytes:             cfg.MaxBytes,
		bypassWriteThreshold: cfg.BypassWriteThreshold,
		bypassWindow:         cfg.BypassWindow,
		bypassCooldown:       cfg.BypassCooldown,
	}
}

// tablesFor returns the list of tables a query touches, memoised across
// calls. Returns nil if the SQL shape is too complex to extract safely
// (subqueries, CTEs) — callers treat nil as "do not cache."
func (c *Cache) tablesFor(sql string) []string {
	if v, ok := c.sqlTables.Load(sql); ok {
		t := v.([]string)
		if len(t) == 0 {
			return nil
		}
		return t
	}
	t := extractTables(sql)
	if t == nil {
		c.sqlTables.Store(sql, []string{}) // sentinel: extracted, none
		return nil
	}
	c.sqlTables.Store(sql, t)
	return t
}

// Get returns cached rows for sql, or (nil, false) on miss / bypass.
// tables is the result of tablesFor(sql) — passed in to avoid double
// extraction on the miss-then-Put path.
func (c *Cache) Get(sql string, tables []string) ([]map[string]any, bool) {
	now := time.Now()
	c.mu.Lock()
	for _, t := range tables {
		if st, ok := c.tables[t]; ok && now.Before(st.bypassedUntil) {
			c.mu.Unlock()
			return nil, false
		}
	}
	key := hashSQL(sql)
	e, ok := c.entries[key]
	if !ok || e.sql != sql {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	c.moveToFrontLocked(e)
	rows := e.rows
	c.mu.Unlock()
	c.hits.Add(1)
	return rows, true
}

// Put stores rows under sql, attributing them to tables for invalidation.
// No-op if any referenced table is currently bypassed. Triggers eviction
// when entry-count or byte caps are exceeded.
func (c *Cache) Put(sql string, tables []string, rows []map[string]any) {
	now := time.Now()
	bytes := estimateBytes(sql, rows)

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, t := range tables {
		if st, ok := c.tables[t]; ok && now.Before(st.bypassedUntil) {
			return
		}
	}

	key := hashSQL(sql)
	if existing, ok := c.entries[key]; ok {
		c.removeLocked(existing)
	}

	e := &cacheEntry{key: key, sql: sql, rows: rows, tables: tables, bytes: bytes}
	c.entries[key] = e
	for _, t := range tables {
		s, ok := c.reverse[t]
		if !ok {
			s = make(map[uint64]struct{}, 1)
			c.reverse[t] = s
		}
		s[key] = struct{}{}
	}
	c.pushFrontLocked(e)
	c.curBytes += bytes

	for len(c.entries) > c.maxEntries || c.curBytes > c.maxBytes {
		if c.tail == nil {
			break
		}
		c.removeLocked(c.tail)
		c.evictions.Add(1)
	}
}

// invalidateTable removes every cached entry that references table and
// updates per-table write-rate stats. Promotes the table to bypass mode
// if the rate exceeds bypassWriteThreshold within bypassWindow.
//
// Called from the update_hook trampoline on every INSERT/UPDATE/DELETE.
func (c *Cache) invalidateTable(table string) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.tables[table]
	if !ok {
		st = &tableStats{windowStart: now}
		c.tables[table] = st
	}
	if now.Sub(st.windowStart) > c.bypassWindow {
		st.windowStart = now
		st.writes = 1
	} else {
		st.writes++
	}
	if st.writes > c.bypassWriteThreshold && !now.Before(st.bypassedUntil) {
		st.bypassedUntil = now.Add(c.bypassCooldown)
	}

	set, ok := c.reverse[table]
	if !ok {
		return
	}
	for key := range set {
		if e, ok := c.entries[key]; ok {
			c.removeLocked(e)
		}
	}
	delete(c.reverse, table)
}

// FlushAll wipes the cache entirely. Used by the data_version watcher
// when an out-of-process or out-of-pool write is detected.
func (c *Cache) FlushAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[uint64]*cacheEntry, 64)
	c.reverse = make(map[string]map[uint64]struct{}, 8)
	c.head, c.tail = nil, nil
	c.curBytes = 0
	// tables is kept — write-rate history shouldn't reset on cache flush.
}

// CacheStats is a snapshot of runtime counters and bypass state. Cheap
// enough to expose to /api/metrics or similar.
type CacheStats struct {
	Hits, Misses, Evictions int64
	Entries                 int
	Bytes                   int
	BypassedTables          []string
}

// Stats returns a copy of the cache's current counters.
func (c *Cache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	now := time.Now()
	c.mu.Lock()
	var bypassed []string
	for t, st := range c.tables {
		if now.Before(st.bypassedUntil) {
			bypassed = append(bypassed, t)
		}
	}
	stats := CacheStats{
		Hits:           c.hits.Load(),
		Misses:         c.misses.Load(),
		Evictions:      c.evictions.Load(),
		Entries:        len(c.entries),
		Bytes:          c.curBytes,
		BypassedTables: bypassed,
	}
	c.mu.Unlock()
	return stats
}

// --- LRU helpers (caller holds c.mu) ---

func (c *Cache) pushFrontLocked(e *cacheEntry) {
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

func (c *Cache) moveToFrontLocked(e *cacheEntry) {
	if e == c.head {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if e == c.tail {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
}

func (c *Cache) removeLocked(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.tail = e.prev
	}
	delete(c.entries, e.key)
	for _, t := range e.tables {
		if s, ok := c.reverse[t]; ok {
			delete(s, e.key)
			if len(s) == 0 {
				delete(c.reverse, t)
			}
		}
	}
	c.curBytes -= e.bytes
}

// estimateBytes is a rough sizer for cache budgeting. Per-row map keys
// and string/byte values are summed; ints/floats counted as 8 bytes.
// Good enough for proportional eviction; not a precise heap accounting.
func estimateBytes(sql string, rows []map[string]any) int {
	n := len(sql)
	for _, r := range rows {
		for k, v := range r {
			n += len(k)
			switch x := v.(type) {
			case string:
				n += len(x)
			case []byte:
				n += len(x)
			default:
				_ = x
				n += 8
			}
		}
	}
	return n
}

func hashSQL(sql string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(sql))
	return h.Sum64()
}

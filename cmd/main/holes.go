package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"sync"
	"text/template"

	"gogogo/modules/build"
	"gogogo/modules/fastdb"
	"gogogo/modules/router"
)

// bufPool reuses bytes.Buffer instances across hole renders. Each buffer
// is reset and returned after composing one response, so the underlying
// slab survives the request and the next caller skips the alloc + grow.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// holeRecord is the parsed view of one in-manifest hole. Template is a
// stdlib text/template parsed once at load and cached by FNV-64 hash
// of the source bytes — manifest reload re-uses parsed templates that
// haven't changed.
//
// Tables is the list of tables the hole's SQL reads (extracted by
// fastdb.TablesIn). Used by the renderer's output cache to know which
// fastdb write events should invalidate the cached page output.
// Tables may be nil if the SQL is too complex to attribute safely
// (subqueries, CTEs) — in that case the page is rendered fresh on
// every request (output cache skips it).
type holeRecord struct {
	Position uint32
	SQL      string
	Template *template.Template
	Tables   []string
}

// templateCache is a lazy parsed-template store keyed by FNV-64 of the
// source bytes. Sub-templates render per-row; cached parses save the
// O(N) parse cost across the build → server-load → request lifecycle.
var templateCache = struct {
	sync.RWMutex
	m map[uint64]*template.Template
}{m: map[uint64]*template.Template{}}

func getOrParseTemplate(tplBytes []byte) (*template.Template, error) {
	h := fnv.New64a()
	h.Write(tplBytes)
	key := h.Sum64()

	templateCache.RLock()
	t, ok := templateCache.m[key]
	templateCache.RUnlock()
	if ok {
		return t, nil
	}

	parsed, err := template.New("hole").Funcs(build.HelpersForHandlers()).Parse(string(tplBytes))
	if err != nil {
		return nil, err
	}

	templateCache.Lock()
	if existing, ok := templateCache.m[key]; ok {
		parsed = existing
	} else {
		templateCache.m[key] = parsed
	}
	templateCache.Unlock()
	return parsed, nil
}

// readHoles parses the per-route hole blob into a slice of holeRecord.
// Layout matches modules/build/v2_compiler.appendHoleBlob:
//
//	[N uint16]
//	repeat: [Position u32][SQLLen u16][SQL bytes][TplLen u16][Tpl bytes]
func readHoles(blob []byte) ([]holeRecord, error) {
	if len(blob) < 2 {
		return nil, nil
	}
	n := binary.LittleEndian.Uint16(blob[:2])
	out := make([]holeRecord, 0, n)
	pos := 2
	for i := 0; i < int(n); i++ {
		if pos+10 > len(blob) {
			return nil, errInvalidHoleBlob
		}
		hPos := binary.LittleEndian.Uint32(blob[pos : pos+4])
		pos += 4
		sqlLen := int(binary.LittleEndian.Uint16(blob[pos : pos+2]))
		pos += 2
		if pos+sqlLen+2 > len(blob) {
			return nil, errInvalidHoleBlob
		}
		sql := string(blob[pos : pos+sqlLen])
		pos += sqlLen
		tplLen := int(binary.LittleEndian.Uint16(blob[pos : pos+2]))
		pos += 2
		if pos+tplLen > len(blob) {
			return nil, errInvalidHoleBlob
		}
		tplBytes := blob[pos : pos+tplLen]
		pos += tplLen

		tpl, err := getOrParseTemplate(tplBytes)
		if err != nil {
			return nil, err
		}

		// Tables=[]string{} (empty, non-nil) for empty-SQL holes means
		// "the hole touches no tables — cacheable, but no table-write
		// signal can invalidate it." Tables=nil from extractTables
		// means "the SQL shape was too complex to attribute safely"
		// and the page should never enter the output cache.
		var tables []string
		if sql == "" {
			tables = []string{}
		} else {
			tables = fastdb.TablesIn(sql)
		}
		out = append(out, holeRecord{
			Position: hPos,
			SQL:      sql,
			Template: tpl,
			Tables:   tables,
		})
	}
	return out, nil
}

var errInvalidHoleBlob = errors.New("gogogo: invalid hole blob")

// composeHoleBody runs each hole's SQL, renders the per-row sub-template
// once per result row, and splices the renderings into the body at the
// recorded positions. The row map is passed as the text/template dot
// context — handlers read columns as `{{ .columnname }}`.
//
// Captured URL bindings (from `:name` / `*name` segments in the route
// pattern) are exposed to every hole template via the `_params` key on
// the dot context — `{{ ._params.id }}`. The underscore prefix avoids
// collision with SQL column names. Routes without bindings get no
// `_params` entry; templates that don't reference it cost nothing.
//
// A hole with an empty SQL string skips the database entirely and runs
// its template exactly once with just the `_params` context — useful
// for rendering pure URL-derived content (API responses, demo pages,
// the `:id` portion of a user profile header).
//
// holes are pre-parsed at manifest load and looked up from
// st.holeCache. Per-request work is just SQL + per-row Execute + bytes
// splicing — no blob parsing, no template parsing.
func composeHoleBody(st *appState, manifest *router.Manifest, entry *router.RouteEntry, bindings []router.Param) ([]byte, error) {
	v := &entry.Variants[router.EncIdentity]
	body := manifest.Payloads[v.DataOffset : v.DataOffset+v.DataLen]

	holes, ok := st.holeCache[holeKey{manifest, entry.HoleOffset}]
	if !ok {
		// Cold path: cache miss (shouldn't happen if loadState was run).
		var err error
		holes, err = readHoles(manifest.HoleBlobs[entry.HoleOffset:])
		if err != nil {
			return nil, err
		}
	}

	var paramsMap map[string]string
	if len(bindings) > 0 {
		paramsMap = make(map[string]string, len(bindings))
		for _, b := range bindings {
			paramsMap[b.Name] = b.Value
		}
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Grow(int(v.DataLen) + 1024)

	cursor := uint32(0)
	var renderErr error
	for i := range holes {
		h := &holes[i]
		buf.Write(body[cursor:h.Position])
		if h.SQL == "" {
			// Param-only hole: no DB query, template runs once.
			ctx := map[string]any{}
			if paramsMap != nil {
				ctx["_params"] = paramsMap
			}
			if err := h.Template.Execute(buf, ctx); err != nil {
				renderErr = err
				break
			}
		} else {
			_, err := fastdb.QueryEach(h.SQL, func(row map[string]any) error {
				if paramsMap != nil {
					row["_params"] = paramsMap
				}
				err := h.Template.Execute(buf, row)
				if paramsMap != nil {
					delete(row, "_params")
				}
				return err
			})
			if err != nil {
				renderErr = err
				break
			}
		}
		cursor = h.Position
	}
	if renderErr != nil {
		bufPool.Put(buf)
		return nil, renderErr
	}
	buf.Write(body[cursor:])

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	bufPool.Put(buf)
	return out, nil
}

// holeKey identifies a hole list within an appState's caches. The
// manifest pointer disambiguates across sites (different sites can share
// HoleOffset values).
type holeKey struct {
	manifest *router.Manifest
	off      uint32
}

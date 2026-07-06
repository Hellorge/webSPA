// Package studio is the gogogo admin site. It's a self-contained module
// providing:
//
//   - A content directory (modules/studio/content/) — the build pipeline
//     bakes its files into the studio manifest as ordinary static routes.
//   - Action routes + handlers for the admin API (sql, tables, metrics,
//     pages, config).
//
// Studio is served as its own site under a configured host (e.g.
// studio.localhost) — there is no special "studio handler" inside the main
// site's request path. cmd/build emits a separate manifest for the studio
// site; cmd/main loads it, registers the host mapping, and dispatches
// requests via the same hash → ActionID → handler flow used for the main
// site.
package studio

import (
	"encoding/json"
	"os"
	"path/filepath"

	"gogogo/modules/actions"
	"gogogo/modules/db"
	"gogogo/modules/gogohttp"
	"gogogo/modules/metrics"
)

// ContentDir points at the file tree that cmd/build walks to bake the
// studio site's static content. Stays alongside the source so the build
// finds it relative to repository root.
const ContentDir = "modules/studio/content"

// Host is the default vhost the studio site listens on. Override via
// configuration if needed (per-environment, multi-instance, etc.).
const Host = "studio.localhost"

// Routes lists studio's dynamic-action routes. Paths are flat — no
// /api/studio/ prefix because the host already says "studio." cmd/build
// reads this list and bakes entries into the studio manifest.
var Routes = []actions.Route{
	{Path: "sql", Method: "POST", ActionID: actions.ActionStudioSQL},
	{Path: "tables", Method: "GET", ActionID: actions.ActionStudioTables},
	{Path: "metrics", Method: "GET", ActionID: actions.ActionStudioMetrics},
	{Path: "pages", Method: "GET", ActionID: actions.ActionStudioPages},
	{Path: "config", Method: "GET, POST", ActionID: actions.ActionStudioConfig},
}

// Handlers maps studio's ActionIDs to their runtime handler functions.
// cmd/main installs these into its actionTable at startup.
var Handlers = map[uint16]actions.Func{
	actions.ActionStudioSQL:     handleSQL,
	actions.ActionStudioTables:  handleTables,
	actions.ActionStudioMetrics: handleMetrics,
	actions.ActionStudioPages:   handlePages,
	actions.ActionStudioConfig:  handleConfig,
}

// Security note: handleSQL executes arbitrary SQL with no auth. Acceptable
// when studio.* is bound to a private interface or behind a reverse-proxy
// auth gateway; gate before exposing publicly.

func handleSQL(c *actions.RouteCtx) {
	// Method enforcement happens in the router (Routes declares "POST"),
	// so we land here only for POST.
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(c.Req.Body, &body); err != nil {
		c.Resp.BadRequest()
		return
	}

	rows, err := db.Query(body.SQL)
	if err != nil {
		writeJSON(c, map[string]interface{}{"error": err.Error()})
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var results []map[string]interface{}
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		rows.Scan(ptrs...)
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
	writeJSON(c, map[string]interface{}{
		"columns": cols,
		"rows":    results,
	})
}

func handleTables(c *actions.RouteCtx) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		c.Resp.ServerError()
		return
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	writeJSON(c, tables)
}

func handleMetrics(c *actions.RouteCtx) {
	writeJSON(c, metrics.Get().GetSnapshot())
}

func handlePages(c *actions.RouteCtx) {
	var pages []string
	root := "web/main/content"
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && path != root {
			rel, _ := filepath.Rel(root, path)
			pages = append(pages, rel)
		}
		return nil
	})
	writeJSON(c, pages)
}

func handleConfig(c *actions.RouteCtx) {
	path := "web/config.toml"
	if c.Req.Method == gogohttp.MethodPOST {
		if err := os.WriteFile(path, c.Req.Body, 0644); err != nil {
			c.Resp.ServerError()
			return
		}
		c.Resp.HeadID = c.App.HeadText()
		c.Resp.Body = []byte("ok")
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		c.Resp.NotFound()
		return
	}
	c.Resp.HeadID = c.App.HeadText()
	c.Resp.Body = body
}

func writeJSON(c *actions.RouteCtx, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		c.Resp.ServerError()
		return
	}
	c.Resp.HeadID = c.App.HeadJSON()
	c.Resp.Body = body
}

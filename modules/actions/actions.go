// Package actions defines the canonical list of dynamic-action routes baked
// into the manifest at build time, plus the types modules implement to plug
// their handlers in.
//
// Adding a new dynamic endpoint:
//   1. Pick a new ActionID constant below.
//   2. Add a Route to either Routes (in this file) or your module's Routes
//      list — whatever fits the module's ownership.
//   3. Register a Func for that ActionID in cmd/main (typically by merging
//      the module's Handlers map into the actionTable).
//   4. Rebuild the manifest (cmd/build).
package actions

import (
	"gogogo/modules/gogohttp"
	"gogogo/modules/router"
)

// Action identifiers. ActionStatic MUST remain 0 — Go's zero-value rule
// means manifest entries that don't explicitly set ActionID inherit it,
// keeping the build-time path simple. New IDs are added in this single
// file to guarantee uniqueness across modules.
const (
	ActionStatic uint16 = iota
	ActionMetrics
	ActionStudioSQL
	ActionStudioTables
	ActionStudioMetrics
	ActionStudioPages
	ActionStudioConfig

	// ActionFile serves bytes from a file on disk via sendfile (zero-copy
	// kernel-direct send). Used for files above the embed threshold —
	// videos, large images, downloads. Range request support is built in.
	// The route's blob (HoleBlobs[HoleOffset:]) stores [pathLen u16][path].
	ActionFile
)

// Route is a single dynamic-route declaration. The fields are visible at
// build time; cmd/build uses them to add manifest entries with the right
// ActionID, methods, and middleware list. cmd/main uses the ActionID to
// dispatch.
//
// Method is a comma-separated list ("GET", "POST", "GET, PUT"). If empty
// the build defaults to "GET". HEAD is auto-derived from GET — declaring
// GET implicitly enables HEAD. Requests whose method isn't in the route's
// list get a 405 with a pre-baked Allow header.
//
// Middleware is a comma-separated list of middleware names that run in
// order before the handler. Each name must match a middleware registered
// at server init via cmd/main's middleware registry. An unregistered
// name fails the server load. A middleware can short-circuit the chain
// (auth fail, rate limit) and the handler will not run.
type Route struct {
	Path       string
	Method     string
	Middleware string
	ActionID   uint16
}

// HandlerCtx is the slice of application state a module-level handler is
// allowed to see. The application's main package implements this interface
// (typically on its appState struct) and passes itself to every handler.
//
// Keeping this minimal lets modules stay decoupled from cmd/main's full
// state. Add fields here when a new module needs something — existing
// handlers ignore what they don't use.
type HandlerCtx interface {
	HeadJSON() uint16
	HeadHTML() uint16
	HeadText() uint16
}

// RouteCtx is the per-request value handed to every action handler. It
// bundles the parsed HTTP request, the response under construction, the
// application's HandlerCtx, and any route bindings the router captured
// from `:name` / `*name` segments.
//
// One RouteCtx per request — pooled by cmd/main to avoid per-request
// allocations. Handlers must NOT retain a reference to it past return.
//
// Future cross-cutting state (request-scoped logger, route metadata,
// trace ID) goes here as new fields without breaking handler signatures.
type RouteCtx struct {
	Req      *gogohttp.Req
	Resp     *gogohttp.Resp
	App      HandlerCtx
	Bindings []router.Param
}

// Bind returns the value of a captured route binding by name, or "" if
// the route doesn't bind that name. Linear scan — the slice is tiny
// (typically 0–2 entries).
func (c *RouteCtx) Bind(name string) string {
	for i := range c.Bindings {
		if c.Bindings[i].Name == name {
			return c.Bindings[i].Value
		}
	}
	return ""
}

// Func is the handler signature dispatched at runtime by ActionID.
// One *RouteCtx per request; the runtime pools and resets it.
type Func func(c *RouteCtx)

// Middleware is a request-stage hook that runs before the handler.
// Returning false short-circuits the chain — the handler does not run,
// and the middleware is responsible for having written a response. A
// middleware that wants the handler to proceed returns true.
//
// Examples:
//   - auth: read session cookie, look up user, attach to RouteCtx; if
//     unauthenticated, write 401 and return false.
//   - rate-limit: check token bucket; if exceeded, write 429 and return
//     false.
//   - audit: log the request; always return true.
//
// The runtime runs middleware in the order they're declared in the
// route's Middleware field. There is no post-handler hook — middleware
// that needs to act after the handler should set up state via the
// RouteCtx and read it back, or wrap behavior with a deferred closure.
type Middleware func(c *RouteCtx) bool

// MiddlewareRegistry is the global name → function map. Modules call
// RegisterMiddleware from their init() to add entries; cmd/main resolves
// route-declared middleware names against it at server load. An
// unregistered name fails the server load (not silent at request time).
var MiddlewareRegistry = map[string]Middleware{}

// RegisterMiddleware registers fn under name. Re-registering the same
// name panics so conflicts surface at startup, not silently at runtime.
func RegisterMiddleware(name string, fn Middleware) {
	if _, exists := MiddlewareRegistry[name]; exists {
		panic("middleware already registered: " + name)
	}
	MiddlewareRegistry[name] = fn
}

// Routes lists the routes the application's main package implements
// directly (not via a module). Module-owned routes live in their respective
// packages (e.g., studio.Routes) and are concatenated at build time.
var Routes = []Route{
	{Path: "api/metrics", Method: "GET", ActionID: ActionMetrics},
}

# Architecture

The thesis: **build does the work; runtime stays dumb-fast**. Templates,
hashes, compression, mime resolution, route tables — all decided at
`./build`. The runtime mmap's a binary manifest and writes bytes.

## Build pipeline (`cmd/build/`)

Driven by `web/config.toml`'s `[[site]]` entries. Per site:

```
web/<site>/                            cmd/build per-site
  ├── content/                         ─ walks subdirs as page families
  │   └── <page>/                        each page = its own dir
  │       ├── content.html               ↓ ALL page metadata lives here:
  │       │                              ↓   {% alias "/X" %}      override URL
  │       │                              ↓   {% block head %}      title/meta
  │       │                              ↓   {% hole "<sql>" %}    live data
  │       │                              ↓   {% extends/block %}   layout
  │       └── style.css | script.js      ↓ optional siblings (auto-linked)
  │
  ├── templates/                       ─ pongo2 master templates
  │   └── <name>/index.html              {% extends "<name>" %} resolves here
  │                                      {% include "<name>" %} too
  │
  └── static/                          ─ assets; below threshold embed,
      └── ...                            above go to dist/<site>/ as ActionFile
```

There is no `meta.toml`. Page metadata is expressed as pongo2 tags
inside `content.html`. The build's dir walk byte-pre-scans each
content.html's first 1 KiB for `{% alias "X" %}` (`build.ScanAlias`)
to discover URL aliases before render. Pages with no alias tag default
to using their directory name as the URL segment.

Per-file dispatch is by extension (`cmd/build/process.go:processFile`):

- `.html` → pongo2 render → hole extraction → minify (only if no holes;
  minification reorders bytes, breaks recorded hole positions) → embed.
- `.css` / `.js` → minify (tdewolff) → embed.
- everything else → embed if `< embedThreshold` (default 1 MiB), else
  write to `dist/<site>/<relPath>` and register an `ActionFile` route.

The result of every file is a `ProcessResult` cached at
`meta/<site>_cache.json`. After the walk, `V2Compiler.Compile` sorts the
route table by `PathHash`, pre-bakes brotli/gzip variants for compressible
mimes, and emits `meta/<site>_router.bin`.

Studio is built separately (`cmd/build/sites.go`) — it's a module-managed
site with action routes and a tiny content tree, not the main pipeline.

## Manifest format (V5, `modules/router/`)

```
meta/<site>_router.bin                 mmap'd at runtime, never re-read
  ├── header                           magic "GOGOV5\x00\x00" + table count
  ├── Table[]                          64-byte entries (one cache line)
  │     PathHash       u64            (unused by trie; kept for future)
  │     PathOffset/Len → Paths slab   URL pattern bytes (with :id / *path)
  │     ActionID       u16             dispatch key
  │     Variants[3]    {HeadID, DataOffset, DataLen}    identity/gzip/brotli
  │     HoleOffset     u32             → HoleBlobs (overloaded for ActionFile)
  │     Methods        u16             bitmask: which HTTP methods this route handles
  │     MwCount        u8              number of middleware names this route runs
  │     MwOffset       u32             byte offset into MiddlewareBlobs
  │
  ├── Payloads          all variants' bytes concatenated (slab)
  ├── Paths             all URL pattern bytes concatenated
  ├── HoleBlobs         per-route blob: hole list OR file path
  ├── MiddlewareBlobs   per-route packed [nameLen u8][name bytes] entries
  └── MimeTable         "type|encoding" strings, indexed by Variant.HeadID
                          at build time (remapped to runtime registry on load)
```

`HoleOffset` is overloaded:
- `ActionStatic` with `HoleOffset != 0`: blob is `[N u16][hole records]`,
  one record per hole (`Position u32`, `SQLLen u16`, `SQL bytes`,
  `TplLen u16`, `Tpl bytes`).
- `ActionFile`: blob is `[pathLen u16][path bytes]` — the on-disk path the
  runtime opens.

`Methods` is a 16-bit bitmask. Bit constants in `modules/router`:
`MethodBitGET`, `MethodBitHEAD`, `MethodBitPOST`, `MethodBitPUT`,
`MethodBitPATCH`, `MethodBitDELETE`, `MethodBitOPTIONS`,
`MethodBitCONNECT`, `MethodBitTRACE`. A request whose method bit is not
set in this mask gets `405 Method Not Allowed` with a pre-baked `Allow:`
header listing the route's actual methods (one head per unique bitmask,
registered at server load).

`MwOffset`/`MwCount` reference the per-route middleware-name list in
`MiddlewareBlobs`. At server load, names are resolved against
`actions.MiddlewareRegistry` into a parallel `[]actions.Middleware`
slice cached on `appState.mwChain[manifest]`. Unknown names fail the
load (loud, not silent).

## Runtime dispatch (`cmd/main/main.go`)

```
[request]
  ↓
Host header → stripPort → appState.sites map → site manifest
                                              (fallback appState.mainSite)
  ↓
manifest.Trie.Match(path, &params)            ← universal lookup
                                                  literal > param > catch-all
                                                  O(depth), not O(log N)
  ↓                                             returns route index (or -1)
idx < 0 → 404
  ↓
entry := &manifest.Table[idx]
  ↓
methodBit & entry.Methods == 0 → 405 with pre-baked Allow header
  ↓
resolve middleware chain (if MwCount > 0):
  for each mw in chain:
    if !mw(rc) → short-circuit (handler skipped, mw wrote a response)
  ↓
switch entry.ActionID
  case ActionStatic:
    if HoleOffset != 0 → composeHoleBody (st, manifest, entry)
                          ↓ st.holeCache lookup ([]holeRecord)
                          ↓ for each hole:
                          ↓   buf.Write body[:hole.Position]
                          ↓   fastdb.QueryEach(hole.SQL, fn)
                          ↓     fn renders pongo2 sub-template per row
                          ↓ buf.Write body[lastPos:]
                          → resp.Body = buf.Bytes()
    else                → pickVariant(entry, req)  (Accept-Encoding negotiate)
                          → resp.Body = manifest.Payloads[variant.DataOffset:]
                          → resp.HeadID = variant.HeadID

  case ActionFile:
    serveFile (cmd/main/files.go) →
      open path, parse Range, set HeadID = fileHeads[mime].head200|head206,
      resp.Stream = io.Copy / io.CopyN  (Go promotes to sendfile(2) on Linux)

  default:
    actionTable[ActionID](rc)              ← rc is *actions.RouteCtx
```

`RouteCtx` (`modules/actions`) bundles the per-request state action
handlers receive: `Req`, `Resp`, `App` (the `HandlerCtx` interface
implemented by `appState`), and `Bindings` (route `:name` captures).
Pooled, reset between requests. Handlers read params via `c.Bind("id")`
— a tiny linear scan over the small slice.

Response writing (`modules/gogohttp/response.go`): head bytes (from
HeaderRegistry) + Content-Length + body — emitted as a single `writev(2)`
via `net.Buffers`. One syscall per response.

## State management

`var state atomic.Pointer[appState]` — every request issues one
`atomic.Load`, no lock contention.

Hot reload (`SIGHUP`):
1. `loadState(cfg, studioPath)` reads every site's manifest, remaps mime
   HeadIDs into a fresh combined registry, pre-parses every hole's body
   into `holeRecord` slices (cached in `appState.holeCache`).
2. `state.Store(next); srv.ReplaceRegistry(next.registry)` — atomic swap.
3. In-flight requests finish on the old `appState`. Old mmaps live until
   nothing references them; address-space "leaks" but RSS doesn't grow
   (cold pages evictable). Process restart is the cleanup primitive.

## Multi-site

Every `[[site]]` in `web/config.toml`:
```toml
[[site]]
name  = "main"               # → web/main/{content,static,templates}
hosts = ["*"]                # → fallback when no host matches

[[site]]
name  = "docs"
hosts = ["docs.localhost"]   # → vhost dispatch
```

The build emits `meta/<name>_router.bin` per site. Runtime maps each
host to its manifest (`appState.sites[host]`), with `appState.mainSite`
as the fallback for the `*` host.

The HeaderRegistry is shared across all sites — a "text/html;
charset=utf-8" head is one entry regardless of how many sites use it.
Each manifest's per-Variant `HeadID` is remapped at load time to point
into the combined runtime registry.

Studio is *not* a `[[site]]`. It's a self-contained module
(`modules/studio/`) that:
- exports `ContentDir`, `Host`, `Routes`, `Handlers`,
- has its own builder in `cmd/build/sites.go`,
- gets registered into `appState.sites[studio.Host]` at runtime.

This pattern is for *applications* (sites with action handlers and bespoke
build needs), not content sites. Don't squeeze new content sites into the
studio pattern; add them as `[[site]]` entries instead.

## Routing — trie, methods, middleware

**Trie** (`modules/router/trie.go`): per-segment, mutable Go struct
built at manifest load. Nodes hold literal-children (map by segment
string), one param child, one catch-all child. `Match(path, &params)`
walks segment-by-segment with backtracking: literal first, then param,
then catch-all. Returns the route's table index; captures populate the
params slab.

Pattern syntax (in route Path strings):
- literal: `api/v1/users`
- param: `:id` — captures one segment
- catch-all: `*path` — captures the rest of the URL (must be terminal)

Build rejects:
- mixed param names at the same trie position (`/users/:id` and
  `/users/:slug` both at depth 2)
- duplicate routes (same path twice)

Mixing static + dynamic siblings *is* allowed: `/users/me` and
`/users/:id` coexist, literal wins on `/users/me`, param wins
elsewhere. The trie tests in `modules/router/trie_test.go` cover every
combination.

**Methods** (`modules/router/v2_binary.go`): `Methods uint16` on
RouteEntry is a bitmask; the build parses each route's `Method` string
(comma-separated) via `router.ParseMethods` and sets the bits.
Declaring `GET` implicitly enables `HEAD` (the runtime auto-derives
HEAD responses by writing headers only). Unknown method names are
silently dropped.

**Allow header** for 405 responses: one head per unique bitmask. At
loadState, the registry walks every route, dedupes by `entry.Methods`,
calls `router.AllowHeaderValue(bits)` to format the comma list, bakes
the full `HTTP/1.1 405 Method Not Allowed\r\nAllow: GET, POST\r\n…`
head, and stores the HeadID in `appState.allowHeads[bits]`. Method
mismatch at request time → `resp.HeadID = st.allowHeads[entry.Methods]`,
done. No string assembly on the hot path.

**Middleware** (`modules/actions/actions.go`):

```go
type Middleware func(c *RouteCtx) bool
```

Returns `false` to short-circuit. The runtime then doesn't call the
handler — the middleware is responsible for having written a response.
There is no post-handler hook (middleware that needs to act after the
handler should set up state via RouteCtx and read it back later).

Registration is global: `actions.RegisterMiddleware("name", fn)`. Module
init() is the natural home. Re-registering the same name panics
(conflicts surface at startup, not at request time).

Per-route names: comma-separated string on the Route declaration. The
build packs them into `MiddlewareBlobs` (length-prefixed strings, one
group per route). At loadState, each route's names are resolved against
`actions.MiddlewareRegistry`; the resolved function-pointer slice is
cached on `appState.mwChain[manifest][routeIdx]`. Unknown names fail
the load — better than silent no-op at request time.

## Holes — the data-rendering primitive

Source form: SQL is inline in the open tag's string argument; the body
is the per-row template.

```
{% hole "SELECT title FROM news ORDER BY date DESC" %}
  <li>{{ row.title }}</li>
{% endhole %}
```

Build-time:
1. `extractHoleBlocks` (a byte scanner, not pongo2) finds every
   `{% hole "<sql>" %}…{% endhole %}` in source HTML, captures (sql,
   body) into parallel slices indexed by the hole's order of appearance,
   and replaces each block with a self-closing `{% hole "<index>" %}`.
2. Pongo2 renders the (now clean) source with `{% extends %}`,
   `{% block %}`, `{% include %}` etc. The hole tag emits a sentinel
   marker (`\x02\x1FHOLE_<index>_HOLE\x1E\x02`) at each site. Markers
   bracket non-printable bytes that never appear in legitimate HTML/CSS/JS.
3. After render, `scanHoleMarkers` walks the output once, records each
   marker's byte offset, looks up the SQL+body by index, strips the
   markers. Result: a clean envelope and a list of `(position, sql,
   body)` tuples.

Runtime:
1. `composeHoleBody` looks up the route's pre-parsed `[]holeRecord` from
   `appState.holeCache` (populated at `loadState`).
2. For each hole: `fastdb.QueryEach(sql, visit)` — one prepared statement
   reused, zero alloc per row. Inside `visit`, the pongo2 sub-template
   renders into a pooled `bytes.Buffer` with a reused row context.
3. The composer splices body slices and rendered chunks; copies into a
   fresh `[]byte` (because the buffer is pooled and will be overwritten
   for the next request).

Constraints: holes can't nest. Position-tracking is a single byte offset.
Each hole is one SELECT and one body. The constraint is the point; it
keeps the runtime a tight loop.

## ActionFile — disk-backed routes

When the build hits a non-HTML file `>= embedThreshold`:
1. Write the bytes to `dist/<site>/<relPath>`.
2. Register an `ActionFile` route whose blob is `[pathLen u16][path]`.
3. At server load (`cmd/main/main.go:loadState`), walk every manifest's
   `ActionFile` routes and pre-register two HTTP heads per mime — one for
   `200 OK`, one for `206 Partial Content`. Both bake `Accept-Ranges: bytes`.

Per request (`cmd/main/files.go:serveFile`):
- Parse `Range` header (RFC 7233; only single-range; multi-range falls
  back to 200 full).
- For full / range / HEAD: pick the right HeadID from `st.fileHeads[mime]`,
  set `resp.Stream = func(w) { io.Copy(w, file) }`. The `gogohttp` server
  invokes the closure with the underlying `*net.TCPConn`. Linux's runtime
  promotes the copy into `sendfile(2)`. Bytes go kernel→kernel.

## fastdb (`modules/fastdb/`)

A minimal SQLite read layer for the hole-render hot path. Bypasses
`database/sql`. Calls `modernc.org/sqlite/lib` directly:

- A pool of `Conn`s (default size `NumCPU * 4`). Each Conn carries its own
  `libc.TLS`, its own `sqlite3 *db`, and its own `map[string]*Stmt`
  (statements are per-connection in SQLite).
- Goroutine acquires a Conn from `chan *Conn`, prepares (or reuses) the
  stmt for its SQL, steps + materializes rows into a *reused*
  `map[string]any`, calls the visitor, returns the Conn.
- Zero allocations per row in steady state. The only fresh alloc is the
  `string()` conversion of TEXT columns — needed because pongo2's row
  access reads strings.

`modules/db/` (the `database/sql` path) is still used for:
- Build-time queries (`cmd/build/process.go`).
- The studio admin handlers (write paths + arbitrary SQL).

Both layers share the same physical DB in WAL mode; concurrent readers
are fine.

## Conventions for adding things

- **A new content page**: drop `web/<site>/content/<dir>/content.html`.
  No sidecar files. The directory name becomes the URL segment unless
  the page declares `{% alias "X" %}` near the top. Page-level metadata
  goes inside `content.html`:
  - `{% extends "def" %}` — layout inheritance
  - `{% alias "/" %}` — override the URL (default = directory name)
  - `{% block head %}<title>…</title><meta …>{% endblock %}` — page head
  - `{% block content %}…{% endblock %}` — page body
  - `{% hole "SELECT …" %}…{% endhole %}` — runtime SQL data
- **A new static asset**: drop `web/<site>/static/<path>`. Below 1 MiB
  embeds; above goes to `dist/<site>/` and serves via sendfile.
- **A new dynamic action**:
  1. Reserve an ActionID in `modules/actions/actions.go`.
  2. Register handler in `actionTable` (in `cmd/main/main.go:init`) or
     module `Handlers` map.
  3. Add a route to `actions.Routes` (or your module's `Routes`) declaring
     `Path`, `Method` (comma-separated list, defaults to `GET`),
     `Middleware` (comma-separated names, optional), and `ActionID`. The
     trie picks up `:name` and `*name` segments in `Path` for free.
  4. (If using middleware that isn't yet registered) call
     `actions.RegisterMiddleware("name", fn)` in module init.
- **A new vhost site**: add a `[[site]]` block to `web/config.toml` and a
  matching `web/<name>/{content,static,templates}` tree. No code changes.
- **A new admin site (with action handlers + bespoke builder)**: copy the
  studio module pattern.

## Page-render context

The build hands `pongo2.Context` to each page render with these
top-level keys (used by master templates and includes):

| Key | Type | Purpose |
|-----|------|---------|
| `PagePath` | string | Final URL the page resolves to (e.g. `/concepts/holes`). Use this in the master template's `<body data-page="…">` and in sidebar `{% if PagePath == "…" %}` active-state checks. |
| `StyleURL` | string | Auto-discovered sibling `style.css` URL, or `""`. |
| `ScriptURL` | string | Auto-discovered sibling `script.js` URL, or `""`. |
| `IsSPAMode` | bool | Single-page-app rendering hint. |

These replace the old `Meta.*` and `Data.*` namespaces. Page-local
variables, if needed, can be set in the page itself with
`{% set foo = "bar" %}` (pongo2 native) and read inside the same
template scope.

# Roadmap — pending features

Items discussed but not yet built. Ordered roughly by foundation-first
(things other features depend on).

## Done since last update

- **Trie-based routing** (`modules/router/trie.go`) replaces binary
  search on a sorted hash table. Supports literal segments, `:param`
  captures, `*catch-all`. Static / param / catch-all sibling
  precedence on overlap. Build-time conflict detection (param-name
  collisions, duplicate routes). Tests in `trie_test.go`.
- **V5 manifest format** (`modules/router/v2_binary.go`): RouteEntry
  grows from 56 → 64 bytes (one cache line). Adds `Methods uint16`
  bitmask, `MwOffset/MwCount` for the per-route middleware-name list,
  and a new `MiddlewareBlobs` section in the file.
- **Method-aware dispatch**: route declarations carry `Method` (comma-
  separated, `GET` default, HEAD auto-derived). 405 returns include a
  pre-baked `Allow:` header — one head per unique bitmask, registered
  at server load.
- **RouteCtx** (`modules/actions/RouteCtx`): per-request bundle of
  Req/Resp/AppState/Bindings handed to action handlers. Pooled,
  reused. Handlers read params via `c.Bind("id")`.
- **Middleware machinery**: `actions.Middleware = func(*RouteCtx) bool`,
  global `actions.MiddlewareRegistry` populated by module init, per-
  route name lists resolved against the registry at server load.
  Short-circuiting (returning false) skips the handler. No middleware
  currently registered — the infrastructure is ready; the first real
  consumer is whoever ships `modules/auth`.

## Foundation / cross-cutting

- **`modules/gogolog`** — async per-request logger, magical context
  via `runtime/pprof` labels. Pre-allocated per-request buffer; writer
  goroutine drains channel to file. ~50ns hot-path cost. The spine for
  observability everywhere else. **(See agents/philosophy.md design discussion.)**
- **All-query observability in `fastdb`** — every `QueryEach` call
  logs SQL hash, prepare/exec time, conn-acquire wait, row count. Pairs
  with gogolog. Aggregated per-SQL-hash for the studio dashboard.
- **`Server-Timing` headers** — auto-emit per-stage durations
  (parse / lookup / db / render / write). Free in DevTools.

## fastdb

- **`fastdb.WithReadTx`** — wrap multi-hole renders in one read
  transaction so all holes see a consistent snapshot. ~80 LOC. SQLite
  WAL makes the BEGIN/COMMIT effectively free.
- **Per-site fastdb pools** — each `[[site]]` gets its own SQLite file
  (default `meta/<site>.db`). Removes cross-site lock contention.
  ~50 LOC; clean refactor.
- **`fastdb.Validate(sql)`** — SQL CI gate. Build calls it for every
  hole's SQL during `processHTML`. Fails compile if syntax / column
  refs are wrong. ~15 LOC.

## Modules to ship

- **`modules/auth`** — *optional reference implementation* of auth as a
  middleware. Cookie + HMAC-signed session token, server-side session
  store, argon2id helpers. Critically: does NOT define a users table or
  role semantics — those are app concerns. Users either import this
  module to get auth out of the box, or write their own auth as a
  middleware registration. The server stays neutral on whether a site
  has users at all. (gogogo is a server, not a framework — auth, roles,
  and user models belong to the application.)
- **`modules/gogojobs`** — background task queue. One worker pool, one
  fastdb table for the queue. ~150 LOC.
- **`modules/gogocron`** — declarative recurring tasks; schedule baked
  into manifest at build.
- **`modules/gogostore`** — file upload handler. Writes to
  `dist/uploads/`, registers as ActionFile route. Reuses existing
  primitives.
- **`modules/gogomail`** — SMTP sender + pongo2 template render. ~200 LOC.
- **`modules/gogortc`** — realtime / SSE module (the "live updates"
  idea). Owns `text/event-stream` connections; subscribes to fastdb
  write events; pushes hole-fragment updates. ~50 LOC of vanilla JS
  client snippet to patch DOM. Single-binary, no Redis.

## Performance

- **ETag / `Last-Modified` for hole pages** — emit headers based on
  `max(updated_at)` from touched tables (build inserts the side query).
  Browser conditional GET → `304 No Content` for unchanged data.
  Big win: skip body + SQL + render entirely.
- **Compile-time partial evaluation** — `{% hole cache="1h" "SELECT …" %}`
  runs the SQL at build, bakes the result inline, sets up a one-shot
  revalidator. Some holes become free (no DB hit).
- **Op-list compilation for hole bodies** — replace pongo2 ExecuteWriter
  in the per-row hot path with a tiny `[]op` walker (`Lit(bytes)` /
  `Field(name)`). Saves ~17% on hole-page CPU. ~150 LOC.
- **io_uring revisit** — earlier rejected at ~5% gain not worth unsafe.
  Could revisit if static QPS becomes a real product goal.

## Build / dev experience

- **Hot reload during dev** — current `-watch` rebuilds; user must
  `SIGHUP` manually. Want auto-SIGHUP after rebuild succeeds.
- **Slow-hole warnings (informational)** — at build, run each hole
  against the dev DB; print warnings for >5ms holes. Never fails build.
- **Better tree command** — already migrated to scan content.html for
  `{% alias %}`. Verify it still works after the big migration.

## Docs site

- **`/concepts/manifest`** — V5 manifest format walkthrough.
- **`/concepts/multi-site`** — how vhost dispatch works.
- **`/concepts/hot-reload`** — SIGHUP + atomic.Pointer state swap.
- **`/about`** — the story / why gogogo exists.
- **Live engine status widget** — small block on landing showing
  routes count + fastdb status, fed by holes.

## Anti-results (already considered, not worth doing)

Documented to avoid re-prototyping:

- **HTTP/2 / HTTP/3 native** — different shape than bake-and-mmap.
  Run behind a reverse proxy if needed.
- **Per-AST-node SQL profiling** — research-level; SQLite's
  `EXPLAIN QUERY PLAN` is the practical substitute.
- **GraphQL endpoint** — out of character; hole-as-API gives most of
  the same value with no new machinery.
- **Type-safe ORM / query builder** — fastdb is the API; SQL is the API.

# Agent briefing — gogogo

A single-binary Go web engine. Pre-rendered, pre-compressed, pre-routed. The
build does the work; the runtime is a dumb-fast hash lookup that mmap's a
manifest and writes bytes to a connection.

## What's in this folder

- [architecture.md](architecture.md) — how it fits together: build pipeline,
  manifest format, runtime dispatch, multi-site, holes, ActionFile.
- [philosophy.md](philosophy.md) — the principles that drive design choices.
  Read this before refactoring or proposing optimizations.
- [benchmarks.md](benchmarks.md) — measured QPS, profile breakdowns, and
  how to reproduce. Use this as the authoritative perf reference.
- [roadmap.md](roadmap.md) — features discussed but not yet built. Check
  here before designing something new — it may already be on the list.

## Bird's-eye view

```
web/                    user content + per-site config
  config.toml             [[site]] entries with hosts
  main/                   the default site
    content/, static/, templates/
  docs/                   the docs site (vhost: docs.localhost)
    content/, static/, templates/

modules/                 engine internals
  gogohttp/               custom HTTP/1.1 server (zero-alloc parser, writev responses)
  router/                 V4 manifest format + mmap loader
  build/                  pongo2 templating + hole extraction
  fastdb/                 zero-database/sql sqlite layer (hole-render hot path)
  db/                     database/sql shim (build-time queries, studio)
  actions/                ActionID registry; dynamic-route handlers go here
  metrics/, profiler/     observability
  studio/                 admin site as a self-contained module
  config/                 typed reader for web/config.toml
  filemanager/, templates/, server/, handlers/

cmd/                     binaries
  build/                  walks web/, runs templates, emits per-site manifests
  main/                   the server: loads manifests, dispatches by Host
  bench/                  in-process router-lookup ceiling test
  simulate/               keep-alive HTTP/1.1 loadgen with p50/p99/p999
  baseline_nethttp/       net/http server for fair-comparison benches
  baseline_fasthttp/      valyala/fasthttp server for fair-comparison benches
  dumpmanifest/           dump a manifest binary's routes (debugging)
  stats/, simulate/       runtime telemetry tools

meta/                    build outputs
  <site>_router.bin       per-site manifest, mmap'd at runtime
  studio_router.bin       module-managed studio manifest
  <site>_cache.json       per-site incremental-build cache
  gogogo.db               SQLite (used by holes, studio)

dist/<site>/             ActionFile-served bodies (large media on disk)
```

## Pages have no sidecar files

There is no `meta.toml`. Each page is a single `content.html` whose
metadata lives as pongo2 tags in the source: `{% alias "/" %}` for URL
override, `{% block head %}<title>…</title>{% endblock %}` for head
tags, `{% hole "SELECT …" %}…{% endhole %}` for live SQL data. The
build's dir walk byte-pre-scans for `{% alias %}` so URL composition
works before any render.

## Single most important fact

The engine has a **universal request flow** for every route:

1. Hash the URL path → binary search a sorted route table.
2. Verify path bytes (collision defense).
3. Dispatch on `ActionID`:
   - `ActionStatic` → write the baked variant (or compose holes).
   - `ActionFile` → open the file, sendfile via `Resp.Stream`.
   - Module action → invoke the registered handler.

There is no hot-path branching for "is this special?" Don't add it. See
[philosophy.md](philosophy.md) for why.

## Working in this repo

- `go run ./cmd/build -force` — full rebuild.
- `go run ./cmd/build -force -stats` — same, with stats.
- `go run ./cmd/build -site=docs -watch` — watch one site.
- `go run ./cmd/main` — start server (`:8082` + pprof on `:6060`).
- `go run ./cmd/simulate -url http://localhost:8082/about -c 100 -d 10s` — bench.
- `go run ./cmd/dumpmanifest meta/main_router.bin` — list a manifest's routes.

Hosts to test multi-site dispatch:
- `http://localhost:8082/` — main site
- `curl -H 'Host: docs.localhost' …` — docs site
- `curl -H 'Host: studio.localhost' …` — admin site

Send `SIGHUP` to the running server to hot-reload manifests without dropping
in-flight requests.

## When you start a task

1. Skim [philosophy.md](philosophy.md) — it dictates whether a given
   optimization or refactor will be welcomed or rejected.
2. If you're touching the request hot path, read the relevant section of
   [architecture.md](architecture.md) first. The path is short; one wrong
   alloc shows up in benchmarks.
3. If you're proposing a perf change, run [benchmarks.md](benchmarks.md)'s
   reproduction recipe before *and* after. Numbers beat hunches.

# Benchmarks

The authoritative perf reference. Numbers below are reproducible with
the recipe at the end.

## Hardware

All measurements: **Intel i7-8665U laptop** (4 physical cores / 8
threads, 1.9 GHz base, 4.8 GHz boost), Linux 6.12, Void Linux, kernel
loopback. Numbers scale roughly with cores and clock; expect ~3–5×
on a modern desktop, ~10–20× on a 32-core server.

## Static-page QPS, controlled comparison

Same machine. Server pinned to cores 0–3 (`taskset -c 0-3`). Loadgen
pinned to cores 4–7. 100 keep-alive connections, 10 second window.
Response body ~4–5 KB.

| Server (post trie + V5 + method dispatch) | QPS | p50 | p99 |
|---|---:|---:|---:|
| **gogogo** | **94,651** | 0.82 ms | 4.3 ms |
| valyala/fasthttp | 65,637 | 1.18 ms | 6.2 ms |
| net/http (stdlib) | 57,675 | 1.19 ms | 8.1 ms |

- gogogo is **+44%** vs fasthttp
- gogogo is **+64%** vs net/http

Previous measurement (before the routing refactor): gogogo at 88k.
Trie lookup did not regress vs binary-search-on-sorted-hash; if
anything it's slightly faster (per-segment hash + map probe vs
full-path hash + log₂N comparisons). Net change: ~+7% on the static
path, comfortably within noise.

(fasthttp serves a 4 KB synthetic; net/http and gogogo serve gogogo's
real `/about` body. Differences in body bytes are small enough not to
flip the ranking.)

### Why gogogo wins this comparison

1. **Pre-baked HTTP heads** — status line + Content-Type + Server +
   `Accept-Ranges` are bytes in the manifest, indexed by `HeadID`.
   Per request: lookup, splice with body, single `writev`.
2. **Pre-compressed variants** — gzip / brotli in the manifest at
   build time. Runtime negotiates Accept-Encoding, picks one variant.
   fasthttp leaves compression to the application.
3. **Single `writev(2)` per response** — head bytes + body in one
   syscall via `net.Buffers`.
4. **Sorted-hash binary search** — zero-alloc lookup, no router state.
5. **`atomic.Pointer[appState]`** — zero-lock state read on every request.

## Hole-page QPS (live SQL per request)

Same conditions as above. URL `/` on main site renders 4 news rows
through a SQL query + per-row pongo2 sub-template.

| Build | QPS |
|---|---:|
| Original (`database/sql`, conn pool 10) | ~33k |
| + bumped pool to 64 | 48k |
| + `db.QueryEach` (zero-alloc scan in `database/sql`) | 45k (no win — wrapper still dominant) |
| **+ `fastdb` (bypass `database/sql`)** | **51k** |
| Post trie + V5 routing | **41k** (within noise — hole-path cost is overwhelmingly fastdb + pongo2, not routing) |

Hole-page is fundamentally bounded by SQL execution. Profile breakdown
of the current state:

```
sqlite VDBE execute (real work)   24%   irreducible
pongo2 ExecuteWriter              25%   eliminable via op-list compile
malloc / GC                       18%   partly eliminable
syscalls (read + writev)          20%   irreducible per request
fastdb plumbing                    6%   bottom of the well
```

The next 10–15k QPS for hole pages is in op-list compilation of hole
sub-templates (replacing pongo2 on the per-row path). Roughly 150 LOC
of build-time compiler + runtime walker.

## Real-network estimate

The pinned-CPU number (~95k QPS) is closer to "real network" reality
than the unpinned ~120k. The unpinned number is inflated by Linux
co-locating loopback reads/writes on the same core for cache locality
— a trick that doesn't exist over a real NIC.

For a production deployment of this code on this hardware over a wired
LAN, expect **~90–115k QPS sustained** for static pages. On a real
server (16-core, 3.5 GHz Ice Lake or Graviton3), expect **~250–350k
QPS**. On a high-core EPYC, **~1M+**.

These are static-path numbers. Hole pages scale less linearly because
SQL execution and SQLite's WAL-reader concurrency become the bottleneck
before CPU does.

## Reproduction recipe

```bash
# 1. Build everything
go build -o /tmp/main-bench ./cmd/main
go build -o /tmp/baseline_nethttp ./cmd/baseline_nethttp
go build -o /tmp/baseline_fasthttp ./cmd/baseline_fasthttp
go build -o /tmp/simulate ./cmd/simulate

# 2. Bench gogogo (server pinned to cores 0-3, loadgen to 4-7)
taskset -c 0-3 /tmp/main-bench &
sleep 2
taskset -c 4-7 /tmp/simulate -url http://localhost:8082/about -c 100 -d 10s
pkill -f /tmp/main-bench

# 3. Bench net/http
taskset -c 0-3 /tmp/baseline_nethttp -addr :9090 -body /tmp/about_body.html &
sleep 2
taskset -c 4-7 /tmp/simulate -url http://localhost:9090/about -c 100 -d 10s
pkill -f /tmp/baseline_nethttp

# 4. Bench fasthttp
taskset -c 0-3 /tmp/baseline_fasthttp -addr :9091 &
sleep 2
taskset -c 4-7 /tmp/simulate -url http://localhost:9091/about -c 100 -d 10s
pkill -f /tmp/baseline_fasthttp
```

To capture a CPU profile of gogogo under load:

```bash
/tmp/main-bench &
sleep 2
# Run the load in the background:
/tmp/simulate -url http://localhost:8082/ -c 100 -d 12s &
# Pull a 10-second profile:
curl -s -o /tmp/cpu.prof "http://localhost:6060/debug/pprof/profile?seconds=10"
wait
# Inspect:
go tool pprof -top -cum /tmp/cpu.prof | head -30
```

(`/cmd/main/main.go` registers `net/http/pprof` on `localhost:6060` at
startup. Don't expose port 6060 in production.)

## When you should re-run these

- After changes to the request hot path (`cmd/main/main.go`, `modules/gogohttp/`).
- After changes to the manifest format (`modules/router/`).
- After changes to fastdb (`modules/fastdb/`).
- Before claiming a perf win. Numbers beat hunches.

If a change makes any of these baselines go down by >5%, that's the
conversation to have before merging.

## Anti-results: things that did *not* work

Documented so we don't repeat the trial:

- **`io_uring` prototype** — gave ~5% QPS at the cost of unsafe pointer
  juggling and a Linux-only path. Rejected at multi-axis evaluation.
- **`db.QueryEach` with reused scan slabs (still inside `database/sql`)**
  — saved ~1k QPS in isolation but the wrapper overhead dominated. The
  win came only when we bypassed the wrapper entirely (`fastdb`).
- **HTTP/2** — explicitly skipped. Multiplexing over a single connection
  is a different shape than the bake-and-mmap design. Run the engine
  behind a reverse proxy if you need H2/H3.

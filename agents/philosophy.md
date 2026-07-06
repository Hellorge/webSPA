# Philosophy

The principles behind every decision in this repo. Read before refactoring,
proposing optimizations, or adding features. Most surprises in this
codebase come from someone violating one of these.

## 1. Build does the work; runtime stays dumb-fast

If a piece of work doesn't *need* per-request data, it doesn't run per
request. Hashing, compression, mime resolution, template parsing,
hole-body extraction, route sorting, alias resolution — all decided at
`./build`. The runtime mmap's bytes and writes them.

Concrete tests for "does this belong at build or runtime":
- "Does the input ever change between requests?" If no → build it.
- "Does the result need to vary by request data?" If only by headers
  (Accept-Encoding, Range, Host), bake all the variants and pick at
  request time. If by SQL data, that's a hole.
- "Could the build do 10x more work to make the runtime simpler?" Yes,
  every time.

This is why we pre-bake gzip *and* brotli variants; pre-register HTTP
heads with HeadIDs; pre-parse hole records at `loadState`; pre-compute
`Accept-Ranges` heads for every ActionFile mime.

## 2. One universal flow, no hot-path special-casing

Every request goes through the same dispatch: hash → binary search →
verify path → `switch ActionID`. There is no "if request looks like X,
take this faster path."

This is by deliberate choice. Tiered/specialized hot paths are how
codebases rot — three engineers each add a "fast path for their case"
and now the resolver is a tree of conditionals nobody can reason about.
One flow, no exceptions.

If a particular route type *is* genuinely hotter (eg ActionStatic vs
ActionFile), the dispatch uses the *same switch* on `ActionID`. The
work that follows differs, but the path to dispatch is uniform.

**Reject** PRs that add `if isStaticAndCached(...)` early-outs in the
resolver, separate fast/slow handler tables, or "skip-the-checks for
trusted routes" branches.

## 3. Multi-axis trade-offs, no fixed % bar

When evaluating a perf optimization, hold four axes in your head:

- **Performance gain** — measured, not guessed.
- **Code complexity** — added LOC, new abstractions, new invariants.
- **Safety** — does it widen the unsafe surface? Add UB risk?
- **Build cost** — slower compile or build? Larger binary?

There is no fixed threshold like "5% perf for 30% complexity". A 3% gain
on the universal hot path with zero added complexity is welcomed; a 30%
gain on a niche path that doubles the number of code paths to maintain
is rejected. Each call is its own conversation.

Examples of how this has played out:
- **brotli baking** (~80% wire savings, +1 dependency, +1 variant slot
  in manifest, no runtime complexity) → **shipped**.
- **io_uring** (~5% gain, +unsafe pointer dance, +Linux-only path,
  +parallel code path to maintain) → **rejected**.
- **fastdb bypass of database/sql** (~50% on hole pages, +220 LOC of
  unsafe-ish modernc.org calls, isolated to one module, doesn't touch
  hot path code style) → **shipped**.

## 4. Don't validate what you don't have to

Trust internal callers. Only validate at system boundaries (HTTP request
input, external API responses, user file I/O). Inside the engine, if
function A returns `(*Manifest, error)` and the caller checks the error,
function B that consumes the manifest doesn't re-check that it's non-nil.

This is why the hot path doesn't have a single `if foo == nil` check —
the boundaries upstream guarantee invariants.

## 5. No half-finished implementations

If you can't ship a feature end-to-end in this PR, ship none of it.
Don't leave half-wired routes, partial fields, or placeholder handlers.
Better: open an issue, sketch the design, then land the whole thing
when it's ready.

This includes feature flags and "shim" code. If you're tempted to add
`if oldBehaviour { ... } else { ... }` for a future migration, just do
the migration.

## 6. Don't write comments that restate code

Default to no comments. Comments earn their place by explaining things
the code can't:
- A hidden constraint (this struct is mmap'd; don't reorder fields).
- A subtle invariant (HoleOffset is overloaded for ActionFile).
- A workaround (pongo2 sub-buffers under {% extends %}, so we use markers).
- A counterintuitive cost (this looks expensive but it's once at load).

Don't write comments that say what the code does, reference the current
task ("added for the X flow"), or document obvious type signatures.

The repo currently has good comments; new code should match that bar.

## 7. Three similar lines beat a premature abstraction

Don't extract a helper after seeing two callers. Three callers with the
same pattern is when you start considering it; a helper that captures
real shared invariants is the goal, not "minimize duplication."

Don't introduce config knobs, plugin points, or registries until at
least one real second user exists.

## 8. Composition over inheritance — modules are files, not packages

Many "modules" in `modules/` are 1–3 files. That's intentional. A module
is a unit of *encapsulation*, not necessarily a unit of *codebase*.
Don't split things across files prematurely. When a module legitimately
has surfaces that need separating (like `cmd/build/`, which has
`main.go`, `context.go`, `process.go`, `worker.go`, `v2_compiler.go`,
etc.), the split is by *role* — not by alphabetical decomposition.

## 9. Numbers beat hunches

Before claiming a perf change works, measure it. Before claiming it's
needed, profile it.

The repo has tooling for this:
- `cmd/simulate` — keep-alive HTTP loadgen with p50/p99/p999 percentiles.
- `cmd/main` exposes pprof on `localhost:6060` in dev — `go tool pprof
  http://localhost:6060/debug/pprof/profile?seconds=10`.
- `cmd/baseline_nethttp` and `cmd/baseline_fasthttp` for fair-comparison
  benchmarks against stdlib and the "extreme Go" reference.
- `cmd/dumpmanifest` to inspect the binary manifest.

If a proposed optimization can't be demonstrated with these, the
proposal isn't ready.

## 10. The docs site is the test bed

`docs.localhost` (`web/docs/`) is the project's own portfolio + docs
site. It's served by the same engine, with the same multi-site
machinery, the same holes, the same ActionFile path. It is not a
toy — it's the regression test you can see in a browser.

When adding a feature to the engine, add a page that demonstrates it
on the docs site. When changing the request flow, run the docs site
and confirm everything still renders.

---

## Visual design language

The docs site is the public face. Its aesthetic mirrors the engine:
small set of decisions, executed sharply. If you're touching docs CSS
or markup, stay within the language below.

### Palette

```
--bg:        #0a0b0f    deep neutral, near-black
--bg-elev:   #131419    cards, code blocks
--rule:      #1f2128    borders
--fg:        #f0f1f5    body text (slightly off-white)
--muted:     #8a909d    secondary text
--accent:    #79b8ff    links, highlights
--accent-2:  #b58ff5    gradient stop
--warm:      #ff9466    gradient stop, warn callouts

--grad-hot:  linear-gradient(95deg, #79b8ff, #b58ff5, #ff9466)
```

The hot gradient is reserved for: brand mark, hero headline accent,
metric numbers, primary CTAs. Don't decorate with it.

### Typography

```
sans:  'Inter' (variable when supported), system-ui, …
mono:  'JetBrains Mono', ui-monospace, Menlo, Consolas, …
serif: ui-serif, Georgia, …  (rarely used; reserve for hero accents)
```

Body 15.5px / 1.65 line-height. Heading hierarchy: 2.15rem → 1.35rem →
1.05rem with tight letter-spacing on h1/h2. Generous vertical rhythm
between sections (3rem h2 spacing).

### Layout

- Max content width: 1080px (1180px for landing).
- Reading column: 720px max.
- Two-column docs shell: 220px sidebar + 1fr article, 3rem gap.
- Sticky header (translucent + backdrop-blur).
- Sticky sidebar (border-left rule, no fill).

### Code blocks

Hand-applied syntax classes: `.kw .str .num .com .fn .ty .pun`. No
real syntax highlighter. Color is purposeful: keywords warm orange,
strings green, types yellow, fields blue. Code blocks live in deeper
background (`#0e1015`) with subtle borders.

### Motion

Minimal. No carousels, no auto-rotating banners, no entry animations
on scroll. Hover transitions are 120–200ms. Scroll is smooth via
`scroll-behavior: smooth`.

### Tone (writing)

- Direct sentences. No hedging.
- Technical-confident, not hype-confident. "The engine does X" beats
  "Lightning-fast blazingly-quick X".
- Lead with the *what*, follow with the *why*. The reader wants to
  understand the constraint, not be sold.
- Don't use emojis in docs prose or code (per project preference).

### Components in use

- `.callout` (info) and `.callout-warn` (warm border) — for asides
  and constraints.
- `.feature-card` — for the landing's 4-up grid.
- `.code-card` — terminal-styled code preview with traffic-light dots.
- `.pager` — next/prev navigation at the bottom of every concept and
  reference page.
- `.actionfile-demo` — the interactive probe block on
  /concepts/actionfile.
- `.news-list` — the hole-rendered live data list.
- `.metric` — for inline scalar holes.

When in doubt, look at `web/docs/static/style.css` and copy the existing
pattern rather than introducing a new component.

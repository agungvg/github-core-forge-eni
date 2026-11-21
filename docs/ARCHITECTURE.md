# Architecture — github-core-clone-01 entry

## What the program is

One Go process, one static binary (`forge-go`, built with
`CGO_ENABLED=0`), speaking four protocols from a single in-memory
store: git smart HTTP, GitHub REST v3, GraphQL v4, and MCP. No
third-party packages, no git binary, no network access from the
server, no model calls, no user interface.

A thin Node launcher (`main.ts`) exists only because the trial pod may
mount the entry directory read-only or noexec: it copies the binary
into a fresh tmp directory, marks it executable, and runs it there.
The Go binary then owns the whole harness lifecycle itself — it binds
a kernel-chosen port, POSTs it to `$MAKEFUN_ENV_URL/register`, answers
`/healthz`, and long-polls `/done` until the trial ends.

## How the code is split

Every file has exactly one job, and the split follows the protocol
boundaries so a change to one surface cannot leak into another:

| File | Job |
| --- | --- |
| `main.ts` | Launcher only — stage `./forge-go` into tmp, chmod, run. |
| `go/main.go` | Harness handshake: bind port, register, poll `/done`, exit. |
| `go/app.go` | HTTP server and router: parse method/path, read the body, dispatch to the surface handlers, write replies. No business logic. |
| `go/store.go` | All state: repositories, refs, issues, labels, comments, pulls; git-content helpers (write/read a file, list a directory, list commits); the squash-merge engine; REST JSON shapes; the deterministic clock and id counters. |
| `go/gitutil.go` | Git primitives: SHA-1 ids, pkt-line framing, packfile v2 read/write, commit/tree/tag build and parse, reachability, ancestry, merge-base. No HTTP, no store. |
| `go/githttp.go` | Git smart HTTP: v2 and v0 advertisements, ls-refs, fetch (v2 sections and v0), receive-pack (commands, pack import, fast-forward report). |
| `go/rest.go` | REST v3 routes: auth, pagination, repositories, contents, branches, commits, labels, issues, comments, pulls, merge. |
| `go/graphql.go` | GraphQL v4: document dispatch, introspection types, repository/issue/PR fields, mutations, batching, opaque global ids. |
| `go/mcp.go` | MCP: `server/discover`, `initialize`, `tools/list` (static, byte-identical across calls), `tools/call` onto the same store, JSON-RPC errors. |
| `go/js.go` | The small JS-value helpers the store uses. |
| `docs/` | `recon.md`, `features.csv`, `ARCHITECTURE.md`, `DECISIONS.md`. |

## One core, four surfaces

`gitutil.go` together with `store.go` is the shared core. Every
surface reads the same repositories, objects and issues through it,
so an issue created over MCP reads back over REST and GraphQL with
the same ids, and a commit pushed over git is visible to REST
contents and GraphQL in the same form. There is no per-surface copy
of the data to drift.

## Determinism

The replay rule decides the design: a live run and its replay must
leave byte-identical state, so nothing served may depend on the wall
clock, on unseeded randomness, or on unseeded UUIDs.

- Timestamps come from `MAKEFUN_TRIAL_NOW` (default
  2025-01-01T00:00:00Z) plus a monotonic sequence counter — one tick
  per created thing — so a replay produces the same state.
- Ids and numbers come from counters (repository ids from 5000, issue
  ids global from 1).
- Global ids are base64url of a typed tag (`R:5000`,
  `I:<repo>:<n>` …) — opaque to the client, stable across reads, and
  decodable for `node()` and mutations without a lookup table.
- The viewer identity is a constant (`adaiversen`, id 1000): the
  scenario seed is fixed and the bearer token is opaque
  (`shpat_<hex>`, encoding no login), so the token's owner is known
  by construction, not derived at runtime.

## Why it is fast enough

- All state lives in memory (`map`s). No per-request disk I/O. The
  hot paths (issue reads and creates, receive-pack, fetch) do no work
  beyond serving the request itself.
- Packs are written with whole objects only — no delta search on the
  push/clone hot paths; client-sent deltas are still resolved on read.
- Fetch builds reachable sets with a visited-set BFS and subtracts
  what the client already holds; shallow and deepen walks are
  depth-limited.
- REST read/write hot paths build one small JSON object per request;
  the request id is a counter, not a UUID.
- Go's `net/http` keep-alive serves the 2,000-read workload on one
  connection, with no per-request allocation beyond the response.

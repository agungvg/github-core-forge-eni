# Architecture — github-core-clone-01 entry

## Stack

- Go 1.25, standard library only. No third-party packages, no git binary,
  no network access from the server, no model calls. One process, one
  binary (`forge-go`, built with CGO_ENABLED=0).
- `main.ts` is the only Node code: a launcher that stages the binary into
  a fresh tmp directory and runs it there, because the trial pod can mount
  `/bot` read-only or noexec.
- The Go binary owns the harness lifecycle: bind a kernel-chosen port,
  POST it to `$MAKEFUN_ENV_URL/register`, answer `/healthz`, then poll
  `/done` until the trial ends and exit 0.
- All state lives in memory (`map`s). No per-request disk I/O. The hot
  paths (issue reads and creates, receive-pack, fetch) do no work beyond
  serving the request itself.

## Files (one job each)

| File | Responsibility |
| --- | --- |
| `main.ts` | Launcher only — stage `./forge-go` into tmp, chmod, run. |
| `go/main.go` | Harness handshake: bind port, register, poll `/done`, exit. |
| `go/app.go` | HTTP server and router: method/path parsing, body reading, dispatch to the surface handlers, reply writing. No business logic. |
| `go/store.go` | All state: repositories, refs, issues, labels, comments, pulls; git-content helpers (write a file, read a file, list a directory, list commits); the squash-merge engine; REST JSON shapes; the deterministic clock and id counters. |
| `go/gitutil.go` | Git primitives: SHA-1 ids, pkt-line framing, packfile v2 read/write, commit/tree/tag build and parse, reachability, ancestry, merge-base. No HTTP, no store. |
| `go/githttp.go` | Git smart HTTP: v2 and v0 advertisements, ls-refs, fetch (v2 sections and v0), receive-pack (commands, pack import, fast-forward report). |
| `go/rest.go` | REST v3 routes: auth, pagination, repositories, contents, branches, commits, labels, issues, comments, pulls, merge. |
| `go/graphql.go` | GraphQL v4: document dispatch, introspection types, repository/issue/PR fields, mutations, batching, opaque global ids. |
| `go/mcp.go` | MCP: `server/discover`, `initialize`, `tools/list` (static, byte-identical across calls), `tools/call` onto the same store, JSON-RPC errors. |
| `go/js.go` | The small JS-value helpers the store uses. |
| `docs/` | `recon.md`, `features.csv`, `ARCHITECTURE.md`, `DECISIONS.md`. |

## One shared core

`gitutil.go` plus `store.go` are the shared core: every surface reads the
same repositories, objects and issues through them. An issue created over
MCP reads back over REST and GraphQL with the same ids, and a commit pushed
over git is visible to REST contents and GraphQL in the same form.

## Determinism

- No wall-clock time, no `math/rand`, no unseeded UUIDs in served data.
- Timestamps come from `MAKEFUN_TRIAL_NOW` (default 2025-01-01T00:00:00Z)
  plus a monotonic sequence counter — one tick per created thing — so a
  replay produces the same state byte-for-byte.
- Ids and numbers come from counters (repository ids from 5000, issue ids
  global from 1).
- Global ids are base64url of a typed tag (`R:5000`, `I:<repo>:<n>` …) —
  opaque to the client, stable across reads, decodable for `node()` and
  mutations without a lookup table.
- The viewer identity is a constant (`adaiversen`, id 1000): the scenario
  seed is fixed and the bearer token is opaque (`shpat_<hex>`, encoding no
  login), so the token's owner is known by construction, not derived at
  runtime.

## Speed notes

- Packs are written with whole objects only — no delta search on the
  push/clone hot paths; client-sent deltas are still resolved on read.
- Fetch builds reachable sets with a visited-set BFS and subtracts what the
  client already holds; shallow and deepen walks are depth-limited.
- REST read/write hot paths build one small JSON object per request; the
  request id is a counter, not a UUID.
- Go's `net/http` keep-alive serves the 2,000-read workload on one
  connection, with no per-request allocation beyond the response.

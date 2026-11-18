# Architecture — github-core-clone-01 entry

## Stack
- Node 22, standard library only. No dependencies, no git binary, no network
  access from the server, no model calls. Entry point `main.ts` (starter
  lifecycle, byte-identical): `listen(0)` → register → poll `done` → exit 0.
- TypeScript in erasable syntax only (no enums/namespaces), run with
  `node --experimental-strip-types main.ts`.
- Everything in memory (`Map`s). No per-request disk I/O. Hot paths
  (GET issue, POST issue, receive-pack, fetch) do no work beyond the
  request itself.

## Modules (one responsibility each, short files)
| File | Owns |
|---|---|
| `main.ts` | Lifecycle only (starter, as-is). |
| `app.ts` | HTTP server + routing only: parses method/path, reads the body, dispatches to the surface handlers, writes the reply. No business logic. |
| `store.ts` | All state: repos, refs, issues, labels, comments, pulls; git-content helpers (commit file, read file, list dir, list commits); squash-merge engine; REST JSON shapes; deterministic clock + ids. |
| `gitutil.ts` | Git primitives: SHA-1 ids, pkt-line, packfile v2 read/write, commit/tree/tag build+parse, reachability, ancestry, merge-base. No HTTP, no store. |
| `git-http.ts` | Git smart HTTP: v2/v0 advertisements, ls-refs, fetch (v2 sections + v0), receive-pack (commands, pack import, fast-forward report). |
| `rest.ts` | REST v3 routes: auth, pagination, repos, contents, branches, commits, labels, issues, comments, pulls, merge. |
| `graphql.ts` | GraphQL v4: document dispatch, introspection types, repository/issues/PRs fields, mutations, batching, opaque global ids. |
| `mcp.ts` | MCP: `server/discover`, `initialize`, `tools/list` (static, byte-identical), `tools/call` onto the same store, JSON-RPC errors. |
| `docs/` | `recon.md`, `features.csv`, `ARCHITECTURE.md`, `DECISIONS.md`. |

## Shared core first
`gitutil.ts` + `store.ts` are the shared core: every surface reads the same
repos/objects/issues through them, so an issue created over MCP reads back
over REST and GraphQL with the same ids, and a commit pushed over git is
visible to REST contents and GraphQL the same way.

## Determinism
- No `Date.now()` / `new Date()` (except derived from the trial clock) /
  `Math.random()` / unseeded UUIDs in served data.
- Timestamps: `MAKEFUN_TRIAL_NOW` (default 2025-01-01T00:00:00Z) + a
  monotonic sequence counter, one tick per created thing.
- Ids/numbers: counters (repo ids from 5000, issue ids global from 1).
- Global ids: base64url of a typed tag (`R:5000`, `I:<repo>:<n>` …) — opaque,
  stable, decodable.
- The viewer identity is a constant (`adaiversen`, 1000): the scenario seed
  is fixed and the bearer token is opaque, so the token names its owner by
  construction.

## Performance notes
- Whole-object packs on write (no delta search); deltas resolved on read.
- Fetch computes reachable sets with visited-set BFS and subtracts the
  client's held set; shallow/deepen walks are depth-limited.
- REST read/write hot paths build one small JSON object per request; the
  request id is a counter, not a UUID.
- Node's `http` server keep-alive serves the 2,000-read workload on one
  connection; no per-request allocation beyond the response.

# github-core-forge-eni

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A git forge that speaks GitHub's wires — git smart HTTP, REST v3, GraphQL
v4 and MCP — implemented in Go for speed, from a clean room, for make.fun's
github-core-clone-01 bounty. It clones, fetches and pushes over smart HTTP,
and serves repositories, issues, labels, comments and pull requests (with
squash merges) through all three APIs, backed by one shared in-memory
store. Nothing but the Go standard library: no dependencies, no git binary,
no external service, no model calls. `main.ts` is a thin Node launcher that
stages the statically linked `./forge-go` binary (built from `go/`) into a
fresh tmp directory and runs it; the Go binary carries out the harness
handshake on its own.

## Running it

```sh
node main.ts
```

The harness starts it as `node main.ts` on Node 22.18+. Three environment
variables steer a run:

- `MAKEFUN_ENV_URL` — where the forge announces itself; the process exits
  immediately without it.
- `MAKEFUN_SHOP_TOKEN` — the Bearer token required on every API call.
- `MAKEFUN_TRIAL_NOW` — the frozen clock every served timestamp is derived
  from, so a trial replayed produces byte-identical state.

## What it serves

| Route | What happens there |
| --- | --- |
| `GET /healthz` | Boot probe: 200, `{"ok":true}` |
| `/<owner>/<repo>.git/...` | Git smart HTTP — upload-pack protocol v2 (ls-refs, fetch, shallow, deepen) and v0, receive-pack v0 with report-status and side-band-64k |
| `/api/v3/...` | REST v3 — repos, branches, commits, contents, labels, issues, comments, pull requests, squash merges |
| `POST /api/graphql` | GraphQL v4 — viewer and repository queries, issue and pull-request connections, create/close/merge mutations, `node()` and `__type` introspection |
| `POST /mcp` | MCP over streamable HTTP — `server/discover` (revision 2026-07-28) and the legacy `initialize` handshake, `tools/list`, `tools/call` |

The git layer is written from scratch: SHA-1 object ids, packfile v2
reading and writing (objects stored whole; OFS and REF deltas resolved on
read), pkt-line framing, want/have negotiation, and a fast-forward check on
every push. Because the four surfaces share a single store, a repository
created over MCP reads back over REST and GraphQL with identical ids.

## Layout

- `main.ts` — launcher only: copies `./forge-go` to a fresh tmp dir, makes
  it executable, runs it with inherited env and stdio, so the entry works
  whether `/bot` is read-only or noexec.
- `forge-go` — statically linked Linux amd64 binary (CGO_ENABLED=0).
- `go/` — the Go source (`go 1.25`, standard library only). `main.go` owns
  the harness handshake (bind a kernel-chosen port, POST it to `/register`,
  poll `/done` until the trial ends); `app.go` is the HTTP server and
  router; `gitutil.go` holds the git primitives (object ids, pkt-lines,
  packfiles, commit/tree/tag build and parse, reachability, ancestry,
  merge-base); `store.go` holds all state, the squash-merge engine and the
  deterministic clock; `githttp.go` is git smart HTTP; `rest.go` is REST
  v3; `graphql.go` is GraphQL v4; `mcp.go` is MCP; `js.go` holds the small
  JS-value helpers the store uses.
- `docs/` — `recon.md` maps the kit's checks onto the contract,
  `features.csv` is the feature matrix, and `ARCHITECTURE.md` /
  `DECISIONS.md` record how the forge is built and why.

## Verifying it locally

From the competition kit directory, with this entry checked out beside it:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token <token> --perf
node cli/makefun.mjs grade-local --kit . --bot <this repo> --driver process
```

Against a fresh server the kit's conformance runner passes all 120 checks —
600 feature points and 200 agent points. `grade-local --driver process`
practice trials of the Go build score ~987 (feature 600, agent 200, perf
~187, p50 ~228,000 µs), against ~965–983 for the TypeScript build on the
same machine.

---

<sub>Entered on [make.fun](https://make.fun/?ref=p9r33tb4&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

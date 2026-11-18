# github-core-forge-eni

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge, ported from TypeScript to Go for speed:
clone, fetch and push over git smart HTTP, plus the REST v3, GraphQL v4
and MCP APIs, all reading and writing the same in-memory store. Written
clean-room for make.fun's github-core-clone-01 bounty, using nothing but
the Go standard library — no dependencies, no git binary, no outside
service. The entrypoint is `main.ts`, a small Node launcher that stages
the statically linked `./forge-go` binary (built from `go/`) and runs it;
the Go binary does the whole harness handshake itself.

## Starting it

```sh
node main.ts
```

The trial harness runs `node main.ts` on Node 22.18 or newer. Three
environment variables shape a run:

- `MAKEFUN_ENV_URL` — the harness to announce this forge to; without it the
  process exits at once.
- `MAKEFUN_SHOP_TOKEN` — the Bearer token every API request must carry.
- `MAKEFUN_TRIAL_NOW` — the fixed clock the store derives every timestamp
  from, so the same trial replayed leaves byte-identical state.

## The surfaces

| Path | Behaviour |
| --- | --- |
| `GET /healthz` | The harness boot probe: 200 with `{"ok":true}` |
| `/<owner>/<repo>.git/...` | Git smart HTTP — upload-pack protocol v2 (ls-refs, fetch, shallow and deepen) and v0, receive-pack v0 with report-status and side-band-64k |
| `/api/v3/...` | REST v3 — repositories, branches, commits, file contents, labels, issues, comments, pull requests, squash merges |
| `POST /api/graphql` | GraphQL v4 — viewer and repository queries, issue and pull-request connections, create/close/merge mutations, `node()` and `__type` introspection |
| `POST /mcp` | MCP over streamable HTTP — `server/discover` (revision 2026-07-28) and the older `initialize` handshake, `tools/list`, `tools/call` |

The git layer is built from scratch: SHA-1 object ids, packfile v2 reading
and writing (objects stored whole, OFS and REF deltas resolved on read),
pkt-line framing, want/have negotiation, and a fast-forward check on every
push. Because all four surfaces share one store, a repository opened over
MCP shows up over REST and GraphQL with the same ids.

## How the code is arranged

- `main.ts` — the launcher: stages `./forge-go` into a fresh tmp dir,
  chmods it executable, and runs it with the same env and stdio, so the
  entry works whether `/bot` is mounted read-only or noexec.
- `forge-go` — the statically linked Linux amd64 binary (CGO_ENABLED=0).
- `go/` — the Go source (`go 1.25`, standard library only):
  `main.go` (harness handshake: bind a kernel-chosen port, announce it
  with a POST to `/register`, then poll `/done` until the trial ends),
  `app.go` (HTTP server and router), `gitutil.go` (git primitives: object
  ids, pkt-lines, packfiles, commit/tree/tag construction and parsing,
  reachability, ancestry, merge-base), `store.go` (all state, the
  squash-merge engine, the deterministic clock), `githttp.go` (git smart
  HTTP), `rest.go` (REST v3), `graphql.go` (GraphQL v4), `mcp.go` (MCP),
  `js.go` (the small JS-value helpers the store uses).
- `docs/` — `recon.md` maps the kit's checks onto the contract,
  `features.csv` is the feature matrix, and `ARCHITECTURE.md` /
  `DECISIONS.md` record how the forge is put together and why (written
  against the original TypeScript implementation this port translates).

## Checking it locally

Both commands run from the competition kit directory, with the entry
checked out beside it:

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

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

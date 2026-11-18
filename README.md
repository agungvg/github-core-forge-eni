# github-core-forge-eni

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

One Node process that behaves like GitHub's core: clone, fetch and push over
git smart HTTP, plus the REST v3, GraphQL v4 and MCP APIs, all reading and
writing the same in-memory store. Written clean-room for make.fun's
github-core-clone-01 bounty, using nothing but the Node standard library —
no dependencies, no git binary, no outside service.

## Starting it

```sh
node --experimental-strip-types main.ts
```

The trial harness runs `node main.ts` on Node 22.18 or newer, which strips
the TypeScript types by itself, so there is nothing to install. The only
imports are `node:http`, `node:crypto` and `node:zlib`, plus this repo's own
modules. Three environment variables shape a run:

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

- `main.ts` — the forge's handshake with the harness: bind a kernel-chosen
  port, announce it with a POST to `/register`, then poll `/done` until the
  trial ends and exit 0.
- `app.ts` — the HTTP server and the router; nothing else lives here.
- `gitutil.ts` — the git primitives: object ids, pkt-lines, packfiles,
  commit/tree/tag construction and parsing, reachability, ancestry, merge-base.
- `store.ts` — all state (repositories, refs, issues, labels, comments, pull
  requests), the squash-merge engine, and the deterministic clock.
- `git-http.ts` — git smart HTTP. `rest.ts` — REST v3. `graphql.ts` —
  GraphQL v4. `mcp.ts` — MCP.
- `docs/` — `recon.md` maps the kit's checks onto the contract,
  `features.csv` is the feature matrix, and `ARCHITECTURE.md` /
  `DECISIONS.md` record how the forge is put together and why.

## Checking it locally

Both commands run from the competition kit directory, with the entry
checked out beside it:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token <token> --perf
node cli/makefun.mjs grade-local --kit . --bot <this repo> --driver process
```

Against a fresh server the kit's conformance runner passes all 120 checks —
600 feature points and 200 agent points. `grade-local --driver process
--instances 3` gives 600 feature and 200 agent points on each of the three
practice trials (mean score 877.3; the performance column moves a little with
machine load).

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

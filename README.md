# github-core-forge-eni

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/agungvg/github-core-forge-eni/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one Node process, standard library only — git
smart HTTP, REST v3, GraphQL v4 and MCP over a single in-memory store. Built
clean-room for make.fun's github-core-clone-01 bounty.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node main.ts` on Node 22.18 or later, which strips
TypeScript types on its own. There is nothing to install: the program imports
`node:http`, `node:crypto` and `node:zlib` plus its own modules, and the trial
has no package manager and no internet. `MAKEFUN_ENV_URL` names the harness
environment to register with; `MAKEFUN_SHOP_TOKEN` is the Bearer <redacted> every
API call arrives with; `MAKEFUN_TRIAL_NOW` is the deterministic clock every
timestamp derives from, so two runs of the same trial leave identical state.

## What it serves

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe |
| `/<owner>/<repo>.git/...` | git smart HTTP: upload-pack protocol v2 (ls-refs, fetch, shallow/deepen) and v0, receive-pack v0 with report-status and side-band-64k |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, labels, issues, comments, pull requests with squash merges |
| `POST /api/graphql` | GitHub GraphQL v4: viewer and repository queries, issue and pull-request connections, create/close/merge mutations, `node()`, `__type` introspection |
| `POST /mcp` | MCP over streamable HTTP: `server/discover` (2026-07-28) and the legacy `initialize` flow, `tools/list`, `tools/call` |

Git objects are implemented from scratch: SHA-1 ids, packfile v2 read/write
(whole objects on write, OFS/REF deltas resolved on read), pkt-line framing,
want/have negotiation, fast-forward enforcement on every push. All four
surfaces read the same in-memory store, so a repo created over MCP reads back
over REST and GraphQL with the same ids.

## Layout

- `main.ts`: the entry ↔ env lifecycle (starter, as-is). Binds an ephemeral
  port, registers it, polls `done`, exits 0.
- `app.ts`: the HTTP server and router only.
- `gitutil.ts`: git primitives — object ids, pkt-lines, packfiles, commit/tree/
  tag build+parse, reachability, ancestry, merge-base.
- `store.ts`: all state — repos, refs, issues, labels, comments, pull
  requests — plus the squash-merge engine and the deterministic clock.
- `git-http.ts`: git smart HTTP. `rest.ts`: REST v3. `graphql.ts`: GraphQL v4.
  `mcp.ts`: MCP.
- `docs/`: `recon.md` (the contract, mapped from the kit's checks),
  `features.csv` (feature matrix), `ARCHITECTURE.md`, `DECISIONS.md`.

## Local grading

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token <token> --perf
node cli/makefun.mjs grade-local --kit . --bot <this repo> --driver process
```

Local conformance (kit `sdk/run.ts`, fresh server): 120/120 checks —
feature 600, agent 200. `grade-local --driver process --instances 3`:
feature 600, agent 200 on all three practice trials (mean score 877.3;
perf varies with machine load).

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

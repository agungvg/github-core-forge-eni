# Recon — github-core-clone-01 (GitHub-compatible git forge)

The job in one line: a standalone Go server (standard library only,
one process, one static binary) that speaks GitHub's four wires — git
smart HTTP, REST v3, GraphQL v4, MCP — well enough to pass the kit's
120 conformance checks and its 4 performance workloads. No UI. No git
binary. No network. No model calls.

Where the contract comes from: `RULES.txt` plus
`program/env/src/checks-{git, rest, graphql, mcp}.ts`, `wire.ts`,
`git-client.ts`, `mcp-client.ts`, `rest-client.ts`, `bench.ts`,
`manifest.ts`, `competition.yaml`. The suite IS the specification;
anything outside it is cuttable.

## 0. Lifecycle / boot (the Go binary owns it)

- Bind an ephemeral port (`listen(0)`, never 8080) → POST
  `{"port": N}` to `$MAKEFUN_ENV_URL/register` → poll GET
  `$MAKEFUN_ENV_URL/done` → exit 0.
- `GET /healthz` → 200 `{"ok":true}` (called twice within 10s of
  registering).

## 1. Git smart HTTP — `/{owner}/{repo}.git`

- `GET /info/refs?service=git-upload-pack` /
  `?service=git-receive-pack`: v2 when the client sends
  `Git-Protocol: version=2`, otherwise v0. Missing repo → 404, and
  the server stays up.
- v2 advertisement: `version 2`, `ls-refs=unborn`, `fetch=shallow
  wait-for-done filter`, `object-format=sha1`. v2 `ls-refs` (peel,
  symrefs, unborn, ref-prefix, `HEAD` with
  `symref-target:refs/heads/main`; annotated tags carry
  `peeled:<commit>`). v2 `fetch` (want/have, thin-pack,
  shallow/deepen, `acknowledgments` / `shallow-info` / `packfile`
  sections, side-band-64k).
- v0 advertisement: `# service=git-upload-pack` preamble,
  capabilities on the first ref line after NUL (`side-band-64k
  thin-pack ofs-delta shallow multi_ack_detailed
  symref=HEAD:refs/heads/main`); an empty repo uses the
  `capabilities^{}` pseudo-ref. Receive-pack v0 capabilities must
  include `report-status`, `side-band-64k`, `delete-refs`.
- Push (receive-pack POST): pkt-line commands plus a pack; a per-ref
  report (`unpack ok`, `ok <ref>` / `ng <ref> non-fast-forward`).
  Every update must fast-forward; a mixed push applies the
  fast-forward ref and refuses the other one.
- Objects built from scratch: SHA-1 ids, loose-object hashing,
  packfile v2 read/write (whole objects only — valid and simplest),
  pkt-line framing, side-band-64k, want/have negotiation, ancestry
  checks.
- A malformed POST body must not crash the server (the repo survives
  errors).

## 2. REST v3 — `/api/v3`

- Every reply is `application/json`, never HTML; a non-empty
  `X-GitHub-Request-Id` on every reply; errors are JSON
  `{message: string}`; unknown route → 404 JSON; malformed JSON body
  → 400.
- Auth: `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` on every call;
  missing or wrong → 401. A repo belongs to the token's login; the
  same name under another owner → 404, never 403.
- `GET /` (200 plus `X-OAuth-Scopes`), `GET /meta`
  (`installed_version`), `GET /user` (viewer identity),
  `GET /user/repos` (paginated, `Link: rel="next"`, at most 10 per
  page), `POST /user/repos` (201; duplicate → 422).
- `GET/PATCH/DELETE /repos/{owner}/{repo}`; `GET /repos/{o}/{r}/branches`,
  `/branches/{b}`, `/git/refs/heads/{b}`; `POST /git/refs`;
  `GET /commits` (branch-filtered, exact messages).
- `PUT/DELETE /repos/{o}/{r}/contents/{path}` (201 create / 200
  update / 409 stale sha / 200 delete with `{commit:{sha}}`; the first
  commit into an unborn repo works; a PUT on a missing branch creates
  it from the default tip).
- Labels (`POST /labels` → 201), issues (`POST /issues` → 201; 422 on
  an unknown label; numbers increment per repo from 1;
  `?state=&sort=created&direction=`), issue comments (`POST` → 201
  `{id, body}`, `GET` list), `PATCH /issues/{n}` (closing persists).
- Pulls (`POST /pulls` → 201 open, head and base shas; a duplicate
  open PR → 422; `GET /pulls/{n}/files` with additions/deletions;
  `PUT /pulls/{n}/merge` (squash → 200 `{sha, merged:true, message}`,
  PR closed and merged, squash commit on the default branch, merged
  content readable; 404 unknown, 405 conflicted).

## 3. GraphQL v4 — `POST /api/graphql`

- `{"query","variables"}` plus a bearer token; unauthenticated →
  exactly 401.
- Documents served: ViewerLogin, RepositoryFields, Issue/PullRequest/
  Repository `__type` introspection, SearchType enum (includes ISSUE),
  OpenIssues (CREATED_AT DESC, ties by number DESC, `states:[OPEN]`),
  IssueByNumber, IssueCreate, IssueClose, PullRequests,
  PullRequestFiles, PullRequestId, PullRequestCreate,
  MergePullRequest (SQUASH), AddComment, ViewerInvalidField (HTTP 200
  plus `errors` naming the field), NodeRepository, IssuesPage (cursor
  pagination, 5+2 of 7), batched operations (array in, array out, in
  order), unknown repo → `data.repository: null`, no errors.
- Global ids are opaque, stable and decodable (base64url typed tags).

## 4. MCP — `POST /mcp` (JSON-RPC 2.0, Streamable HTTP)

- `server/discover` → `supportedVersions` (a non-empty YYYY-MM-DD
  list including `2026-07-28`), `serverInfo`,
  `capabilities.tools`; the legacy `initialize` plus
  `notifications/initialized` handshake (2025-11-25 or earlier) is
  also supported.
- `tools/list` → at least 6 tools including `create_repo`,
  `get_repo`, `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`; every tool has a non-empty description and
  `inputSchema.type === "object"`; two `tools/list` calls answer
  byte-identical. Plain property names (`owner`, `repo`, `name`,
  `title`, `body`, `head`, `base`, `pull_number`, `state`).
- `tools/call`: results carry both `content` text JSON and
  `structuredContent`; concurrent calls keep their own ids; unknown
  tool → -32602/-32601 (never HTTP 500); a missing required argument
  → `isError:true` or -32602; a non-JSON body → HTTP 400 plus -32700;
  no Authorization → 401.
- Tools act on the SAME store as REST (ids unchanged across surfaces).

## 5. Performance — 4 workloads, each worth at most 50 points

`push` — 200 sequential 1 KiB pushes (T_ref 1640.82 ms) ·
`clone` — 50 concurrent shallow depth-1 clones of a 200-commit repo
(266.941 ms) · `read` — 2,000 GETs of one issue on one keep-alive
connection (2759.715 ms) · `write` — 200 sequential issue creates
(348.965 ms). Scoring:
`round(clamp(25 - 12.5*log2(t/T_ref), 0, 50))`.

## Core flows (the check flows, in build order)

1. Boot → healthz → register → the suite connects.
2. Create a repo (REST) → push the first commit (git) → clone it back
   byte-exact → fetch incremental / shallow / deepen.
3. Contents PUT → file readable → update (sha) → delete → 404.
4. Label → issue → comment → close → list sort and pagination.
5. Branch → contents on the branch → PR → files → squash merge →
   content on the default branch; a second PR on the same file → 405.
6. GraphQL viewer/repo/issues/PRs/mutations mirror the REST state.
7. MCP tools create and read the same objects REST reads back.

## Data model

User (viewer `adaiversen` id 1000; teammate `quinnkeller` 1001;
stranger `olgabauer` 1002) · Repo (id, name, owner, description,
private, defaultBranch, refs, labels, issues, pulls) · GitObject
(type, data) keyed by SHA-1 · Ref (name → oid) · Issue (id, number,
title, body, state, labels, comments) · Comment (id, body) · Label
(name, color) · Pull (id, number, title, body, state, merged,
head/base ref and sha).

## Scenario identities (fixed by the sealed scenario seed — constants, not config)

Viewer login `adaiversen`, id 1000, `adaiversen@example.invalid`;
stranger `olgabauer`, id 1002; org `faint-meadow-org`; first repo
`amber-thistle` id 5000; labels bug/enhancement/documentation/
question/good first issue/help wanted; default branch `main`; epoch
2025-01-01T00:00:00Z (`MAKEFUN_TRIAL_NOW` overrides).

## Cut explicitly (not in the contract)

Design tokens, component specs, screenshots and visual diffs, demo
seed data, extra GitHub endpoints (search, users, rate_limit beyond
`/meta`), pack deltas on write (whole objects only; deltas resolved
on read), v0 multi_ack_detailed negotiation beyond NAK/ACK, OFS-delta
packs on write.

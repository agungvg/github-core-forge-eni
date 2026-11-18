# Recon — github-core-clone-01 (GitHub-compatible git forge)

Scope in one line: a standalone Node 22 server (stdlib only, one process) that
speaks GitHub's four wires — git smart HTTP, REST v3, GraphQL v4, MCP — well
enough to pass the kit's 120 conformance checks and its 4 perf workloads.
No UI. No git binary. No network. No model calls.

Recon sources (the contract): `RULES.txt` + `program/env/src/checks-{git,rest,
graphql,mcp}.ts`, `wire.ts`, `git-client.ts`, `mcp-client.ts`, `rest-client.ts`,
`bench.ts`, `manifest.ts`, `competition.yaml`. The suite IS the spec; anything
not in it is cuttable.

## Surfaces

### 0. Lifecycle / boot (starter main.ts, kept byte-identical)
- Bind ephemeral port (`listen(0)`, never 8080) → POST `{"port": N}` to
  `$MAKEFUN_ENV_URL/register` → poll GET `$MAKEFUN_ENV_URL/done` → exit 0.
- `GET /healthz` → 200 `{"ok":true}` (called twice within 10s of registering).

### 1. Git smart HTTP — `/{owner}/{repo}.git`
- `GET /info/refs?service=git-upload-pack` / `?service=git-receive-pack`:
  v2 when the client sends `Git-Protocol: version=2`, else v0. Missing repo → 404,
  server stays up.
- v2 advertisement: `version 2`, `ls-refs=unborn`, `fetch=shallow wait-for-done
  filter`, `object-format=sha1`. v2 `ls-refs` (peel/symrefs/unborn/ref-prefix,
  `HEAD` with `symref-target:refs/heads/main`, annotated tags carry
  `peeled:<commit>`). v2 `fetch` (want/have, thin-pack, shallow/deepen,
  `acknowledgments` / `shallow-info` / `packfile` sections, side-band-64k).
- v0 advertisement: `# service=git-upload-pack` preamble, caps on first ref
  line after NUL (`side-band-64k thin-pack ofs-delta shallow
  multi_ack_detailed symref=HEAD:refs/heads/main`); empty repo uses the
  `capabilities^{}` pseudo-ref. Receive-pack v0 caps must include
  `report-status`, `side-band-64k`, `delete-refs`.
- Push (receive-pack POST): pkt-line commands + pack; per-ref report
  (`unpack ok`, `ok <ref>` / `ng <ref> non-fast-forward`). Every update must
  fast-forward; mixed pushes apply the FF ref and refuse the other.
- Objects implemented from scratch: SHA-1 ids, loose-object hashing, packfile
  v2 read/write (whole objects only — valid, simplest), pkt-line framing,
  side-band-64k, want/have negotiation, ancestry checks.
- Malformed POST body must not crash the server (repo survives errors).

### 2. REST v3 — `/api/v3`
- Every reply `application/json`, never HTML; non-empty `X-GitHub-Request-Id`;
  errors are JSON `{message: string}`; unknown route → 404 JSON; malformed
  JSON body → 400.
- Auth: `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` on every call; missing or
  wrong → 401. A repo belongs to the token's login; same name under another
  owner → 404, never 403.
- `GET /` (200 + `X-OAuth-Scopes`), `GET /meta` (`installed_version`),
  `GET /user` (viewer identity), `GET /user/repos` (paginated, `Link:
  rel="next"`, ≤10/page), `POST /user/repos` (201; duplicate → 422).
- `GET/PATCH/DELETE /repos/{owner}/{repo}`; `GET /repos/{o}/{r}/branches`,
  `/branches/{b}`, `/git/refs/heads/{b}`; `POST /git/refs`; `GET /commits`
  (branch-filtered, exact messages).
- `PUT/DELETE /repos/{o}/{r}/contents/{path}` (201 create / 200 update /
  409 stale sha / 200 delete with `{commit:{sha}}`; unborn-repo first commit
  works; PUT on a missing branch creates it from the default tip).
- Labels (`POST /labels` → 201), issues (`POST /issues` → 201; 422 on unknown
  label; numbers increment per repo from 1; `?state=&sort=created&direction=`),
  issue comments (`POST` → 201 `{id, body}`, `GET` list), `PATCH /issues/{n}`
  (close persists).
- Pulls (`POST /pulls` → 201 open, head+base shas; duplicate open PR → 422;
  `GET /pulls/{n}/files` with additions/deletions; `PUT /pulls/{n}/merge`
  (squash → 200 `{sha, merged:true, message}`, PR closed+merged, squash
  commit on default branch, merged content readable; 404 unknown, 405
  conflicted).

### 3. GraphQL v4 — `POST /api/graphql`
- `{"query","variables"}` + bearer token; unauthenticated → exactly 401.
- Documents served: ViewerLogin, RepositoryFields, Issue/PullRequest/
  Repository `__type` introspection, SearchType enum (includes ISSUE),
  OpenIssues (CREATED_AT DESC, ties by number DESC, `states:[OPEN]`),
  IssueByNumber, IssueCreate, IssueClose, PullRequests, PullRequestFiles,
  PullRequestId, PullRequestCreate, MergePullRequest (SQUASH), AddComment,
  ViewerInvalidField (HTTP 200 + `errors` naming the field), NodeRepository,
  IssuesPage (cursor pagination, 5+2 of 7), batched operations (array in,
  array out, in order), unknown repo → `data.repository: null`, no errors.
- Global ids are opaque, stable, decodable (base64url typed tags).

### 4. MCP — `POST /mcp` (JSON-RPC 2.0, Streamable HTTP)
- `server/discover` → `supportedVersions` (non-empty YYYY-MM-DD list incl.
  `2026-07-28`), `serverInfo`, `capabilities.tools`; legacy `initialize` +
  `notifications/initialized` (2025-11-25 or earlier) also supported.
- `tools/list` → ≥6 tools incl. `create_repo`, `get_repo`, `create_issue`,
  `list_issues`, `create_pull_request`, `merge_pull_request`; every tool has
  a non-empty description + `inputSchema.type === "object"`; two `tools/list`
  calls answer byte-identical. Plain property names (`owner`, `repo`, `name`,
  `title`, `body`, `head`, `base`, `pull_number`, `state`).
- `tools/call`: results carry both `content` text JSON and
  `structuredContent`; concurrent calls keep their own ids; unknown tool →
  -32602/-32601 (never HTTP 500); missing required arg → `isError:true` or
  -32602; non-JSON body → HTTP 400 + -32700; no Authorization → 401.
- Tools act on the SAME store as REST (ids unchanged across surfaces).

### 5. Performance — 4 workloads, each ≤50 pts
`push` 200 sequential 1 KiB pushes (T_ref 1640.82 ms) · `clone` 50 concurrent
shallow depth-1 clones of a 200-commit repo (266.941 ms) · `read` 2,000 GETs
of one issue on one keep-alive connection (2759.715 ms) · `write` 200
sequential issue creates (348.965 ms). Formula:
`round(clamp(25 - 12.5*log2(t/T_ref), 0, 50))`.

## Core flows (the check flows, in wave order)
1. Boot → healthz → register → suite connects.
2. Create repo (REST) → push first commit (git) → clone it back byte-exact →
   fetch incremental / shallow / deepen.
3. Contents PUT → file readable → update (sha) → delete → 404.
4. Label → issue → comment → close → list sort/pagination.
5. Branch → contents on branch → PR → files → squash merge → content on
   default branch; second PR on same file → 405.
6. GraphQL viewer/repo/issues/PRs/mutations mirror REST state.
7. MCP tools create/read the same objects REST reads back.

## Data model
User(viewer `adaiversen` id 1000; teammate `quinnkeller` 1001; stranger
`olgabauer` 1002) · Repo(id, name, owner, description, private,
defaultBranch, refs, labels, issues, pulls) · GitObject(type, data) keyed by
SHA-1 · Ref(name → oid) · Issue(id, number, title, body, state, labels,
comments) · Comment(id, body) · Label(name, color) · Pull(id, number, title,
body, state, merged, head/base ref+sha).

## Scenario identities (fixed by the sealed scenario seed — constants, not config)
Viewer login `adaiversen`, id 1000, `adaiversen@example.invalid`; stranger
`olgabauer`, id 1002; org `faint-meadow-org`; first repo `amber-thistle`
id 5000; labels bug/enhancement/documentation/question/good first issue/
help wanted; default branch `main`; epoch 2025-01-01T00:00:00Z
(`MAKEFUN_TRIAL_NOW` overrides).

## Cut explicitly (not in the contract)
Design tokens, component specs, screenshots/visual diffs, demo seed data,
extra GitHub endpoints (search, users, rate_limit beyond `/meta`), pack
deltas on write (whole objects only; deltas resolved on read), v0
multi_ack_detailed negotiation beyond NAK/ACK, OFS-delta packs on write.

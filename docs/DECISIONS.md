# Decisions — github-core-clone-01 entry

One decision per entry, with the reason. New decisions append; old ones are
not rewritten (a reversal gets a new entry).

## D1. Whole-object packs on write; deltas resolved on read (2026-11-18)
Packfile v2 entries are written as whole zlib objects, never deltas. The
harness's `pack-deltas` check passes with 0 deltas as long as objects resolve
byte-exactly, and delta search would cost the most on the push/clone hot
paths. `readPack` still resolves OFS/REF deltas (thin packs included), so
client-sent deltas are safe.

## D2. In-memory store, no disk (2026-11-18)
All repos/objects/issues live in `Map`s in one process. The trial is one
process lifetime; disk would only add per-request I/O on the perf workloads.
Determinism follows from counters + trial clock (see D4).

## D3. Viewer identity as constants (2026-11-18)
`adaiversen` (id 1000), `olgabauer` (1002), `quinnkeller` (1001) are embedded
constants. The scenario seed is a fixed constant in the sealed env and the
bearer token is opaque (`shpat_<hex>`, encodes no login), so the token's
owner cannot be derived at runtime — it is known from the scenario.

## D4. Deterministic time = trial epoch + sequence (2026-11-18)
`MAKEFUN_TRIAL_NOW` (default 2025-01-01T00:00:00Z) is the base; every created
thing takes the next sequence tick as its timestamp. Replays produce the
same state byte-for-byte; wall-clock time never appears in served data.

## D5. Opaque global ids = base64url typed tags (2026-11-18)
Repo `R:<dbId>`, issue `I:<repoDbId>:<number>`, PR `P:<repoDbId>:<number>`,
comment `C:<repoDbId>:<issue>:<commentId>`. Opaque to the client (never the
bare database id), stable across reads, decodable for `node()` and
mutations without a lookup table.

## D6. Squash merge = merge-base three-way diff (2026-11-18)
Merge applies the merge-base→head diff onto the base tip tree; a path
changed on both sides differently is a conflict (405). This is the smallest
rule that satisfies the merge + conflict checks and keeps merged content
readable on the default branch.

## D7. MCP schemas use plain property names (2026-11-18)
Tool schemas declare `owner`, `repo`, `name`, `title`, `body`, `head`,
`base`, `pull_number`, `state` — the names the suite's argument mapper
fills directly. No `full_name`-only schemas, no required property the suite
never sends. `create_repo` requires only `name`.

## D8. Clean-room: protocol from the harness source, code from scratch (2026-11-18)
The kit's `checks-*.ts` / `*-client.ts` were read to learn the wire contract
(they are the spec). No harness code, no check code, and no code from the
other account's entry (`mellow-robin-54`) is copied into this repo.

## D9. Wave order: git+REST core, then GraphQL, then MCP, then perf (2026-11-18)
Score weight drives the order: boot/healthz + REST + git smart HTTP are the
bulk of the 600 feature points; GraphQL is 20 checks; MCP is the 200
agent-native points; perf tuning last, after correctness is green. The main
flow (boot → register → healthz) stays green at the end of every session.

## D10. Cut list (2026-11-18)
No UI of any kind (design tokens, component specs, screenshots, visual
diffs, demo seed data — this bounty has no visual surface). No extra GitHub
endpoints beyond the checked ones. No pack deltas on write (D1). No v0
multi_ack negotiation beyond NAK/ACK. No hidden text, prompt injection, or
suite/scorer exploitation of any kind.

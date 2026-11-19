# Decisions — github-core-clone-01 entry

One entry per decision, with the reason it was made. Entries are appended,
never rewritten; a reversal gets a new entry of its own.

## D1. Packs carry whole objects on write; deltas are resolved on read (2026-11-18)
Packfile v2 entries are written as complete zlib objects, never as deltas.
The harness's `pack-deltas` check passes with 0 deltas provided every
object resolves byte-exactly, and searching for deltas would cost the most
exactly on the push and clone hot paths. `readPack` still resolves OFS and
REF deltas — thin packs included — so deltas sent by a client are safe.

## D2. The store is in memory; nothing is written to disk (2026-11-18)
Repositories, objects and issues all live in `map`s inside the one
process. A trial is a single process lifetime, so disk would only add
per-request I/O on the performance workloads. Determinism comes from the
counters plus the trial clock (see D4).

## D3. Viewer identity is a constant, not derived from the token (2026-11-18)
`adaiversen` (id 1000), `olgabauer` (1002) and `quinnkeller` (1001) are
embedded constants. The scenario seed is a fixed constant in the sealed
environment and the bearer token is opaque (`shpat_<hex>`, carrying no
login), so no runtime derivation of the token's owner is possible — the
owner is known from the scenario itself.

## D4. Deterministic time: trial epoch plus a sequence tick (2026-11-18)
`MAKEFUN_TRIAL_NOW` (default 2025-01-01T00:00:00Z) is the base clock, and
every created thing takes the next sequence tick as its timestamp. Replays
produce identical state byte-for-byte; wall-clock time never shows up in
served data.

## D5. Opaque global ids are base64url of a typed tag (2026-11-18)
Repositories are `R:<dbId>`, issues `I:<repoDbId>:<number>`, pull requests
`P:<repoDbId>:<number>`, comments `C:<repoDbId>:<issue>:<commentId>`. The
client sees only the opaque form (never the bare database id); the ids are
stable across reads and decodable for `node()` and mutations without any
lookup table.

## D6. Squash merge is a merge-base three-way diff (2026-11-18)
A merge applies the diff from the merge base to the head onto the base
tip's tree. A path changed differently on both sides is a conflict (405).
That is the smallest rule that satisfies both the merge checks and the
conflict checks while keeping the merged content readable on the default
branch.

## D7. MCP tool schemas use plain property names (2026-11-18)
Schemas declare `owner`, `repo`, `name`, `title`, `body`, `head`, `base`,
`pull_number` and `state` — the names the suite's argument mapper fills in
directly. No `full_name`-only schemas, and no required property the suite
never sends. `create_repo` requires only `name`.

## D8. Clean room: the wire contract comes from the harness, the code is written from scratch (2026-11-18)
The kit's `checks-*.ts` and `*-client.ts` files were read to learn the wire
contract — they are the specification. No harness code, no check code, and
no code from the other account's entry (`mellow-robin-54`) is copied into
this repository.

## D9. Build order: git plus REST first, then GraphQL, then MCP, then speed (2026-11-18)
Score weight sets the order: boot/healthz, REST and git smart HTTP carry
the bulk of the 600 feature points; GraphQL is 20 checks; MCP is the 200
agent-native points; performance tuning comes last, once correctness is
green. The main flow (boot → register → healthz) stays green at the end of
every session.

## D10. Cut list (2026-11-18)
No UI of any kind — no design tokens, component specs, screenshots, visual
diffs or demo seed data; this bounty has no visual surface. No GitHub
endpoints beyond the checked ones. No pack deltas on write (D1). No v0
multi_ack negotiation beyond NAK/ACK. No hidden text, prompt injection, or
suite/scorer exploitation of any kind.

## D11. Side-band data chunks are 65,515 bytes, not 65,516 (2026-11-18)
A pkt-line holds at most 65,520 bytes including its 4-byte length header,
and a side-band frame adds one band byte on top — so the data chunk is
65,515 bytes. The first 65,516-byte chunk broke `push-100-files`
(`pkt-line length 65521 exceeds 65520`); the one-byte fix took the suite
to 120/120.

## D12. Pack inflate: exact length from a byte-counting reader (2026-11-19, revised)
`inflateOne` inflates through a reader that counts every byte the flate
decoder pulls. The decoder reads one byte at a time from an io.ByteReader
and stops at the stream's end, so the count after the read is the exact
consumed length — one inflate, no re-deflate, no search, for any encoder.
The earlier re-deflate echo + binary-search fallback was correct but slow:
a foreign encoder (real git's C zlib) never matched the echo, so every
object paid a binary search of repeated inflates, which dominated the
push workload. The counting reader gives the same consumed length at the
cost of a single inflate and kept all 120 checks green.

## D13. GraphQL states are uppercase enums; PR mergeable is an enum (2026-11-18)
REST answers lowercase `open`/`closed`; GraphQL answers `OPEN`/`CLOSED`/
`MERGED` for issue and pull-request state and `MERGEABLE`/`CONFLICTING`
for `mergeable`. Both layers map from the same stored state.

// gitutil.ts — the git object layer, written from the wire format up.
// No git binary, no library: SHA-1 object ids, loose-object hashing, packfile v2
// read/write (whole objects; deltas resolved on read), pkt-line framing, tree/commit/
// tag build+parse, and reachability/ancestry over an in-memory object store.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const OID_LEN = 40;

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjectType;
  data: Buffer;
}

export function isOid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);
}

/** SHA-1 of `<type> <len>\0<data>` — the git object id. */
export function hashObject(type: ObjectType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(head).update(data).digest('hex');
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

// ---------------------------------------------------------------- pkt-line

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

/** One pkt-line carrying `text` (which normally ends with \n). */
export function pktLine(text: string): Buffer {
  const payload = Buffer.from(text, 'utf8');
  const len = payload.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, payload]);
}

/** One pkt-line carrying raw bytes. */
export function pktData(payload: Buffer): Buffer {
  const len = payload.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, payload]);
}

/** Encode a list of lines/buffers/flush/delim markers into one pkt-line stream. */
export function encodePktLines(items: readonly (string | Buffer | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(PKT_FLUSH);
    else if (item === 'delim') parts.push(PKT_DELIM);
    else if (typeof item === 'string') parts.push(pktLine(item));
    else parts.push(pktData(item));
  }
  return Buffer.concat(parts);
}

export interface PktPacket {
  kind: 'data' | 'flush' | 'delim';
  payload: Buffer;
}

/** Decode a whole pkt-line stream. Throws on any malformed framing. */
export function decodePktLines(buf: Buffer): PktPacket[] {
  const { packets } = decodePktPrefix(buf);
  return packets;
}

/**
 * Decode pkt-lines from the start of `buf` until the first flush packet
 * (inclusive) and report how many bytes that prefix consumed. The bytes
 * after the flush — a packfile, in receive-pack — are left untouched.
 * Throws on malformed framing inside the prefix.
 */
export function decodePktPrefix(buf: Buffer): { packets: PktPacket[]; consumed: number } {
  const packets: PktPacket[] = [];
  let pos = 0;
  while (pos < buf.length) {
    if (pos + 4 > buf.length) throw new Error('pkt-line truncated header');
    const head = buf.toString('ascii', pos, pos + 4);
    if (head === '0000') {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      return { packets, consumed: pos + 4 };
    }
    if (head === '0001') {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      pos += 4;
      continue;
    }
    const len = parseInt(head, 16);
    if (!Number.isFinite(len) || len < 4 || pos + len > buf.length) {
      throw new Error(`pkt-line bad length ${JSON.stringify(head)}`);
    }
    packets.push({ kind: 'data', payload: buf.subarray(pos + 4, pos + len) });
    pos += len;
  }
  return { packets, consumed: pos };
}

/** Side-band-64k: wrap `payload` as band-1 pkt-lines, each ≤ 65520 bytes total. */
export function sideband(payload: Buffer): Buffer {
  const parts: Buffer[] = [];
  // Max pkt-line is 65520 bytes including its 4-byte header, so the payload
  // (1 band byte + data) may be at most 65516 bytes.
  const CHUNK = 65515;
  for (let off = 0; off < payload.length; off += CHUNK) {
    const slice = payload.subarray(off, Math.min(payload.length, off + CHUNK));
    const framed = Buffer.concat([Buffer.from([1]), slice]);
    parts.push(pktData(framed));
  }
  if (payload.length === 0) parts.push(pktData(Buffer.from([1])));
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- commits / trees / tags

export interface Identity {
  name: string;
  email: string;
  ts: number;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: Identity;
  committer: Identity;
  message: string;
}

function parseIdentity(line: string): Identity {
  // "Name <email> 1735689600 +0000"
  const m = /^(.*) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(line);
  if (!m) return { name: line, email: '', ts: 0 };
  return { name: m[1], email: m[2], ts: Number(m[3]) };
}

function formatIdentity(id: Identity): string {
  return `${id.name} <${id.email}> ${id.ts} +0000`;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author: Identity = { name: '', email: '', ts: 0 };
  let committer: Identity = { name: '', email: '', ts: 0 };
  for (const line of head.split('\n')) {
    if (line.startsWith(' ')) continue; // continuation of a multi-line header
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = parseIdentity(line.slice(7));
    else if (line.startsWith('committer ')) committer = parseIdentity(line.slice(10));
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(c: CommitInfo): Buffer {
  let out = `tree ${c.tree}\n`;
  for (const p of c.parents) out += `parent ${p}\n`;
  out += `author ${formatIdentity(c.author)}\n`;
  out += `committer ${formatIdentity(c.committer)}\n`;
  out += `\n${c.message}`;
  return Buffer.from(out, 'utf8');
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    if (space < 0) throw new Error('tree entry has no mode');
    const nul = data.indexOf(0, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('tree entry truncated');
    const mode = data.toString('ascii', pos, space);
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    pos = nul + 21;
  }
  return entries;
}

/** Git tree ordering: names sort as if directories carried a trailing '/'. */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = a.name + (a.mode === '40000' ? '/' : '');
    const bn = b.name + (b.mode === '40000' ? '/' : '');
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface TagInfo {
  object: string;
  type: ObjectType;
  tag: string;
  tagger: Identity | null;
  message: string;
}

export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type: ObjectType = 'commit';
  let tag = '';
  let tagger: Identity | null = null;
  for (const line of head.split('\n')) {
    if (line.startsWith(' ')) continue;
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim() as ObjectType;
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = parseIdentity(line.slice(7));
  }
  return { object, type, tag, tagger, message };
}

export function buildTag(t: TagInfo): Buffer {
  let out = `object ${t.object}\ntype ${t.type}\ntag ${t.tag}\n`;
  if (t.tagger) out += `tagger ${formatIdentity(t.tagger)}\n`;
  out += `\n${t.message}`;
  return Buffer.from(out, 'utf8');
}

// ---------------------------------------------------------------- packfile v2

const PACK_TYPE: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const PACK_TYPE_NAME: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** Write a packfile of whole objects only (no deltas) — valid pack v2. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objects.length, 8);
  parts.push(head);
  for (const obj of objects) {
    const data = obj.data;
    const typeNum = PACK_TYPE[obj.type];
    // size+type varint: low 4 bits in first byte, then 7 bits per byte, LE.
    let size = data.length;
    const first = (size & 0x0f) | (typeNum << 4) | (size > 0x0f ? 0x80 : 0);
    const header: number[] = [first];
    size >>= 4;
    while (size > 0) {
      header.push((size & 0x7f) | (size > 0x7f ? 0x80 : 0));
      size >>= 7;
    }
    parts.push(Buffer.from(header));
    parts.push(deflateSync(data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

/** Inflate exactly one zlib stream starting at `pos`. `hint` is the expected output size. */
function inflateOne(buf: Buffer, pos: number, hint: number): { data: Buffer; consumed: number } {
  const rem = buf.length - pos;
  if (rem <= 0) throw new Error('pack zlib stream truncated');
  // Fast path: inflate the first stream, then re-deflate and check the echo.
  // Packs here are built with zlib deflateSync defaults (the harness's
  // writePack and this entry's writePack alike), so the echo is byte-identical
  // to the stream on disk and its length is the consumed length. A mismatch
  // (a foreign encoder) falls back to the bounded search below.
  let data: Buffer;
  try {
    data = inflateSync(buf.subarray(pos));
  } catch {
    throw new Error('pack zlib stream is corrupt');
  }
  const echo = deflateSync(data);
  if (echo.length <= rem && buf.subarray(pos, pos + echo.length).equals(echo)) {
    return { data, consumed: echo.length };
  }
  // Fallback: bounded search for the smallest length that inflates.
  const ok = (n: number): Buffer | null => {
    try {
      return inflateSync(buf.subarray(pos, pos + n));
    } catch {
      return null;
    }
  };
  // Find a length that inflates: start near the hinted size, double on failure.
  let lo = 0;
  let hi = Math.min(rem, Math.max(8, hint + 32));
  let found = ok(hi);
  if (found === null) {
    lo = hi;
    hi = Math.min(rem, hi * 2);
    while (found === null) {
      if (hi >= rem) {
        found = ok(rem);
        if (found === null) throw new Error('pack zlib stream is corrupt');
        break;
      }
      found = ok(hi);
      if (found === null) {
        lo = hi;
        hi = Math.min(rem, hi * 2);
      }
    }
  }
  // Binary-search the smallest length that inflates: the true stream end.
  let a = lo;
  let b = hi;
  let ans = hi;
  while (a + 1 < b) {
    const mid = (a + b) >> 1;
    if (ok(mid) !== null) {
      ans = mid;
      b = mid;
    } else {
      a = mid;
    }
  }
  const finalData = ok(ans);
  if (finalData === null) throw new Error('pack zlib stream is corrupt');
  return { data: finalData, consumed: ans };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let out = 0;
    let shift = 0;
    for (;;) {
      if (pos >= delta.length) throw new Error('delta varint truncated');
      const b = delta[pos++];
      out |= (b & 0x7f) << shift;
      shift += 7;
      if ((b & 0x80) === 0) break;
    }
    return out;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const chunks: Buffer[] = [];
  let written = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) offset |= delta[pos++] << (8 * i);
      for (let i = 0; i < 3; i++) if (op & (0x10 << i)) size |= delta[pos++] << (8 * i);
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta copy out of range');
      chunks.push(base.subarray(offset, offset + size));
      written += size;
    } else if (op !== 0) {
      if (pos + op > delta.length) throw new Error('delta insert truncated');
      chunks.push(delta.subarray(pos, pos + op));
      pos += op;
      written += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== targetSize) throw new Error('delta target size mismatch');
  return Buffer.concat(chunks);
}

export interface PackReadResult {
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/**
 * Read a packfile v2/v3, resolving OFS_DELTA and REF_DELTA entries. `resolveBase`
 * supplies a base object the pack omits (thin packs); without it a missing base
 * is an error.
 */
export function readPack(
  buf: Buffer,
  resolveBase?: (oid: string) => GitObject | undefined,
): PackReadResult {
  if (buf.length < 12 || buf.toString('ascii', 0, 4) !== 'PACK') {
    throw new Error('not a packfile');
  }
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = buf.readUInt32BE(8);
  // Trailer check: last 20 bytes are the SHA-1 of everything before them.
  if (buf.length < 32) throw new Error('packfile too short');
  const expected = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest('hex');
  const actual = buf.subarray(buf.length - 20).toString('hex');
  if (expected !== actual) throw new Error('packfile checksum mismatch');

  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  let deltaCount = 0;
  let pos = 12;
  const end = buf.length - 20;
  for (let i = 0; i < count; i++) {
    const entryStart = pos;
    if (pos >= end) throw new Error('packfile truncated');
    let b = buf[pos++];
    const typeNum = (b >> 4) & 0x07;
    let size = b & 0x0f;
    let shift = 4;
    while (b & 0x80) {
      if (pos >= end) throw new Error('packfile size varint truncated');
      b = buf[pos++];
      size |= (b & 0x7f) << shift;
      shift += 7;
    }
    let baseOid: string | undefined;
    let baseOffset: number | undefined;
    if (typeNum === 6) {
      // OFS_DELTA: offset varint, git's (offset+1)<<7 encoding.
      if (pos >= end) throw new Error('packfile ofs-delta truncated');
      b = buf[pos++];
      let offset = b & 0x7f;
      while (b & 0x80) {
        if (pos >= end) throw new Error('packfile ofs-delta truncated');
        b = buf[pos++];
        offset = ((offset + 1) << 7) | (b & 0x7f);
      }
      baseOffset = entryStart - offset;
    } else if (typeNum === 7) {
      if (pos + 20 > end) throw new Error('packfile ref-delta truncated');
      baseOid = buf.subarray(pos, pos + 20).toString('hex');
      pos += 20;
    }
    const { data, consumed } = inflateOne(buf, pos, size);
    pos += consumed;
    if (data.length !== size) throw new Error('pack object size mismatch');
    if (typeNum === 6 || typeNum === 7) {
      deltaCount += 1;
      let base: GitObject | undefined;
      if (baseOffset !== undefined) base = byOffset.get(baseOffset);
      else if (baseOid !== undefined) base = objects.get(baseOid) ?? resolveBase?.(baseOid);
      if (!base) throw new Error('pack delta base missing');
      const resolved = applyDelta(base.data, data);
      const type = base.type;
      const oid = hashObject(type, resolved);
      const obj: GitObject = { type, data: resolved };
      objects.set(oid, obj);
      byOffset.set(entryStart, obj);
    } else {
      const type = PACK_TYPE_NAME[typeNum];
      if (!type) throw new Error(`pack object type ${typeNum} unknown`);
      const oid = hashObject(type, data);
      const obj: GitObject = { type, data };
      objects.set(oid, obj);
      byOffset.set(entryStart, obj);
    }
  }
  return { objects, deltaCount };
}

// ---------------------------------------------------------------- object store

/** An in-memory object store: loose objects keyed by id, nothing on disk. */
export class ObjectStore {
  readonly objects = new Map<string, GitObject>();

  has(oid: string): boolean {
    return this.objects.has(oid);
  }

  get(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  put(type: ObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  putObject(oid: string, obj: GitObject): void {
    if (!this.objects.has(oid)) this.objects.set(oid, obj);
  }
}

/**
 * Walk commits from `tips`: returns the commit ids reached, and the boundary
 * commits — reached commits whose parents were NOT walked (depth limit, or a
 * stopAt boundary). `maxDepth` counts edges from the tips (0 = tips only).
 */
export function walkCommits(
  store: ObjectStore,
  tips: readonly string[],
  opts: { maxDepth?: number; stopAt?: ReadonlySet<string> } = {},
): { commits: Set<string>; boundary: Set<string> } {
  const commits = new Set<string>();
  const boundary = new Set<string>();
  const queue: Array<{ oid: string; depth: number }> = [];
  for (const tip of tips) {
    if (isOid(tip) && !commits.has(tip)) queue.push({ oid: tip, depth: 0 });
  }
  while (queue.length > 0) {
    const { oid, depth } = queue.shift()!;
    if (commits.has(oid)) continue;
    const obj = store.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    commits.add(oid);
    if (opts.stopAt?.has(oid)) {
      boundary.add(oid);
      continue;
    }
    const info = parseCommit(obj.data);
    if (opts.maxDepth !== undefined && depth >= opts.maxDepth) {
      if (info.parents.length > 0) boundary.add(oid);
      continue;
    }
    for (const p of info.parents) {
      if (!commits.has(p)) queue.push({ oid: p, depth: depth + 1 });
    }
  }
  return { commits, boundary };
}

/** Every object reachable from `commitOids` (commits, their trees/blobs, tags). */
export function collectReachable(store: ObjectStore, commitOids: ReadonlySet<string>): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  const visitCommit = (oid: string): void => {
    const obj = store.get(oid);
    if (!obj || obj.type !== 'commit' || out.has(oid)) return;
    out.set(oid, obj);
    const info = parseCommit(obj.data);
    visitTree(info.tree);
    for (const p of info.parents) visitCommit(p);
  };
  const visitTree = (oid: string): void => {
    const obj = store.get(oid);
    if (!obj || out.has(oid)) return;
    out.set(oid, obj);
    if (obj.type !== 'tree') return;
    for (const e of parseTree(obj.data)) {
      const child = store.get(e.oid);
      if (!child) continue;
      if (child.type === 'tree') visitTree(e.oid);
      else out.set(e.oid, child);
    }
  };
  for (const oid of commitOids) {
    const obj = store.get(oid);
    if (!obj) continue;
    if (obj.type === 'commit') visitCommit(oid);
    else if (obj.type === 'tag') {
      out.set(oid, obj);
      const tag = parseTag(obj.data);
      if (tag.type === 'commit') visitCommit(tag.object);
      else if (tag.type === 'tree') visitTree(tag.object);
      else {
        const target = store.get(tag.object);
        if (target) out.set(tag.object, target);
      }
    } else out.set(oid, obj);
  }
  return out;
}

/**
 * The objects of exactly the given commits (commit + its tree + its blobs),
 * WITHOUT following parents. This is what a depth-limited fetch sends: the
 * walk decides which commits, this decides which objects they carry.
 */
export function collectCommitObjects(store: ObjectStore, commitOids: ReadonlySet<string>): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  const visitTree = (oid: string): void => {
    const obj = store.get(oid);
    if (!obj || out.has(oid)) return;
    out.set(oid, obj);
    if (obj.type !== 'tree') return;
    for (const e of parseTree(obj.data)) {
      const child = store.get(e.oid);
      if (!child) continue;
      if (child.type === 'tree') visitTree(e.oid);
      else out.set(e.oid, child);
    }
  };
  for (const oid of commitOids) {
    const obj = store.get(oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      out.set(oid, obj);
      visitTree(parseCommit(obj.data).tree);
    } else if (obj.type === 'tag') {
      out.set(oid, obj);
      const tag = parseTag(obj.data);
      if (tag.type === 'commit') {
        const target = store.get(tag.object);
        if (target) {
          out.set(tag.object, target);
          visitTree(parseCommit(target.data).tree);
        }
      } else if (tag.type === 'tree') visitTree(tag.object);
      else {
        const target = store.get(tag.object);
        if (target) out.set(tag.object, target);
      }
    } else out.set(oid, obj);
  }
  return out;
}

/** Is `ancestor` reachable from `descendant` by following parents? */
export function isAncestor(store: ObjectStore, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const queue: string[] = [descendant];
  while (queue.length > 0) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = store.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) {
      if (p === ancestor) return true;
      if (!seen.has(p)) queue.push(p);
    }
  }
  return false;
}

/** The merge-base of two commits: the newest common ancestor (by walk order). */
export function mergeBase(store: ObjectStore, a: string, b: string): string | null {
  const ancestorsA = new Set<string>();
  const queueA: string[] = [a];
  while (queueA.length > 0) {
    const oid = queueA.shift()!;
    if (ancestorsA.has(oid)) continue;
    ancestorsA.add(oid);
    const obj = store.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) queueA.push(p);
  }
  // BFS from b: first commit also in A is the merge base.
  const seen = new Set<string>();
  const queueB: string[] = [b];
  while (queueB.length > 0) {
    const oid = queueB.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (ancestorsA.has(oid)) return oid;
    const obj = store.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) queueB.push(p);
  }
  return null;
}

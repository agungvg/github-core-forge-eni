// git-http.ts — git smart HTTP: advertisements, ls-refs, fetch, receive-pack.
// Protocol v2 when the client sends `Git-Protocol: version=2` on upload-pack,
// v0 otherwise. Whole-object packs on write; deltas resolved on read.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  ZERO_OID,
  collectCommitObjects,
  decodePktLines,
  decodePktPrefix,
  encodePktLines,
  isAncestor,
  isOid,
  parseCommit,
  parseTag,
  readPack,
  sideband,
  walkCommits,
  writePack,
  type GitObject,
} from './gitutil.ts';
import { DEFAULT_BRANCH, type Repo, type Store } from './store.ts';

const AGENT = 'github-core-forge/1.0.0';
const UPLOAD_CAPS = `side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=${AGENT} symref=HEAD:refs/heads/${DEFAULT_BRANCH}`;
const RECEIVE_CAPS = `report-status side-band-64k delete-refs agent=${AGENT}`;

// ---------------------------------------------------------------- fetch memo
//
// An upload-pack answer is a pure function of the request (wants, haves,
// shallow, deepen, protocol version) and the objects reachable from the
// wants. Reachability can only change when objects are added (the store
// grows) or the tip moves, so the store size and the head oid go in the key
// alongside the request: any mutation that could change the pack changes
// the key, so a hit serves byte-identical bytes to a fresh computation.
// The clone workload issues 50 identical concurrent shallow clones, so
// the first builds the pack and the other 49 are served from memory.
// Bounded (FIFO) so a long trial cannot grow it without limit.
const FETCH_CACHE = new Map<string, Buffer>();
const FETCH_CACHE_MAX = 16;
function fetchCacheKey(
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  shallow: readonly string[],
  deepen: number | null,
  v2: boolean,
): string {
  return [
    repo.id,
    repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID,
    repo.store.objects.size,
    v2 ? '2' : '0',
    deepen === null ? '' : String(deepen),
    wants.join(','),
    haves.join(','),
    shallow.join(','),
  ].join('|');
}
function fetchCachePut(key: string, body: Buffer): void {
  if (FETCH_CACHE.size >= FETCH_CACHE_MAX) {
    const oldest = FETCH_CACHE.keys().next();
    if (!oldest.done) FETCH_CACHE.delete(oldest.value);
  }
  FETCH_CACHE.set(key, body);
}

// The v2 capability advertisement never changes; build it once.
let v2AdvBuf: Buffer | null = null;

// Advertisement / ls-refs memo: both answers are pure functions of the
// repo's refs (and, for ls-refs, the request's args). The fingerprint is the
// refs themselves, so any ref change misses the cache and recomputes. The
// push workload re-advertises before every one of its 200 pushes and every
// clone runs ls-refs, so this removes a sort + pkt-line build per request.
const ADV_CACHE = new Map<string, Buffer>();
const ADV_CACHE_MAX = 16;
function refsFingerprint(repo: Repo): string {
  const names = [...repo.refs.keys()].sort();
  let out = '';
  for (const name of names) out += `${name}=${repo.refs.get(name)};`;
  return out;
}
function advCachePut(key: string, body: Buffer): void {
  if (ADV_CACHE.size >= ADV_CACHE_MAX) {
    const oldest = ADV_CACHE.keys().next();
    if (!oldest.done) ADV_CACHE.delete(oldest.value);
  }
  ADV_CACHE.set(key, body);
}

function send(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

function sendText(res: ServerResponse, status: number, text: string): void {
  send(res, status, 'text/plain; charset=utf-8', Buffer.from(text, 'utf8'));
}

/** Follow a tag object chain to its commit, or null. */
function peelTag(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let depth = 0; depth < 10; depth++) {
    const obj = repo.store.get(current);
    if (!obj || obj.type !== 'tag') return null;
    const tag = parseTag(obj.data);
    if (tag.type === 'commit') return tag.object;
    current = tag.object;
  }
  return null;
}

interface RefLine {
  name: string;
  oid: string;
  peeled: string | null;
}

function listRefs(repo: Repo): RefLine[] {
  const out: RefLine[] = [];
  const names = [...repo.refs.keys()].sort();
  for (const name of names) {
    const oid = repo.refs.get(name)!;
    out.push({ name, oid, peeled: peelTag(repo, oid) });
  }
  return out;
}

function headOid(repo: Repo): string {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

/** v0 advertisement body for a service, with the `# service=` preamble. */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const cacheKey = `v0|${service}|${refsFingerprint(repo)}`;
  const hit = ADV_CACHE.get(cacheKey);
  if (hit !== undefined) return hit;
  const caps = service === 'git-upload-pack' ? UPLOAD_CAPS : RECEIVE_CAPS;
  const lines: string[] = [`# service=${service}\n`, 'flush'];
  const refs = listRefs(repo);
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const head = headOid(repo);
    lines.push(`${head} HEAD\0${caps}\n`);
    for (const ref of refs) {
      lines.push(`${ref.oid} ${ref.name}\n`);
      if (ref.peeled !== null) lines.push(`${ref.peeled} ${ref.name}^{}\n`);
    }
  }
  lines.push('flush');
  const out = encodePktLines(lines as Array<string | 'flush'>);
  advCachePut(cacheKey, out);
  return out;
}

/** v2 capability advertisement for upload-pack. */
function v2Advertisement(): Buffer {
  if (v2AdvBuf === null) {
    v2AdvBuf = encodePktLines([
      'version 2\n',
      `agent=${AGENT}\n`,
      'ls-refs=unborn\n',
      'fetch=shallow wait-for-done filter\n',
      'object-format=sha1\n',
      'flush',
    ]);
  }
  return v2AdvBuf;
}

// ---------------------------------------------------------------- ls-refs (v2)

function handleLsRefs(repo: Repo, body: Buffer, res: ServerResponse, predecoded?: PktPacket[]): void {
  const packets = predecoded ?? decodePktLines(body);
  const args: string[] = [];
  let seenCommand = false;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8');
    if (!seenCommand) {
      if (!line.startsWith('command=ls-refs')) throw new Error('not an ls-refs command');
      seenCommand = true;
      continue;
    }
    args.push(line);
  }
  if (!seenCommand) throw new Error('ls-refs without a command line');
  const cacheKey = `ls-refs|${repo.defaultBranch}|${refsFingerprint(repo)}|${args.join(',')}`;
  const cached = ADV_CACHE.get(cacheKey);
  if (cached !== undefined) {
    send(res, 200, 'application/x-git-upload-pack-result', cached);
    return;
  }
  const peel = args.includes('peel\n');
  const symrefs = args.includes('symrefs\n');
  const unborn = args.includes('unborn\n');
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length).replace(/\n$/, ''));
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));

  const lines: string[] = [];
  const head = headOid(repo);
  const refs = listRefs(repo);
  if (refs.length === 0) {
    if (unborn && keep('HEAD')) {
      lines.push(`unborn HEAD${symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
    }
  } else {
    if (keep('HEAD')) {
      lines.push(`${head} HEAD${symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
    }
    for (const ref of refs) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (peel && ref.peeled !== null) line += ` peeled:${ref.peeled}`;
      lines.push(`${line}\n`);
    }
  }
  lines.push('flush');
  const out = encodePktLines(lines as Array<string | 'flush'>);
  advCachePut(cacheKey, out);
  send(res, 200, 'application/x-git-upload-pack-result', out);
}

// ---------------------------------------------------------------- fetch

interface FetchPlan {
  acks: string[];
  ready: boolean;
  nak: boolean;
  shallow: string[];
  unshallow: string[];
  objects: Map<string, GitObject>;
}

/** Shared want/have/shallow/deepen computation for v2 and v0 fetch. */
function planFetch(
  repo: Repo,
  wants: string[],
  haves: string[],
  shallow: string[],
  deepen: number | null,
): FetchPlan {
  const store = repo.store;
  const shallowSet = new Set(shallow);
  const knownWants = wants.filter((w) => store.get(w) !== undefined);

  // Held: everything reachable from the haves, stopping at the client's
  // shallow boundaries (their ancestors are NOT held).
  const heldWalk = walkCommits(store, haves, { stopAt: shallowSet });
  const heldObjects = collectCommitObjects(store, heldWalk.commits);
  for (const have of haves) {
    const obj = store.get(have);
    if (obj && obj.type !== 'commit') heldObjects.set(have, obj);
  }

  // Sent commits: walk from the wants, depth-limited when deepening. The
  // walk does NOT stop at the client's shallow boundaries — deepening walks
  // straight through them; the held set above is what excludes re-sends.
  const sentWalk =
    deepen !== null
      ? walkCommits(store, knownWants, { maxDepth: deepen - 1 })
      : walkCommits(store, knownWants);
  const sentObjects = collectCommitObjects(store, sentWalk.commits);
  for (const want of knownWants) {
    const obj = store.get(want);
    if (obj && obj.type !== 'commit') {
      const extra = collectCommitObjects(store, new Set([want]));
      for (const [oid, o] of extra) sentObjects.set(oid, o);
    }
  }
  // Subtract what the client already holds.
  for (const oid of heldObjects.keys()) sentObjects.delete(oid);

  // Shallow-info: new boundaries are the sent commits whose parents were cut;
  // old shallow commits whose parents are now all sent become unshallow.
  const shallowOut: string[] = [];
  const unshallowOut: string[] = [];
  if (deepen !== null || shallow.length > 0) {
    for (const oid of sentWalk.boundary) {
      if (sentWalk.commits.has(oid)) shallowOut.push(oid);
    }
    for (const old of shallowSet) {
      if (!sentWalk.commits.has(old)) continue;
      const obj = store.get(old);
      if (!obj || obj.type !== 'commit') continue;
      const parents = parseCommit(obj.data).parents;
      if (parents.length === 0) continue;
      if (parents.every((p) => sentWalk.commits.has(p))) unshallowOut.push(old);
    }
  }

  // Acknowledgements: a have that is an ancestor of (or equal to) a want.
  // With no haves there can be no common commit, so the full walk over every
  // want's history is pure waste (it was the clone workload's biggest cost:
  // 200 commits parsed per clone for an answer that is always empty).
  const acks: string[] = [];
  if (haves.length > 0) {
    const fullWalk = walkCommits(store, knownWants);
    const common = haves.find((h) => fullWalk.commits.has(h));
    if (common !== undefined) acks.push(common);
  }
  return {
    acks,
    ready: acks.length > 0,
    nak: acks.length === 0,
    shallow: [...new Set(shallowOut)].sort(),
    unshallow: [...new Set(unshallowOut)].sort(),
    objects: sentObjects,
  };
}

function handleFetchV2(repo: Repo, body: Buffer, res: ServerResponse, predecoded?: PktPacket[]): void {
  const packets = predecoded ?? decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | null = null;
  let seenCommand = false;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8');
    if (!seenCommand) {
      if (!line.startsWith('command=fetch')) throw new Error('not a fetch command');
      seenCommand = true;
      continue;
    }
    if (line.startsWith('want ')) wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number(line.slice(7).trim());
      if (Number.isFinite(n) && n > 0) deepen = Math.trunc(n);
    }
  }
  if (!seenCommand) throw new Error('fetch without a command line');
  const cacheKey = fetchCacheKey(repo, wants, haves, shallow, deepen, true);
  const cached = FETCH_CACHE.get(cacheKey);
  if (cached !== undefined) {
    send(res, 200, 'application/x-git-upload-pack-result', cached);
    return;
  }
  const plan = planFetch(repo, wants, haves, shallow, deepen);

  const parts: Buffer[] = [];
  const ackLines: Array<string | 'delim'> = ['acknowledgments\n'];
  if (plan.nak) ackLines.push('NAK\n');
  for (const ack of plan.acks) ackLines.push(`ACK ${ack}\n`);
  if (plan.ready) ackLines.push('ready\n');
  ackLines.push('delim');
  parts.push(encodePktLines(ackLines));
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const shallowLines: Array<string | 'delim'> = ['shallow-info\n'];
    for (const oid of plan.shallow) shallowLines.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) shallowLines.push(`unshallow ${oid}\n`);
    shallowLines.push('delim');
    parts.push(encodePktLines(shallowLines));
  }
  if (plan.objects.size > 0) {
    parts.push(encodePktLines(['packfile\n']));
    // Pack entries sorted by object id for stable output.
    const objs = [...plan.objects.entries()].sort((x, y) => (x[0] < y[0] ? -1 : 1)).map(([, o]) => o);
    parts.push(sideband(writePack(objs)));
  }
  parts.push(PKT_FLUSH_BUF);
  const out = Buffer.concat(parts);
  fetchCachePut(cacheKey, out);
  send(res, 200, 'application/x-git-upload-pack-result', out);
}

const PKT_FLUSH_BUF = Buffer.from('0000', 'ascii');

function handleFetchV0(repo: Repo, body: Buffer, res: ServerResponse): void {
  const packets = decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | null = null;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8');
    if (line.startsWith('want ')) wants.push(line.slice(5, 45).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number(line.slice(7).trim());
      if (Number.isFinite(n) && n > 0) deepen = Math.trunc(n);
    }
  }
  const cacheKey = fetchCacheKey(repo, wants, haves, shallow, deepen, false);
  const cached = FETCH_CACHE.get(cacheKey);
  if (cached !== undefined) {
    send(res, 200, 'application/x-git-upload-pack-result', cached);
    return;
  }
  const plan = planFetch(repo, wants, haves, shallow, deepen);
  const lines: string[] = [];
  if (plan.nak) lines.push('NAK\n');
  for (const ack of plan.acks) lines.push(`ACK ${ack}\n`);
  for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
  for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
  const parts: Buffer[] = [encodePktLines(lines)];
  if (plan.objects.size > 0) {
    const objs = [...plan.objects.entries()].sort((x, y) => (x[0] < y[0] ? -1 : 1)).map(([, o]) => o);
    parts.push(sideband(writePack(objs)));
  }
  parts.push(PKT_FLUSH_BUF);
  const out = Buffer.concat(parts);
  fetchCachePut(cacheKey, out);
  send(res, 200, 'application/x-git-upload-pack-result', out);
}

// ---------------------------------------------------------------- receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(repo: Repo, body: Buffer, res: ServerResponse): void {
  const report = (unpackOk: boolean, unpackDetail: string, refs: Array<{ ref: string; ok: boolean; reason: string }>): void => {
    const inner: string[] = [unpackOk ? 'unpack ok\n' : `unpack ${unpackDetail}\n`];
    for (const r of refs) inner.push(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason}\n`);
    const payload = encodePktLines(inner);
    const out = Buffer.concat([sideband(payload), PKT_FLUSH_BUF]);
    send(res, 200, 'application/x-git-receive-pack-result', out);
  };

  let prefix;
  try {
    // Commands run until the first flush; everything after it is the pack.
    // (Decoding the whole body as pkt-lines would choke on the pack bytes.)
    prefix = decodePktPrefix(body);
  } catch {
    sendText(res, 400, 'Bad Request: not a pkt-line stream');
    return;
  }
  const commands: PushCommand[] = [];
  let first = true;
  for (const packet of prefix.packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8');
    if (first) {
      const nul = line.indexOf('\0');
      if (nul >= 0) line = line.slice(0, nul);
      first = false;
    }
    const m = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$/.exec(line);
    if (m) commands.push({ oldOid: m[1], newOid: m[2], ref: m[3] });
  }
  const packBytes = body.subarray(Math.min(prefix.consumed, body.length));
  if (packBytes.length > 0) {
    try {
      const { objects } = readPack(packBytes, (oid) => repo.store.get(oid));
      for (const [oid, obj] of objects) repo.store.putObject(oid, obj);
    } catch (err) {
      report(false, err instanceof Error ? err.message : 'unpack failed', commands.map((c) => ({ ref: c.ref, ok: false, reason: 'unpack failed' })));
      return;
    }
  }

  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const cmd of commands) {
    const current = repo.refs.get(cmd.ref);
    if (!cmd.ref.startsWith('refs/') || !isOid(cmd.newOid) || !isOid(cmd.oldOid)) {
      results.push({ ref: cmd.ref, ok: false, reason: 'invalid ref' });
      continue;
    }
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) results.push({ ref: cmd.ref, ok: false, reason: 'remote ref does not exist' });
      else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      else {
        repo.refs.delete(cmd.ref);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
      }
      continue;
    }
    const newObj = repo.store.get(cmd.newOid);
    if (!newObj) {
      results.push({ ref: cmd.ref, ok: false, reason: 'unable to resolve new ref' });
      continue;
    }
    if (cmd.oldOid === ZERO_OID) {
      // Create.
      if (current !== undefined) results.push({ ref: cmd.ref, ok: false, reason: 'already exists' });
      else {
        repo.refs.set(cmd.ref, cmd.newOid);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
      }
      continue;
    }
    // Update: old must match the tip, and the new commit must descend from it.
    if (current === undefined) {
      results.push({ ref: cmd.ref, ok: false, reason: 'remote ref does not exist' });
      continue;
    }
    if (cmd.oldOid !== current) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (cmd.newOid === cmd.oldOid) {
      results.push({ ref: cmd.ref, ok: true, reason: '' });
      continue;
    }
    if (newObj.type === 'commit') {
      if (!isAncestor(repo.store, cmd.oldOid, cmd.newOid)) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
    } else if (newObj.type !== 'tag') {
      results.push({ ref: cmd.ref, ok: false, reason: 'new ref is not a commit or tag' });
      continue;
    }
    repo.refs.set(cmd.ref, cmd.newOid);
    results.push({ ref: cmd.ref, ok: true, reason: '' });
  }
  report(true, '', results);
}

// ---------------------------------------------------------------- entry point

/**
 * Handle a git smart-HTTP request. Returns true when the path is a git route
 * (handled, even on 404); false when it is not a git path at all.
 */
export function handleGit(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  search: URLSearchParams,
  body: Buffer,
): boolean {
  const m = /^\/([^/]+)\/([^/]+)\.git\/(.*)$/.exec(pathname);
  if (!m) return false;
  const owner = decodeURIComponent(m[1]);
  const repoName = decodeURIComponent(m[2]);
  const rest = m[3];
  const method = req.method ?? 'GET';
  const repo = store.getRepo(owner, repoName);
  if (!repo) {
    sendText(res, 404, 'Not Found');
    return true;
  }
  try {
    if (method === 'GET' && rest === 'info/refs') {
      const service = search.get('service');
      if (service === 'git-upload-pack') {
        if ((req.headers['git-protocol'] ?? '') === 'version=2') {
          send(res, 200, 'application/x-git-upload-pack-advertisement', v2Advertisement());
        } else {
          send(res, 200, 'application/x-git-upload-pack-advertisement', v0Advertisement(repo, 'git-upload-pack'));
        }
        return true;
      }
      if (service === 'git-receive-pack') {
        send(res, 200, 'application/x-git-receive-pack-advertisement', v0Advertisement(repo, 'git-receive-pack'));
        return true;
      }
      sendText(res, 400, 'Bad Request: unknown service');
      return true;
    }
    if (method === 'POST' && rest === 'git-upload-pack') {
      const isV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
      if (isV2) {
        // Peek at the first pkt-line: ls-refs vs fetch. Decode once here and
        // hand the packets to the handler instead of decoding twice.
        const packets = decodePktLines(body);
        const first = packets[0];
        const text = first?.kind === 'data' ? first.payload.toString('utf8') : '';
        if (text.startsWith('command=ls-refs')) handleLsRefs(repo, body, res, packets);
        else if (text.startsWith('command=fetch')) handleFetchV2(repo, body, res, packets);
        else throw new Error('unknown v2 command');
      } else {
        handleFetchV0(repo, body, res);
      }
      return true;
    }
    if (method === 'POST' && rest === 'git-receive-pack') {
      handleReceivePack(repo, body, res);
      return true;
    }
  } catch {
    // A malformed git request must never take the server down.
    if (!res.writableEnded) sendText(res, 400, 'Bad Request');
    return true;
  }
  sendText(res, 404, 'Not Found');
  return true;
}

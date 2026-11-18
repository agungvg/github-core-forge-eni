// store.ts — the forge's whole state, in memory.
// One Store holds every repository: its git objects and refs, its issues, labels,
// comments and pull requests. Everything is deterministic: ids and numbers come
// from counters, timestamps from MAKEFUN_TRIAL_NOW plus a sequence (never the
// wall clock), so a replayed trial leaves the same state byte-for-byte.

import {
  ObjectStore,
  ZERO_OID,
  buildCommit,
  buildTree,
  hashObject,
  isOid,
  mergeBase,
  parseCommit,
  parseTree,
  type CommitInfo,
  type Identity,
  type TreeEntry,
} from './gitutil.ts';

export const VIEWER = {
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
} as const;
export const TEAMMATE = { login: 'quinnkeller', id: 1001, name: 'quinn keller', email: 'quinnkeller@example.invalid' } as const;
export const STRANGER = {
  login: 'olgabauer',
  id: 1002,
  name: 'olga bauer',
  email: 'olgabauer@example.invalid',
} as const;
export const DEFAULT_BRANCH = 'main';
export const SCENARIO_EPOCH_SEC = 1735689600; // 2025-01-01T00:00:00Z

export function trialEpochSec(): number {
  const raw = process.env.MAKEFUN_TRIAL_NOW ?? '';
  if (raw !== '') {
    const asNum = Number(raw);
    if (Number.isFinite(asNum) && asNum > 0) return asNum > 1e12 ? Math.floor(asNum / 1000) : Math.floor(asNum);
    const parsed = Date.parse(raw);
    if (Number.isFinite(parsed)) return Math.floor(parsed / 1000);
  }
  return SCENARIO_EPOCH_SEC;
}

export function viewerIdentity(ts: number): Identity {
  return { name: VIEWER.name, email: VIEWER.email, ts };
}

/** Opaque, deterministic global ids: base64url of a typed tag. */
export function globalId(tag: string, ...parts: (string | number)[]): string {
  return Buffer.from([tag, ...parts.map(String)].join(':'), 'utf8').toString('base64url');
}

export function decodeGlobalId(id: string): { tag: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const parts = text.split(':');
    const tag = parts.shift();
    if (!tag || !/^[A-Z]$/.test(tag)) return null;
    return { tag, parts };
  } catch {
    return null;
  }
}

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  id: number;
  body: string;
  createdSeq: number;
  userLogin: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  createdSeq: number;
  updatedSeq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  createdSeq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  store: ObjectStore;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Issue[];
  pulls: Pull[];
  nextIssueNumber: number;
  nextPullNumber: number;
  createdSeq: number;
  updatedSeq: number;
}

export interface FileChange {
  path: string;
  status: 'added' | 'modified' | 'changed';
  additions: number;
  deletions: number;
  headOid: string | null;
  baseOid: string | null;
}

function lineCounts(base: Buffer | null, head: Buffer | null): { additions: number; deletions: number } {
  const baseLines = base === null ? [] : base.toString('utf8').split('\n');
  const headLines = head === null ? [] : head.toString('utf8').split('\n');
  // Drop the trailing empty element a final newline leaves behind.
  if (baseLines.length > 0 && baseLines[baseLines.length - 1] === '') baseLines.pop();
  if (headLines.length > 0 && headLines[headLines.length - 1] === '') headLines.pop();
  const baseCount = new Map<string, number>();
  for (const l of baseLines) baseCount.set(l, (baseCount.get(l) ?? 0) + 1);
  let additions = 0;
  for (const l of headLines) {
    const c = baseCount.get(l) ?? 0;
    if (c > 0) baseCount.set(l, c - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const c of baseCount.values()) deletions += c;
  return { additions, deletions };
}

/** Flatten a tree (by oid) into path -> {oid, mode}. */
export function flattenTree(store: ObjectStore, treeOid: string, prefix = '', out = new Map<string, { oid: string; mode: string }>()): Map<string, { oid: string; mode: string }> {
  const obj = store.get(treeOid);
  if (!obj || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') flattenTree(store, e.oid, path, out);
    else out.set(path, { oid: e.oid, mode: e.mode });
  }
  return out;
}

export function diffTrees(store: ObjectStore, baseTreeOid: string | null, headTreeOid: string | null): FileChange[] {
  const base = baseTreeOid ? flattenTree(store, baseTreeOid) : new Map();
  const head = headTreeOid ? flattenTree(store, headTreeOid) : new Map();
  const changes: FileChange[] = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const h = head.get(path);
    if (b && h && b.oid === h.oid) continue;
    const baseData = b ? store.get(b.oid)?.data ?? null : null;
    const headData = h ? store.get(h.oid)?.data ?? null : null;
    const { additions, deletions } = lineCounts(baseData, headData);
    const status = !b ? 'added' : !h ? 'changed' : 'modified';
    changes.push({ path, status, additions, deletions, headOid: h?.oid ?? null, baseOid: b?.oid ?? null });
  }
  return changes;
}

export class Store {
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 5000;
  private nextIssueId = 1;
  private nextCommentId = 1;
  private nextPullId = 1;
  private seq = 0;
  readonly epochSec: number;

  constructor() {
    this.epochSec = trialEpochSec();
  }

  /** Monotonic sequence; every created thing takes the next one. */
  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  isoAt(seq: number): string {
    return new Date((this.epochSec + seq) * 1000).toISOString();
  }

  /** "Now" as an ISO timestamp, deterministic within a trial. */
  nowIso(): string {
    return this.isoAt(this.seq);
  }

  identity(): Identity {
    return viewerIdentity(this.epochSec + this.seq);
  }

  // ---------------------------------------------------------- repositories

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== VIEWER.login) return undefined;
    return this.repos.get(this.repoKey(owner, name));
  }

  getRepoById(id: number): Repo | undefined {
    for (const repo of this.repos.values()) if (repo.id === id) return repo;
    return undefined;
  }

  createRepo(name: string, opts: { description?: string; private?: boolean; defaultBranch?: string } = {}): Repo | null {
    if (this.repos.has(this.repoKey(VIEWER.login, name))) return null;
    const seq = this.nextSeq();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner: VIEWER.login,
      description: opts.description ?? '',
      private: opts.private ?? false,
      defaultBranch: opts.defaultBranch ?? DEFAULT_BRANCH,
      store: new ObjectStore(),
      refs: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextIssueNumber: 1,
      nextPullNumber: 1,
      createdSeq: seq,
      updatedSeq: seq,
    };
    this.repos.set(this.repoKey(VIEWER.login, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    if (owner !== VIEWER.login) return false;
    return this.repos.delete(this.repoKey(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER.login) out.push(repo);
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  // ---------------------------------------------------------- git refs

  refName(repo: Repo, branch: string): string {
    return `refs/heads/${branch}`;
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  /** Resolve a branch name or full ref to its tip commit oid. */
  resolveBranch(repo: Repo, branchOrRef: string): string | undefined {
    if (branchOrRef.startsWith('refs/')) return repo.refs.get(branchOrRef);
    return repo.refs.get(this.refName(repo, branchOrRef));
  }

  createBranch(repo: Repo, branch: string, sha: string): boolean {
    const obj = repo.store.get(sha);
    if (!obj || obj.type !== 'commit') return false;
    repo.refs.set(this.refName(repo, branch), sha);
    return true;
  }

  /** Read a file (blob) at a ref; null when the ref/path does not exist. */
  readFileAt(repo: Repo, ref: string, path: string): { oid: string; data: Buffer } | null {
    const tip = this.resolveBranch(repo, ref);
    if (!tip) return null;
    const commit = repo.store.get(tip);
    if (!commit || commit.type !== 'commit') return null;
    const tree = parseCommit(commit.data).tree;
    const flat = flattenTree(repo.store, tree);
    const entry = flat.get(path);
    if (!entry) return null;
    const blob = repo.store.get(entry.oid);
    if (!blob) return null;
    return { oid: entry.oid, data: blob.data };
  }

  /** List a directory at a ref: null when the ref/path does not exist. */
  listDirAt(repo: Repo, ref: string, path: string): Array<{ name: string; path: string; oid: string; type: 'file' | 'dir' }> | null {
    const tip = this.resolveBranch(repo, ref);
    if (!tip) return null;
    const commit = repo.store.get(tip);
    if (!commit || commit.type !== 'commit') return null;
    let treeOid = parseCommit(commit.data).tree;
    if (path !== '') {
      // Find the subtree at `path`.
      const walk = (tree: string, parts: string[]): string | null => {
        if (parts.length === 0) return tree;
        const obj = repo.store.get(tree);
        if (!obj || obj.type !== 'tree') return null;
        for (const e of parseTree(obj.data)) {
          if (e.name === parts[0] && e.mode === '40000') return walk(e.oid, parts.slice(1));
        }
        return null;
      };
      const found = walk(treeOid, path.split('/'));
      if (found === null) return null;
      treeOid = found;
    }
    const obj = repo.store.get(treeOid);
    if (!obj || obj.type !== 'tree') return null;
    return parseTree(obj.data).map((e) => ({
      name: e.name,
      path: path === '' ? e.name : `${path}/${e.name}`,
      oid: e.oid,
      type: e.mode === '40000' ? ('dir' as const) : ('file' as const),
    }));
  }

  /** Set (or delete, when content is null) one path in a tree; returns the new tree oid. */
  private setPathInTree(store: ObjectStore, treeOid: string | null, parts: string[], content: Buffer | null): string {
    const entries: TreeEntry[] = treeOid ? parseTree(store.get(treeOid)?.data ?? Buffer.alloc(0)) : [];
    const [head, ...rest] = parts;
    const idx = entries.findIndex((e) => e.name === head);
    if (rest.length === 0) {
      if (content === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else {
        const blobOid = store.put('blob', content);
        if (idx >= 0) entries[idx] = { mode: '100644', name: head, oid: blobOid };
        else entries.push({ mode: '100644', name: head, oid: blobOid });
      }
    } else {
      const childTree = idx >= 0 && entries[idx].mode === '40000' ? entries[idx].oid : null;
      const newChild = this.setPathInTree(store, childTree, rest, content);
      // An emptied subtree disappears; a fresh one appears.
      const childObj = store.get(newChild);
      const childEmpty = childObj !== undefined && childObj.type === 'tree' && parseTree(childObj.data).length === 0;
      if (childEmpty) {
        if (idx >= 0) entries.splice(idx, 1);
      } else if (idx >= 0) entries[idx] = { mode: '40000', name: head, oid: newChild };
      else entries.push({ mode: '40000', name: head, oid: newChild });
    }
    return store.put('tree', buildTree(entries));
  }

  /**
   * Commit a file change on `branch` (creating the branch from the default tip
   * when it does not exist, creating the root commit when the repo is unborn).
   * Returns null when the branch base does not exist.
   */
  commitFile(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer | null,
    message: string,
  ): { commitSha: string; blobSha: string | null; created: boolean } | null {
    const ref = this.refName(repo, branch);
    const tip = repo.refs.get(ref);
    let baseTree: string | null = null;
    const parents: string[] = [];
    if (tip !== undefined) {
      const commit = repo.store.get(tip);
      if (!commit || commit.type !== 'commit') return null;
      baseTree = parseCommit(commit.data).tree;
      parents.push(tip);
    } else if (branch !== repo.defaultBranch) {
      // New branch starts from the default branch tip.
      const defaultTip = repo.refs.get(this.refName(repo, repo.defaultBranch));
      if (defaultTip === undefined) return null;
      const commit = repo.store.get(defaultTip);
      if (!commit || commit.type !== 'commit') return null;
      baseTree = parseCommit(commit.data).tree;
      parents.push(defaultTip);
    }
    const parts = path.split('/').filter((p) => p !== '');
    if (parts.length === 0) return null;
    const newTree = this.setPathInTree(repo.store, baseTree, parts, content);
    const identity = this.identity();
    const commitInfo: CommitInfo = { tree: newTree, parents, author: identity, committer: identity, message };
    const commitData = buildCommit(commitInfo);
    const commitSha = repo.store.put('commit', commitData);
    repo.refs.set(ref, commitSha);
    const blobSha = content === null ? null : hashObject('blob', content);
    return { commitSha, blobSha, created: tip === undefined && parents.length === 0 };
  }

  /** Commits reachable from a ref, newest first (first-parent walk, linear histories). */
  listCommits(repo: Repo, branchOrRef: string): Array<{ sha: string; message: string }> {
    const tip = this.resolveBranch(repo, branchOrRef);
    if (!tip) return [];
    const out: Array<{ sha: string; message: string }> = [];
    const seen = new Set<string>();
    const queue: string[] = [tip];
    while (queue.length > 0) {
      const oid = queue.shift()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.store.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const info = parseCommit(obj.data);
      out.push({ sha: oid, message: info.message });
      for (const p of info.parents) queue.push(p);
    }
    return out;
  }

  // ---------------------------------------------------------- issues / labels

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const seq = this.nextSeq();
    const issue: Issue = {
      id: this.nextIssueId++,
      number: repo.nextIssueNumber++,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
      createdSeq: seq,
      updatedSeq: seq,
    };
    repo.issues.push(issue);
    return issue;
  }

  getIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.find((i) => i.number === number);
  }

  createLabel(repo: Repo, name: string, color = 'ededed'): Label {
    const existing = repo.labels.get(name);
    if (existing) return existing;
    const label: Label = { name, color, description: '' };
    repo.labels.set(name, label);
    return label;
  }

  addComment(repo: Repo, issue: Issue, body: string): Comment {
    const comment: Comment = { id: this.nextCommentId++, body, createdSeq: this.nextSeq(), userLogin: VIEWER.login };
    issue.comments.push(comment);
    return comment;
  }

  // ---------------------------------------------------------- pull requests

  createPull(repo: Repo, title: string, body: string, headRef: string, baseRef: string): Pull | null {
    const headSha = this.resolveBranch(repo, headRef);
    const baseSha = this.resolveBranch(repo, baseRef);
    if (!headSha || !baseSha) return null;
    const seq = this.nextSeq();
    const pr: Pull = {
      id: this.nextPullId++,
      number: repo.nextPullNumber++,
      title,
      body,
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef,
      baseRef,
      headSha,
      baseSha,
      createdSeq: seq,
    };
    repo.pulls.push(pr);
    return pr;
  }

  getPull(repo: Repo, number: number): Pull | undefined {
    return repo.pulls.find((p) => p.number === number);
  }

  /** Files changed between the merge-base tree and the head tree. */
  pullFiles(repo: Repo, pr: Pull): FileChange[] {
    const base = mergeBase(repo.store, pr.headSha, pr.baseSha);
    const baseTree = base ? parseCommit(repo.store.get(base)!.data).tree : null;
    const headTree = parseCommit(repo.store.get(pr.headSha)!.data).tree;
    return diffTrees(repo.store, baseTree, headTree);
  }

  /** Would a squash merge of this PR conflict? (Three-way, merge-base vs head vs base tip.) */
  mergeConflicts(repo: Repo, pr: Pull): boolean {
    const baseTipSha = this.resolveBranch(repo, pr.baseRef);
    if (!baseTipSha) return true;
    const base = mergeBase(repo.store, pr.headSha, baseTipSha);
    const baseTreeOid = base ? parseCommit(repo.store.get(base)!.data).tree : null;
    const headTreeOid = parseCommit(repo.store.get(pr.headSha)!.data).tree;
    const tipTreeOid = parseCommit(repo.store.get(baseTipSha)!.data).tree;
    const baseFlat = baseTreeOid ? flattenTree(repo.store, baseTreeOid) : new Map();
    const headFlat = flattenTree(repo.store, headTreeOid);
    const tipFlat = flattenTree(repo.store, tipTreeOid);
    for (const [path, h] of headFlat) {
      const b = baseFlat.get(path);
      const t = tipFlat.get(path);
      if (b?.oid === h.oid) continue; // unchanged from base
      if (t && t.oid !== b?.oid && t.oid !== h.oid) return true; // both sides changed it
    }
    for (const [path, b] of baseFlat) {
      if (headFlat.has(path)) continue; // deleted on head
      const t = tipFlat.get(path);
      if (t && t.oid !== b.oid) return true; // deleted on head, changed on base
    }
    return false;
  }

  /**
   * Squash-merge a PR onto its base branch. Returns the merge commit sha, or
   * null when the PR cannot merge (conflict or missing refs).
   */
  mergePull(repo: Repo, pr: Pull): { sha: string; message: string } | null {
    if (pr.state !== 'open') return null;
    if (this.mergeConflicts(repo, pr)) return null;
    const baseTipSha = this.resolveBranch(repo, pr.baseRef);
    if (!baseTipSha) return null;
    const base = mergeBase(repo.store, pr.headSha, baseTipSha);
    const baseTreeOid = base ? parseCommit(repo.store.get(base)!.data).tree : null;
    const headTreeOid = parseCommit(repo.store.get(pr.headSha)!.data).tree;
    const tipTreeOid = parseCommit(repo.store.get(baseTipSha)!.data).tree;
    const baseFlat = baseTreeOid ? flattenTree(repo.store, baseTreeOid) : new Map();
    const headFlat = flattenTree(repo.store, headTreeOid);
    // Start from the base tip tree; apply every head-side change.
    let newTreeOid = tipTreeOid;
    const changed = new Set<string>();
    for (const [path, h] of headFlat) {
      const b = baseFlat.get(path);
      if (b?.oid === h.oid) continue;
      changed.add(path);
    }
    for (const [path] of baseFlat) {
      if (!headFlat.has(path)) changed.add(path); // deleted on head
    }
    // Apply changes path by path, rebuilding trees bottom-up.
    for (const path of [...changed].sort()) {
      const h = headFlat.get(path);
      const content = h ? repo.store.get(h.oid)?.data ?? null : null;
      newTreeOid = this.setPathInTree(repo.store, newTreeOid, path.split('/'), content);
    }
    const seq = this.nextSeq();
    const identity = this.identity();
    const message = `Merge pull request #${pr.number} from ${pr.headRef}\n\n${pr.title}`;
    const commitData = buildCommit({
      tree: newTreeOid,
      parents: [baseTipSha],
      author: identity,
      committer: identity,
      message,
    });
    const sha = repo.store.put('commit', commitData);
    repo.refs.set(this.refName(repo, pr.baseRef), sha);
    pr.state = 'closed';
    pr.merged = true;
    pr.mergedAt = this.isoAt(seq);
    pr.mergeCommitSha = sha;
    return { sha, message };
  }

  // ---------------------------------------------------------- JSON shapes

  repoJson(repo: Repo): Record<string, unknown> {
    return {
      id: repo.id,
      name: repo.name,
      full_name: `${repo.owner}/${repo.name}`,
      owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
      private: repo.private,
      description: repo.description,
      default_branch: repo.defaultBranch,
      created_at: this.isoAt(repo.createdSeq),
      updated_at: this.isoAt(repo.updatedSeq),
      has_issues: true,
      has_pull_requests: true,
      clone_url: `https://example.invalid/${repo.owner}/${repo.name}.git`,
      html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
      open_issues_count: repo.issues.filter((i) => i.state === 'open').length,
    };
  }

  issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
    return {
      id: issue.id,
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: issue.labels.map((name) => {
        const label = repo.labels.get(name);
        return { name, color: label?.color ?? 'ededed', description: label?.description ?? '' };
      }),
      user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
      assignee: null,
      comments: issue.comments.length,
      created_at: this.isoAt(issue.createdSeq),
      updated_at: this.isoAt(issue.updatedSeq),
      repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
      html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    };
  }

  pullJson(repo: Repo, pr: Pull): Record<string, unknown> {
    return {
      id: pr.id,
      number: pr.number,
      title: pr.title,
      body: pr.body,
      state: pr.state,
      merged: pr.merged,
      merged_at: pr.mergedAt,
      merge_commit_sha: pr.mergeCommitSha,
      mergeable: !this.mergeConflicts(repo, pr),
      head: { ref: pr.headRef, sha: pr.headSha, label: `${repo.owner}:${pr.headRef}` },
      base: { ref: pr.baseRef, sha: pr.baseSha, label: `${repo.owner}:${pr.baseRef}` },
      user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
      created_at: this.isoAt(pr.createdSeq),
      updated_at: this.isoAt(pr.createdSeq),
      html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pr.number}`,
    };
  }

  labelJson(label: Label): Record<string, unknown> {
    return { name: label.name, color: label.color, description: label.description };
  }

  commentJson(comment: Comment): Record<string, unknown> {
    return {
      id: comment.id,
      body: comment.body,
      user: { login: comment.userLogin, id: VIEWER.id, type: 'User' },
      created_at: this.isoAt(comment.createdSeq),
      updated_at: this.isoAt(comment.createdSeq),
    };
  }
}

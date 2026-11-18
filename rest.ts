// rest.ts — REST v3 under /api/v3.
// Every reply is application/json with a non-empty X-GitHub-Request-Id.
// Auth is a bearer token (MAKEFUN_SHOP_TOKEN); missing/wrong -> 401.
// A repo belongs to the token's login; another owner -> 404, never 403.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER, type Repo, type Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

let requestCounter = 0;

function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
  extraHeaders: Readonly<Record<string, string>> = {},
): void {
  requestCounter += 1;
  const headers: Record<string, string> = {
    'content-type': JSON_TYPE,
    'x-github-request-id': `req-${requestCounter.toString(16)}`,
    ...extraHeaders,
  };
  if (status === 204 || body === null) {
    res.writeHead(status, { ...headers, 'content-length': 0 });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  headers['content-length'] = String(Buffer.byteLength(text));
  res.writeHead(status, headers);
  res.end(text);
}

function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function expectedToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function isAuthorized(req: IncomingMessage): boolean {
  const expected = expectedToken();
  if (expected === '') return false;
  return req.headers.authorization === `Bearer ${expected}`;
}

function parseBody(raw: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (raw.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(raw.toString('utf8')) as unknown };
  } catch {
    return { ok: false };
  }
}

function asRecord(value: unknown): Record<string, unknown> {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  return {};
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function intParam(search: URLSearchParams, name: string, fallback: number): number {
  const raw = search.get(name);
  if (raw === null) return fallback;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.trunc(n) : fallback;
}

/** Paginate a list; returns the page plus a `Link: rel="next"` value when needed. */
function paginate<T>(
  items: readonly T[],
  search: URLSearchParams,
  req: IncomingMessage,
  pathname: string,
): { page: T[]; link: string | null } {
  const perPage = Math.min(100, intParam(search, 'per_page', 30));
  const pageNum = intParam(search, 'page', 1);
  const start = (pageNum - 1) * perPage;
  const page = items.slice(start, start + perPage);
  let link: string | null = null;
  if (start + perPage < items.length) {
    const host = req.headers.host ?? 'localhost';
    const params = new URLSearchParams(search);
    params.set('per_page', String(perPage));
    params.set('page', String(pageNum + 1));
    link = `<http://${host}${pathname}?${params.toString()}>; rel="next"`;
  }
  return { page, link };
}

function linkHeaders(link: string | null): Record<string, string> {
  return link === null ? {} : { link };
}

/**
 * Handle a REST request. Returns true when the path is under /api/v3
 * (handled, even on 404); false otherwise.
 */
export function handleRest(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  search: URLSearchParams,
  rawBody: Buffer,
): boolean {
  const clean = pathname === '/api/v3' ? '/api/v3/' : pathname.replace(/\/+$/, '') || '/';
  if (!clean.startsWith('/api/v3')) return false;
  const method = req.method ?? 'GET';
  const path = clean.slice('/api/v3'.length) || '/';

  // Auth first: every API call carries the bearer token.
  if (!isAuthorized(req)) {
    sendError(res, 401, 'Bad credentials');
    return true;
  }

  // Body parsing for methods that carry one.
  let bodyValue: unknown = {};
  if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
    const parsed = parseBody(rawBody);
    if (!parsed.ok) {
      sendError(res, 400, 'Problems parsing JSON');
      return true;
    }
    bodyValue = parsed.value;
  }
  const body = asRecord(bodyValue);

  const segs = path.split('/').filter((s) => s !== '');
  const seg = (i: number): string | undefined => (segs[i] === undefined ? undefined : decodeURIComponent(segs[i]));

  try {
    // ---- API root + meta + viewer -------------------------------------
    if (method === 'GET' && path === '/') {
      sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
      return true;
    }
    if (method === 'GET' && path === '/meta') {
      sendJson(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: false });
      return true;
    }
    if (method === 'GET' && path === '/user') {
      sendJson(res, 200, { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email, type: 'User' });
      return true;
    }

    // ---- /user/repos ----------------------------------------------------
    if (segs[0] === 'user' && segs[1] === 'repos' && segs.length === 2) {
      if (method === 'GET') {
        const repos = store.listViewerRepos().map((r) => store.repoJson(r));
        const { page, link } = paginate(repos, search, req, pathname);
        sendJson(res, 200, page, linkHeaders(link));
        return true;
      }
      if (method === 'POST') {
        const name = asString(body.name);
        if (!name) {
          sendError(res, 422, 'name is required');
          return true;
        }
        const repo = store.createRepo(name, {
          description: asString(body.description),
          private: body.private === true,
        });
        if (repo === null) {
          sendError(res, 422, 'Repository already exists');
          return true;
        }
        sendJson(res, 201, store.repoJson(repo));
        return true;
      }
    }

    // ---- /repos/{owner}/{repo}/... --------------------------------------
    if (segs[0] === 'repos' && segs.length >= 3) {
      const owner = seg(1)!;
      const repoName = seg(2)!;
      const repo = store.getRepo(owner, repoName);
      if (!repo) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      const rest = segs.slice(3);

      // GET/PATCH/DELETE the repo itself.
      if (rest.length === 0) {
        if (method === 'GET') {
          sendJson(res, 200, store.repoJson(repo));
          return true;
        }
        if (method === 'PATCH') {
          const description = asString(body.description);
          if (description !== undefined) repo.description = description;
          if (body.private === true || body.private === false) repo.private = body.private;
          repo.updatedSeq = store.nextSeq();
          sendJson(res, 200, store.repoJson(repo));
          return true;
        }
        if (method === 'DELETE') {
          store.deleteRepo(owner, repoName);
          sendJson(res, 204, null);
          return true;
        }
      }

      // Branches.
      if (rest[0] === 'branches') {
        if (method === 'GET' && rest.length === 1) {
          const names = [...repo.refs.keys()]
            .filter((r) => r.startsWith('refs/heads/'))
            .map((r) => r.slice('refs/heads/'.length));
          names.sort((a, b) => (a === repo.defaultBranch ? -1 : b === repo.defaultBranch ? 1 : a < b ? -1 : 1));
          sendJson(res, 200, names.map((name) => ({ name, commit: { sha: repo.refs.get(`refs/heads/${name}`) } })));
          return true;
        }
        if (method === 'GET' && rest.length === 2) {
          const branch = decodeURIComponent(rest[1]);
          const sha = repo.refs.get(`refs/heads/${branch}`);
          if (!sha) {
            sendError(res, 404, 'Branch not found');
            return true;
          }
          sendJson(res, 200, { name: branch, commit: { sha } });
          return true;
        }
      }

      // Git refs.
      if (rest[0] === 'git' && rest[1] === 'refs') {
        if (method === 'GET' && rest.length >= 4 && rest[2] === 'heads') {
          const branch = rest.slice(3).map((s) => decodeURIComponent(s)).join('/');
          const sha = repo.refs.get(`refs/heads/${branch}`);
          if (!sha) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          sendJson(res, 200, { ref: `refs/heads/${branch}`, object: { sha, type: 'commit' } });
          return true;
        }
        if (method === 'POST' && rest.length === 2) {
          const ref = asString(body.ref);
          const sha = asString(body.sha);
          if (!ref || !sha) {
            sendError(res, 422, 'ref and sha are required');
            return true;
          }
          const obj = repo.store.get(sha);
          if (!obj || obj.type !== 'commit') {
            sendError(res, 422, 'sha is not a known commit');
            return true;
          }
          repo.refs.set(ref, sha);
          sendJson(res, 201, { ref, object: { sha, type: 'commit' } });
          return true;
        }
      }

      // Commits.
      if (rest[0] === 'commits' && method === 'GET' && rest.length === 1) {
        const branch = search.get('sha') ?? repo.defaultBranch;
        const commits = store.listCommits(repo, branch).map((c) => ({
          sha: c.sha,
          commit: {
            message: c.message,
            author: { name: VIEWER.name, email: VIEWER.email },
            committer: { name: VIEWER.name, email: VIEWER.email },
          },
        }));
        sendJson(res, 200, commits);
        return true;
      }

      // Contents.
      if (rest[0] === 'contents') {
        const contentPath = rest.slice(1).map((s) => decodeURIComponent(s)).join('/');
        const ref = search.get('ref') ?? repo.defaultBranch;
        if (method === 'GET') {
          const file = contentPath === '' ? null : store.readFileAt(repo, ref, contentPath);
          if (file) {
            sendJson(res, 200, {
              type: 'file',
              encoding: 'base64',
              content: file.data.toString('base64'),
              sha: file.oid,
              size: file.data.length,
              name: contentPath.split('/').pop(),
              path: contentPath,
            });
            return true;
          }
          const dir = store.listDirAt(repo, ref, contentPath);
          if (dir) {
            sendJson(res, 200, dir.map((e) => ({ type: e.type, name: e.name, path: e.path, sha: e.oid })));
            return true;
          }
          sendError(res, 404, 'Not Found');
          return true;
        }
        if (method === 'PUT') {
          const message = asString(body.message) ?? '';
          const contentB64 = asString(body.content);
          if (contentB64 === undefined) {
            sendError(res, 422, 'content is required');
            return true;
          }
          let content: Buffer;
          try {
            content = Buffer.from(contentB64, 'base64');
          } catch {
            sendError(res, 400, 'content is not valid base64');
            return true;
          }
          const branch = asString(body.branch) ?? repo.defaultBranch;
          const existing = store.readFileAt(repo, branch, contentPath);
          const sha = asString(body.sha);
          if (existing) {
            if (sha === undefined || sha !== existing.oid) {
              sendError(res, 409, 'sha does not match the current blob');
              return true;
            }
          }
          const result = store.commitFile(repo, branch, contentPath, content, message);
          if (!result) {
            sendError(res, 404, 'Branch not found');
            return true;
          }
          sendJson(res, existing ? 200 : 201, {
            content: { name: contentPath.split('/').pop(), path: contentPath, sha: result.blobSha, encoding: 'base64' },
            commit: { sha: result.commitSha },
          });
          return true;
        }
        if (method === 'DELETE') {
          const existing = store.readFileAt(repo, repo.defaultBranch, contentPath);
          if (!existing) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          const sha = asString(body.sha);
          if (sha !== undefined && sha !== existing.oid) {
            sendError(res, 409, 'sha does not match the current blob');
            return true;
          }
          const message = asString(body.message) ?? '';
          const result = store.commitFile(repo, repo.defaultBranch, contentPath, null, message);
          if (!result) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          sendJson(res, 200, { commit: { sha: result.commitSha } });
          return true;
        }
      }

      // Labels.
      if (rest[0] === 'labels') {
        if (method === 'GET' && rest.length === 1) {
          sendJson(res, 200, [...repo.labels.values()].map((l) => store.labelJson(l)));
          return true;
        }
        if (method === 'POST' && rest.length === 1) {
          const name = asString(body.name);
          if (!name) {
            sendError(res, 422, 'name is required');
            return true;
          }
          const label = store.createLabel(repo, name, asString(body.color) ?? 'ededed');
          sendJson(res, 201, store.labelJson(label));
          return true;
        }
      }

      // Issues.
      if (rest[0] === 'issues') {
        if (rest.length === 1) {
          if (method === 'GET') {
            const state = search.get('state') ?? 'open';
            const direction = search.get('direction') ?? 'desc';
            let issues = repo.issues.filter((i) => state === 'all' || i.state === state);
            issues = [...issues].sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
            const { page, link } = paginate(issues.map((i) => store.issueJson(repo, i)), search, req, pathname);
            sendJson(res, 200, page, linkHeaders(link));
            return true;
          }
          if (method === 'POST') {
            const title = asString(body.title);
            if (!title) {
              sendError(res, 422, 'title is required');
              return true;
            }
            const rawLabels = Array.isArray(body.labels) ? body.labels : [];
            const labels: string[] = [];
            for (const raw of rawLabels) {
              const name = typeof raw === 'string' ? raw : asString((raw as Record<string, unknown>)?.name);
              if (!name) continue;
              if (!repo.labels.has(name)) {
                sendError(res, 422, `label ${name} does not exist`);
                return true;
              }
              labels.push(name);
            }
            const issue = store.createIssue(repo, title, asString(body.body) ?? '', labels);
            sendJson(res, 201, store.issueJson(repo, issue));
            return true;
          }
        }
        const number = Number(rest[1]);
        if (Number.isInteger(number) && number > 0) {
          const issue = store.getIssue(repo, number);
          if (!issue) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          if (rest.length === 2) {
            if (method === 'GET') {
              sendJson(res, 200, store.issueJson(repo, issue));
              return true;
            }
            if (method === 'PATCH') {
              const state = asString(body.state);
              if (state === 'open' || state === 'closed') issue.state = state;
              const title = asString(body.title);
              if (title !== undefined) issue.title = title;
              const b = asString(body.body);
              if (b !== undefined) issue.body = b;
              issue.updatedSeq = store.nextSeq();
              sendJson(res, 200, store.issueJson(repo, issue));
              return true;
            }
          }
          if (rest[2] === 'comments') {
            if (method === 'GET') {
              sendJson(res, 200, issue.comments.map((c) => store.commentJson(c)));
              return true;
            }
            if (method === 'POST') {
              const commentBody = asString(body.body);
              if (commentBody === undefined) {
                sendError(res, 422, 'body is required');
                return true;
              }
              const comment = store.addComment(repo, issue, commentBody);
              sendJson(res, 201, store.commentJson(comment));
              return true;
            }
          }
        }
      }

      // Pull requests.
      if (rest[0] === 'pulls') {
        if (rest.length === 1) {
          if (method === 'GET') {
            const state = search.get('state') ?? 'open';
            const pulls = repo.pulls
              .filter((p) => state === 'all' || p.state === state)
              .map((p) => store.pullJson(repo, p));
            const { page, link } = paginate(pulls, search, req, pathname);
            sendJson(res, 200, page, linkHeaders(link));
            return true;
          }
          if (method === 'POST') {
            const title = asString(body.title);
            const head = asString(body.head);
            const base = asString(body.base) ?? repo.defaultBranch;
            if (!title || !head) {
              sendError(res, 422, 'title and head are required');
              return true;
            }
            if (!store.resolveBranch(repo, head) || !store.resolveBranch(repo, base)) {
              sendError(res, 422, 'head or base branch does not exist');
              return true;
            }
            const duplicate = repo.pulls.find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
            if (duplicate) {
              sendError(res, 422, 'A pull request already exists for this head and base');
              return true;
            }
            const pr = store.createPull(repo, title, asString(body.body) ?? '', head, base);
            if (!pr) {
              sendError(res, 422, 'head or base branch does not exist');
              return true;
            }
            sendJson(res, 201, store.pullJson(repo, pr));
            return true;
          }
        }
        const number = Number(rest[1]);
        if (Number.isInteger(number) && number > 0) {
          const pr = store.getPull(repo, number);
          if (!pr) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          if (rest.length === 2 && method === 'GET') {
            sendJson(res, 200, store.pullJson(repo, pr));
            return true;
          }
          if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
            const files = store.pullFiles(repo, pr).map((f) => ({
              filename: f.path,
              status: f.status,
              additions: f.additions,
              deletions: f.deletions,
              changes: f.additions + f.deletions,
            }));
            sendJson(res, 200, files);
            return true;
          }
          if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
            if (pr.state !== 'open') {
              sendError(res, 405, 'Pull request is already closed');
              return true;
            }
            const merged = store.mergePull(repo, pr);
            if (!merged) {
              sendError(res, 405, 'This pull request cannot be merged: conflicts');
              return true;
            }
            sendJson(res, 200, { sha: merged.sha, merged: true, message: merged.message });
            return true;
          }
        }
      }
    }

    // Unknown route under /api/v3.
    sendError(res, 404, 'Not Found');
    return true;
  } catch {
    if (!res.writableEnded) sendError(res, 500, 'Internal Server Error');
    return true;
  }
}

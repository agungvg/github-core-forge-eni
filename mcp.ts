// mcp.ts — MCP at POST /mcp: JSON-RPC 2.0 over Streamable HTTP (plain JSON
// answers; no SSE needed). Revision 2026-07-28 via server/discover, legacy
// handshake via initialize + notifications/initialized. Tools act on the same
// store as REST, with the same ids.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER, type Store } from './store.ts';

const SERVER_NAME = 'github-core-forge';
const SERVER_VERSION = '1.0.0';
const REVISIONS = ['2026-07-28', '2025-11-25'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description?: string }>;
    required: string[];
  };
}

const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login (defaults to the authenticated user)' },
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request into its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'number', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, result };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

function toolResult(obj: unknown): Record<string, unknown> {
  const text = JSON.stringify(obj);
  return { content: [{ type: 'text', text }], structuredContent: obj };
}

function toolError(message: string): Record<string, unknown> {
  const text = JSON.stringify({ error: message });
  return { content: [{ type: 'text', text }], structuredContent: { error: message }, isError: true };
}

function isAuthorized(req: IncomingMessage): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (expected === '') return false;
  return req.headers.authorization === `Bearer ${expected}`;
}

function asRecord(value: unknown): Record<string, unknown> {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) return value as Record<string, unknown>;
  return {};
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

/** Extract a JSON-RPC id from raw text even when the body is truncated. */
function sniffId(raw: string): unknown {
  const m = /"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+)/.exec(raw);
  if (!m) return null;
  const token = m[1];
  if (token.startsWith('"')) {
    try {
      return JSON.parse(token) as unknown;
    } catch {
      return null;
    }
  }
  return Number(token);
}

function findTool(name: string): ToolDef | undefined {
  const lower = name.toLowerCase();
  return TOOLS.find((t) => lower === t.name || lower.endsWith(`__${t.name}`) || lower.endsWith(`_${t.name}`));
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = findTool(name);
  if (!tool) return { __rpcError: { code: -32602, message: `unknown tool ${name}` } };
  const owner = asString(args.owner) ?? VIEWER.login;
  const repoName = asString(args.repo) ?? asString(args.repository) ?? asString(args.repo_name);

  switch (tool.name) {
    case 'create_repo': {
      const repoNameArg = asString(args.name);
      if (!repoNameArg) return toolError('name is required');
      const repo = store.createRepo(repoNameArg, {
        description: asString(args.description),
        private: args.private === true,
      });
      if (!repo) return toolError(`repository ${repoNameArg} already exists`);
      const obj = { ...store.repoJson(repo), full_name: `${repo.owner}/${repo.name}` };
      return toolResult(obj);
    }
    case 'get_repo': {
      if (!repoName) return toolError('repo is required');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`repository ${owner}/${repoName} not found`);
      return toolResult(store.repoJson(repo));
    }
    case 'create_issue': {
      if (!repoName) return toolError('repo is required');
      const title = asString(args.title);
      if (!title) return toolError('title is required');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`repository ${owner}/${repoName} not found`);
      const issue = store.createIssue(repo, title, asString(args.body) ?? '', []);
      return toolResult(store.issueJson(repo, issue));
    }
    case 'list_issues': {
      if (!repoName) return toolError('repo is required');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`repository ${owner}/${repoName} not found`);
      const state = asString(args.state) ?? 'open';
      const issues = repo.issues
        .filter((i) => state === 'all' || i.state === state)
        .map((i) => store.issueJson(repo, i));
      return toolResult({ issues });
    }
    case 'create_pull_request': {
      if (!repoName) return toolError('repo is required');
      const title = asString(args.title);
      const head = asString(args.head);
      const base = asString(args.base) ?? 'main';
      if (!title) return toolError('title is required');
      if (!head) return toolError('head is required');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`repository ${owner}/${repoName} not found`);
      if (!store.resolveBranch(repo, head) || !store.resolveBranch(repo, base)) {
        return toolError('head or base branch does not exist');
      }
      let pr = repo.pulls.find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
      if (!pr) pr = store.createPull(repo, title, asString(args.body) ?? '', head, base) ?? undefined;
      if (!pr) return toolError('head or base branch does not exist');
      const obj = { ...store.pullJson(repo, pr), number: pr.number, pull_number: pr.number };
      return toolResult(obj);
    }
    case 'merge_pull_request': {
      if (!repoName) return toolError('repo is required');
      const num = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number ?? args.number);
      if (!Number.isInteger(num) || num <= 0) return toolError('pull_number is required');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`repository ${owner}/${repoName} not found`);
      const pr = store.getPull(repo, num);
      if (!pr) return toolError(`pull request ${num} not found`);
      if (pr.state !== 'open') return toolError('pull request is already closed');
      const merged = store.mergePull(repo, pr);
      if (!merged) return toolError('pull request cannot be merged: conflicts');
      return toolResult({ sha: merged.sha, merged: true, message: merged.message, number: pr.number, pull_number: pr.number });
    }
    default:
      return toolError(`tool ${tool.name} is not implemented`);
  }
}

/** Handle POST /mcp. Returns true when handled. */
export function handleMcp(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  rawBody: Buffer,
): boolean {
  if (pathname !== '/mcp') return false;
  if ((req.method ?? 'GET') !== 'POST') {
    sendJson(res, 405, rpcError(null, -32600, 'method not allowed'));
    return true;
  }
  const raw = rawBody.toString('utf8');
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw) as unknown;
  } catch {
    // Non-JSON body: HTTP 400 with JSON-RPC -32700 (modern era).
    sendJson(res, 400, rpcError(sniffId(raw), -32700, 'Parse error'));
    return true;
  }
  if (!isAuthorized(req)) {
    sendJson(res, 401, rpcError(null, -32000, 'Bad credentials'));
    return true;
  }
  const msg = asRecord(parsed);
  const id: unknown = 'id' in msg ? msg.id : undefined;
  const method = asString(msg.method);

  // Notifications carry no id: answer 202 with an empty body.
  if (id === undefined) {
    res.writeHead(202, { 'content-length': 0 });
    res.end();
    return true;
  }

  if (method === 'server/discover') {
    const serverInfo = { name: SERVER_NAME, version: SERVER_VERSION };
    sendJson(res, 200, rpcResult(id, {
      supportedVersions: [...REVISIONS],
      serverInfo,
      capabilities: { tools: {} },
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
    }));
    return true;
  }
  if (method === 'initialize') {
    const params = asRecord(msg.params);
    const requested = asString(params.protocolVersion);
    const protocolVersion = requested && REVISIONS.includes(requested) ? requested : REVISIONS[0];
    sendJson(res, 200, rpcResult(id, {
      protocolVersion,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    }));
    return true;
  }
  if (method === 'ping') {
    sendJson(res, 200, rpcResult(id, {}));
    return true;
  }
  if (method === 'tools/list') {
    // Static list: two calls answer byte for byte the same.
    sendJson(res, 200, rpcResult(id, { tools: TOOLS }));
    return true;
  }
  if (method === 'tools/call') {
    const params = asRecord(msg.params);
    const name = asString(params.name);
    if (!name) {
      sendJson(res, 200, rpcError(id, -32602, 'tool name is required'));
      return true;
    }
    const tool = findTool(name);
    if (!tool) {
      sendJson(res, 200, rpcError(id, -32602, `unknown tool ${name}`));
      return true;
    }
    const args = asRecord(params.arguments);
    const result = callTool(store, name, args);
    if ('__rpcError' in result) {
      const err = (result as { __rpcError: { code: number; message: string } }).__rpcError;
      sendJson(res, 200, rpcError(id, err.code, err.message));
      return true;
    }
    sendJson(res, 200, rpcResult(id, result));
    return true;
  }
  sendJson(res, 200, rpcError(id, -32601, `method not found: ${method ?? ''}`));
  return true;
}

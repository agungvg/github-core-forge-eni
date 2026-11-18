// graphql.ts — GraphQL v4 at POST /api/graphql.
// A small executor for the documents gh and github-mcp-server send: viewer,
// repository, issues, pull requests, node, __type introspection, the five
// mutations, and batched operations. Unknown fields answer HTTP 200 with an
// errors array naming the field; unknown repositories answer data.repository
// null with no errors.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  VIEWER,
  decodeGlobalId,
  globalId,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

// ------------------------------------------------------------------ parsing

type GqlValue =
  | string
  | number
  | boolean
  | null
  | GqlValue[]
  | { [key: string]: GqlValue }
  | { __variable: string };

interface GqlField {
  alias: string | null;
  name: string;
  args: Record<string, GqlValue>;
  selections: GqlField[] | null;
  inlineType: string | null;
}

interface GqlOperation {
  kind: 'query' | 'mutation';
  selections: GqlField[];
}

function tokenize(input: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < input.length) {
    const c = input[i];
    if (/\s/.test(c)) {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && input.startsWith('...', i)) {
      tokens.push('...');
      i += 3;
      continue;
    }
    if ('{}():,=!@$[]'.includes(c)) {
      tokens.push(c);
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          out += input[j + 1];
          j += 2;
        } else {
          out += input[j];
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < input.length && /[A-Za-z0-9_]/.test(input[j])) j += 1;
      tokens.push(input.slice(i, j));
      i = j;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      while (j < input.length && /[0-9.eE+-]/.test(input[j])) j += 1;
      tokens.push(input.slice(i, j));
      i = j;
      continue;
    }
    i += 1; // skip anything else
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: string[];
  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  peek(): string | undefined {
    return this.tokens[this.pos];
  }

  next(): string | undefined {
    return this.tokens[this.pos++];
  }

  expect(tok: string): void {
    const got = this.next();
    if (got !== tok) throw new Error(`expected ${tok}, got ${got ?? 'end'}`);
  }

  parseOperation(): GqlOperation {
    let kind: 'query' | 'mutation' = 'query';
    if (this.peek() === 'query' || this.peek() === 'mutation') {
      kind = this.next() as 'query' | 'mutation';
    }
    // Optional operation name.
    if (this.peek() !== undefined && this.peek() !== '{' && this.peek() !== '(') this.next();
    // Optional variable definitions: skip balanced parens.
    if (this.peek() === '(') {
      let depth = 0;
      while (this.pos < this.tokens.length) {
        const t = this.next()!;
        if (t === '(') depth += 1;
        else if (t === ')') {
          depth -= 1;
          if (depth === 0) break;
        }
      }
    }
    const selections = this.parseSelectionSet();
    return { kind, selections };
  }

  parseSelectionSet(): GqlField[] {
    this.expect('{');
    const fields: GqlField[] = [];
    while (this.peek() !== undefined && this.peek() !== '}') {
      fields.push(...this.parseField());
    }
    this.expect('}');
    return fields;
  }

  parseField(): GqlField[] {
    if (this.peek() === '...') {
      this.next();
      let inlineType: string | null = null;
      if (this.peek() === 'on') {
        this.next();
        inlineType = this.next() ?? null;
      } else {
        // Named fragment spread (not used by the suite's documents).
        this.next();
        return [];
      }
      const selections = this.parseSelectionSet();
      return selections.map((f) => ({ ...f, inlineType }));
    }
    let name = this.next()!;
    let alias: string | null = null;
    if (this.peek() === ':') {
      this.next();
      alias = name;
      name = this.next()!;
    }
    const args: Record<string, GqlValue> = {};
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== undefined && this.peek() !== ')') {
        const argName = this.next()!;
        this.expect(':');
        args[argName] = this.parseValue();
        if (this.peek() === ',') this.next();
      }
      this.expect(')');
    }
    let selections: GqlField[] | null = null;
    if (this.peek() === '{') selections = this.parseSelectionSet();
    return [{ alias, name, args, selections, inlineType: null }];
  }

  parseValue(): GqlValue {
    const tok = this.next()!;
    if (tok === '$') {
      return { __variable: this.next()! };
    }
    if (tok === '[') {
      const items: GqlValue[] = [];
      while (this.peek() !== undefined && this.peek() !== ']') {
        items.push(this.parseValue());
        if (this.peek() === ',') this.next();
      }
      this.expect(']');
      return items;
    }
    if (tok === '{') {
      const obj: Record<string, GqlValue> = {};
      while (this.peek() !== undefined && this.peek() !== '}') {
        const key = this.next()!;
        this.expect(':');
        obj[key] = this.parseValue();
        if (this.peek() === ',') this.next();
      }
      this.expect('}');
      return obj;
    }
    if (tok.startsWith('"')) return JSON.parse(tok) as string;
    if (tok === 'true') return true;
    if (tok === 'false') return false;
    if (tok === 'null') return null;
    if (/^-?\d+(\.\d+)?$/.test(tok)) return Number(tok);
    return tok; // enum value, kept as a bare string
  }
}

function resolveVariables(value: GqlValue, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object') {
    if (Array.isArray(value)) return value.map((v) => resolveVariables(v, variables));
    if ('__variable' in value) return variables[(value as { __variable: string }).__variable] ?? null;
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = resolveVariables(v, variables);
    return out;
  }
  return value;
}

// ------------------------------------------------------------------ schema

const TYPE_FIELDS: Record<string, readonly string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  User: ['login', 'id', 'name', 'email'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests'],
  Issue: ['id', 'number', 'title', 'state', 'body'],
  PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
  IssueConnection: ['nodes', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'pageInfo', 'totalCount'],
  FileConnection: ['nodes'],
  PageInfo: ['hasNextPage', 'endCursor'],
  Ref: ['name'],
  CommentEdge: ['node'],
  Comment: ['id', 'body'],
  File: ['path'],
  __Type: ['name', 'kind', 'fields', 'enumValues'],
  __Field: ['name'],
  __EnumValue: ['name'],
};

const INTROSPECTION: Record<string, { fields?: string[]; enumValues?: string[] }> = {
  Issue: { fields: ['id', 'number', 'title', 'state', 'body'] },
  PullRequest: { fields: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'] },
  Repository: { fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'] },
  SearchType: { enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] },
};

// ------------------------------------------------------------------ execution

interface Ctx {
  store: Store;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function unknownField(ctx: Ctx, typeName: string, fieldName: string): null {
  ctx.errors.push({ message: `Cannot query field "${fieldName}" on type "${typeName}".` });
  return null;
}

function selectFields(
  ctx: Ctx,
  typeName: string,
  value: Record<string, unknown> | null,
  selections: GqlField[] | null,
): Record<string, unknown> | null {
  if (value === null) return null;
  if (selections === null) return value;
  const out: Record<string, unknown> = {};
  const allowed = TYPE_FIELDS[typeName] ?? [];
  for (const field of selections) {
    const key = field.alias ?? field.name;
    if (!allowed.includes(field.name)) {
      out[key] = unknownField(ctx, typeName, field.name);
      continue;
    }
    out[key] = resolveField(ctx, typeName, value, field);
  }
  return out;
}

function issueValue(ctx: Ctx, repo: Repo, issueNumber: number, selections: GqlField[] | null): Record<string, unknown> | null {
  const issue = ctx.store.getIssue(repo, issueNumber);
  if (!issue) return null;
  return selectFields(ctx, 'Issue', {
    id: globalId('I', repo.id, issue.number),
    number: issue.number,
    title: issue.title,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    body: issue.body,
  }, selections);
}

function pullValue(ctx: Ctx, repo: Repo, pr: Pull, selections: GqlField[] | null): Record<string, unknown> | null {
  return selectFields(ctx, 'PullRequest', {
    id: globalId('P', repo.id, pr.number),
    number: pr.number,
    headRefName: pr.headRef,
    baseRefName: pr.baseRef,
    mergeable: ctx.store.mergeConflicts(repo, pr) ? 'CONFLICTING' : 'MERGEABLE',
    state: pr.merged ? 'MERGED' : pr.state === 'open' ? 'OPEN' : 'CLOSED',
    merged: pr.merged,
    __repo: repo,
    __pr: pr,
  }, selections);
}

function cursorFor(number: number): string {
  return Buffer.from(`issue:${number}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = /^issue:(\d+)$/.exec(text);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

function resolveField(ctx: Ctx, typeName: string, value: Record<string, unknown>, field: GqlField): unknown {
  const args: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(field.args)) args[k] = resolveVariables(v, ctx.variables);

  switch (typeName) {
    case 'Query': {
      if (field.name === 'viewer') {
        return selectFields(ctx, 'User', { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email }, field.selections);
      }
      if (field.name === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        const repo = ctx.store.getRepo(owner, name);
        if (!repo) return null;
        return selectFields(ctx, 'Repository', repositoryValue(repo), field.selections);
      }
      if (field.name === 'node') {
        const id = String(args.id ?? '');
        const decoded = decodeGlobalId(id);
        if (!decoded) return null;
        if (decoded.tag === 'R') {
          const repo = ctx.store.getRepoById(Number(decoded.parts[0]));
          if (!repo) return null;
          return selectFields(ctx, 'Repository', repositoryValue(repo), field.selections);
        }
        if (decoded.tag === 'I') {
          const repo = ctx.store.getRepoById(Number(decoded.parts[0]));
          if (!repo) return null;
          return issueValue(ctx, repo, Number(decoded.parts[1]), field.selections);
        }
        if (decoded.tag === 'P') {
          const repo = ctx.store.getRepoById(Number(decoded.parts[0]));
          if (!repo) return null;
          const pr = ctx.store.getPull(repo, Number(decoded.parts[1]));
          if (!pr) return null;
          return pullValue(ctx, repo, pr, field.selections);
        }
        return null;
      }
      if (field.name === '__type') {
        const name = String(args.name ?? '');
        const info = INTROSPECTION[name];
        if (!info) return null;
        return selectFields(ctx, '__Type', { name, kind: info.enumValues ? 'ENUM' : 'OBJECT', __fields: info.fields ?? [], __enumValues: info.enumValues ?? [] }, field.selections);
      }
      if (field.name === '__schema') return { queryType: { name: 'Query' } };
      return unknownField(ctx, typeName, field.name);
    }
    case 'Repository': {
      const repo = value.__repo as Repo;
      if (field.name === 'owner') {
        return selectFields(ctx, 'User', { login: repo.owner, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email }, field.selections);
      }
      if (field.name === 'defaultBranchRef') {
        const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        if (!tip) return null;
        return selectFields(ctx, 'Ref', { name: repo.defaultBranch }, field.selections);
      }
      if (field.name === 'issue') {
        return issueValue(ctx, repo, Number(args.number), field.selections);
      }
      if (field.name === 'issues') {
        const states = Array.isArray(args.states) ? (args.states as string[]) : null;
        const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
        const first = typeof args.first === 'number' ? args.first : 10;
        const after = typeof args.after === 'string' ? cursorNumber(args.after) : null;
        let issues = [...repo.issues];
        if (states) issues = issues.filter((i) => states.includes(i.state.toUpperCase()));
        if (orderBy.field === 'CREATED_AT' && orderBy.direction === 'DESC') {
          issues.sort((a, b) => b.createdSeq - a.createdSeq || b.number - a.number);
        } else if (orderBy.field === 'CREATED_AT' && orderBy.direction === 'ASC') {
          issues.sort((a, b) => a.createdSeq - b.createdSeq || a.number - b.number);
        } else {
          issues.sort((a, b) => a.number - b.number);
        }
        if (after !== null) {
          const idx = issues.findIndex((i) => i.number === after);
          issues = idx >= 0 ? issues.slice(idx + 1) : issues;
        }
        const page = issues.slice(0, first);
        const hasNext = issues.length > first;
        const endCursor = page.length > 0 ? cursorFor(page[page.length - 1].number) : null;
        const nodes = page.map((issue) => {
          const nodeSelections = field.selections?.find((s) => s.name === 'nodes')?.selections ?? null;
          return issueValue(ctx, repo, issue.number, nodeSelections);
        });
        const pageInfoSel = field.selections?.find((s) => s.name === 'pageInfo')?.selections ?? null;
        return selectFields(ctx, 'IssueConnection', {
          __nodes: nodes,
          __pageInfo: { hasNextPage: hasNext, endCursor },
          totalCount: issues.length,
          nodes,
          pageInfo: pageInfoSel ? selectFields(ctx, 'PageInfo', { hasNextPage: hasNext, endCursor }, pageInfoSel) : { hasNextPage: hasNext, endCursor },
        }, field.selections);
      }
      if (field.name === 'pullRequest') {
        const pr = ctx.store.getPull(repo, Number(args.number));
        if (!pr) return null;
        return pullValue(ctx, repo, pr, field.selections);
      }
      if (field.name === 'pullRequests') {
        const first = typeof args.first === 'number' ? args.first : 10;
        const nodes = [...repo.pulls].sort((a, b) => a.number - b.number).slice(0, first).map((pr) => {
          const nodeSelections = field.selections?.find((s) => s.name === 'nodes')?.selections ?? null;
          return pullValue(ctx, repo, pr, nodeSelections);
        });
        return selectFields(ctx, 'PullRequestConnection', { __nodes: nodes, nodes, totalCount: repo.pulls.length }, field.selections);
      }
      // Scalar repository fields live on the value object.
      return value[field.name] ?? null;
    }
    case 'PullRequest': {
      if (field.name === 'files') {
        const repo = value.__repo as Repo;
        const pr = value.__pr as Pull;
        const first = typeof args.first === 'number' ? args.first : 10;
        const files = ctx.store.pullFiles(repo, pr).slice(0, first);
        const nodeSelections = field.selections?.find((s) => s.name === 'nodes')?.selections ?? null;
        const nodes = files.map((f) => selectFields(ctx, 'File', { path: f.path }, nodeSelections));
        return selectFields(ctx, 'FileConnection', { nodes }, field.selections);
      }
      return value[field.name] ?? null;
    }
    case 'IssueConnection': {
      if (field.name === 'nodes') return value.__nodes ?? [];
      if (field.name === 'pageInfo') {
        const info = value.__pageInfo as Record<string, unknown>;
        return selectFields(ctx, 'PageInfo', info, field.selections);
      }
      return value[field.name] ?? null;
    }
    case 'PullRequestConnection': {
      if (field.name === 'nodes') return value.__nodes ?? [];
      return value[field.name] ?? null;
    }
    case 'FileConnection': {
      if (field.name === 'nodes') return value.nodes ?? [];
      return value[field.name] ?? null;
    }
    case '__Type': {
      if (field.name === 'fields') {
        const names = (value.__fields as string[]) ?? [];
        const nameSel = field.selections?.find((s) => s.name === 'name');
        void nameSel;
        return names.map((n) => selectFields(ctx, '__Field', { name: n }, field.selections));
      }
      if (field.name === 'enumValues') {
        const names = (value.__enumValues as string[]) ?? [];
        return names.map((n) => selectFields(ctx, '__EnumValue', { name: n }, field.selections));
      }
      return value[field.name] ?? null;
    }
    default:
      return value[field.name] ?? null;
  }
}

function repositoryValue(repo: Repo): Record<string, unknown> {
  return {
    __repo: repo,
    id: globalId('R', repo.id),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
  };
}

// ------------------------------------------------------------------ mutations

function executeMutation(ctx: Ctx, field: GqlField): Record<string, unknown> | null {
  const args: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(field.args)) args[k] = resolveVariables(v, ctx.variables);
  const input = (args.input ?? {}) as Record<string, unknown>;
  const key = field.alias ?? field.name;

  if (field.name === 'createIssue') {
    const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded?.tag === 'R' ? ctx.store.getRepoById(Number(decoded.parts[0])) : undefined;
    if (!repo) {
      ctx.errors.push({ message: 'repository not found' });
      return { [key]: null };
    }
    const issue = ctx.store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''), []);
    const sel = field.selections?.find((s) => s.name === 'issue')?.selections ?? null;
    return { [key]: { issue: issueValue(ctx, repo, issue.number, sel) } };
  }
  if (field.name === 'closeIssue') {
    const decoded = decodeGlobalId(String(input.issueId ?? ''));
    const repo = decoded?.tag === 'I' ? ctx.store.getRepoById(Number(decoded.parts[0])) : undefined;
    const issue = repo ? ctx.store.getIssue(repo, Number(decoded!.parts[1])) : undefined;
    if (!repo || !issue) {
      ctx.errors.push({ message: 'issue not found' });
      return { [key]: null };
    }
    issue.state = 'closed';
    issue.updatedSeq = ctx.store.nextSeq();
    const sel = field.selections?.find((s) => s.name === 'issue')?.selections ?? null;
    return { [key]: { issue: issueValue(ctx, repo, issue.number, sel) } };
  }
  if (field.name === 'createPullRequest') {
    const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
    const repo = decoded?.tag === 'R' ? ctx.store.getRepoById(Number(decoded.parts[0])) : undefined;
    if (!repo) {
      ctx.errors.push({ message: 'repository not found' });
      return { [key]: null };
    }
    const head = String(input.headRefName ?? '');
    const base = String(input.baseRefName ?? repo.defaultBranch);
    let pr = repo.pulls.find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
    if (!pr) {
      pr = ctx.store.createPull(repo, String(input.title ?? ''), String(input.body ?? ''), head, base) ?? undefined;
    }
    if (!pr) {
      ctx.errors.push({ message: 'head or base branch does not exist' });
      return { [key]: null };
    }
    const sel = field.selections?.find((s) => s.name === 'pullRequest')?.selections ?? null;
    return { [key]: { pullRequest: pullValue(ctx, repo, pr, sel) } };
  }
  if (field.name === 'mergePullRequest') {
    const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
    const repo = decoded?.tag === 'P' ? ctx.store.getRepoById(Number(decoded.parts[0])) : undefined;
    const pr = repo ? ctx.store.getPull(repo, Number(decoded!.parts[1])) : undefined;
    if (!repo || !pr) {
      ctx.errors.push({ message: 'pull request not found' });
      return { [key]: null };
    }
    if (pr.state === 'open') ctx.store.mergePull(repo, pr);
    const sel = field.selections?.find((s) => s.name === 'pullRequest')?.selections ?? null;
    return { [key]: { pullRequest: pullValue(ctx, repo, pr, sel) } };
  }
  if (field.name === 'addComment') {
    const decoded = decodeGlobalId(String(input.subjectId ?? ''));
    const repo = decoded?.tag === 'I' ? ctx.store.getRepoById(Number(decoded.parts[0])) : undefined;
    const issue = repo ? ctx.store.getIssue(repo, Number(decoded!.parts[1])) : undefined;
    if (!repo || !issue) {
      ctx.errors.push({ message: 'subject not found' });
      return { [key]: null };
    }
    const comment = ctx.store.addComment(repo, issue, String(input.body ?? ''));
    const nodeSel = field.selections
      ?.find((s) => s.name === 'commentEdge')
      ?.selections?.find((s) => s.name === 'node')?.selections ?? null;
    return {
      [key]: {
        commentEdge: {
          node: selectFields(ctx, 'Comment', { id: globalId('C', repo.id, issue.number, comment.id), body: comment.body }, nodeSel),
        },
      },
    };
  }
  ctx.errors.push({ message: `Cannot query field "${field.name}" on type "Mutation".` });
  return { [key]: null };
}

function executeOperation(store: Store, query: string, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: Ctx = { store, variables, errors: [] };
  let operation: GqlOperation;
  try {
    const parser = new Parser(tokenize(query));
    operation = parser.parseOperation();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'could not parse query' }] };
  }
  const data: Record<string, unknown> = {};
  if (operation.kind === 'mutation') {
    for (const field of operation.selections) {
      const result = executeMutation(ctx, field);
      if (result) Object.assign(data, result);
    }
  } else {
    for (const field of operation.selections) {
      const key = field.alias ?? field.name;
      if (field.name === 'viewer' || field.name === 'repository' || field.name === 'node' || field.name === '__type' || field.name === '__schema') {
        data[key] = resolveField(ctx, 'Query', {}, field);
      } else {
        data[key] = unknownField(ctx, 'Query', field.name);
      }
    }
  }
  const out: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) out.errors = ctx.errors;
  return out;
}

// ------------------------------------------------------------------ HTTP

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function isAuthorized(req: IncomingMessage): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (expected === '') return false;
  return req.headers.authorization === `Bearer ${expected}`;
}

/** Handle POST /api/graphql. Returns true when handled. */
export function handleGraphQL(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  rawBody: Buffer,
): boolean {
  if (pathname !== '/api/graphql') return false;
  if ((req.method ?? 'GET') !== 'POST') {
    sendJson(res, 405, { data: null, errors: [{ message: 'method not allowed' }] });
    return true;
  }
  if (!isAuthorized(req)) {
    sendJson(res, 401, { message: 'Bad credentials' });
    return true;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8')) as unknown;
  } catch {
    sendJson(res, 400, { data: null, errors: [{ message: 'request body is not valid JSON' }] });
    return true;
  }
  if (Array.isArray(parsed)) {
    const results = parsed.map((entry) => {
      const op = (entry ?? {}) as Record<string, unknown>;
      const query = typeof op.query === 'string' ? op.query : '';
      const variables = (op.variables ?? {}) as Record<string, unknown>;
      return executeOperation(store, query, variables);
    });
    sendJson(res, 200, results);
    return true;
  }
  const op = (parsed ?? {}) as Record<string, unknown>;
  const query = typeof op.query === 'string' ? op.query : '';
  const variables = (op.variables ?? {}) as Record<string, unknown>;
  sendJson(res, 200, executeOperation(store, query, variables));
  return true;
}

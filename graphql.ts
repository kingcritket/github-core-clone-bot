// GraphQL v4 (/api/graphql): a small parser + executor for exactly the documents gh and
// github-mcp-server send. It is not a general GraphQL engine: it parses operations,
// variables, aliases, inline fragments, enums, lists and input objects, resolves them
// against the same store REST uses, and answers unknown fields with HTTP 200 + errors.

import {
  VIEWER_LOGIN,
  globalId,
  parseGlobalId,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { diffTrees, getObject, mergeBase, parseCommit } from './git.ts';
import { doMergePull } from './rest.ts';

// ---------------------------------------------------------------- AST + parser

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  kind: 'field';
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
  selections: SelectionNode[] | null;
}

interface FragmentNode {
  kind: 'fragment';
  type: string;
  selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentNode;

interface Operation {
  op: 'query' | 'mutation';
  name: string | null;
  vars: Array<{ name: string; defaultValue?: ValueNode }>;
  selections: SelectionNode[];
}

interface Token {
  kind: 'punct' | 'name' | 'string' | 'int' | 'float';
  value: string;
}

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i] as string;
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}():!=$[]'.includes(c)) {
      tokens.push({ kind: 'punct', value: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const e = src[j + 1] as string;
          out += e === 'n' ? '\n' : e === 't' ? '\t' : e === 'r' ? '\r' : e;
          j += 2;
        } else {
          out += src[j] as string;
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      let isFloat = false;
      while (j < src.length && /[0-9.eE+-]/.test(src[j] as string)) {
        if (src[j] === '.' || src[j] === 'e' || src[j] === 'E') isFloat = true;
        j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(c)}`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  next(): Token {
    const t = this.tokens[this.pos];
    if (t === undefined) throw new Error('unexpected end of document');
    this.pos += 1;
    return t;
  }

  expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`expected ${v}, got ${t.value}`);
  }

  acceptPunct(v: string): boolean {
    const t = this.peek();
    if (t !== undefined && t.kind === 'punct' && t.value === v) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.pos < this.tokens.length) {
      ops.push(this.parseOperation());
    }
    return ops;
  }

  parseOperation(): Operation {
    let op: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const t = this.peek();
    if (t !== undefined && t.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
      op = this.next().value as 'query' | 'mutation';
      const n = this.peek();
      if (n !== undefined && n.kind === 'name') name = this.next().value;
    }
    const vars: Array<{ name: string; defaultValue?: ValueNode }> = [];
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        this.expectPunct('$');
        const vname = this.next().value;
        this.expectPunct(':');
        this.parseType();
        let defaultValue: ValueNode | undefined;
        if (this.acceptPunct('=')) defaultValue = this.parseValue();
        vars.push({ name: vname, defaultValue });
      }
    }
    const selections = this.parseSelectionSet();
    return { op, name, vars, selections };
  }

  parseType(): void {
    if (this.acceptPunct('[')) {
      this.parseType();
      this.expectPunct(']');
    } else {
      this.next();
    }
    this.acceptPunct('!');
  }

  parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const out: SelectionNode[] = [];
    while (!this.acceptPunct('}')) {
      if (this.acceptPunct('...')) {
        const t = this.peek();
        if (t !== undefined && t.kind === 'name' && t.value === 'on') {
          this.next();
          const type = this.next().value;
          out.push({ kind: 'fragment', type, selections: this.parseSelectionSet() });
        } else {
          // A named fragment spread: unsupported; skip its selection set if any.
          this.next();
          if (this.peek()?.kind === 'punct' && this.peek()?.value === '{') this.parseSelectionSet();
        }
      } else {
        const first = this.next();
        if (first.kind !== 'name') throw new Error(`expected field name, got ${first.value}`);
        let alias: string | null = null;
        let fieldName = first.value;
        if (this.acceptPunct(':')) {
          alias = fieldName;
          fieldName = this.next().value;
        }
        const args: Array<{ name: string; value: ValueNode }> = [];
        if (this.acceptPunct('(')) {
          while (!this.acceptPunct(')')) {
            const aname = this.next().value;
            this.expectPunct(':');
            args.push({ name: aname, value: this.parseValue() });
          }
        }
        let selections: SelectionNode[] | null = null;
        if (this.peek()?.kind === 'punct' && this.peek()?.value === '{') selections = this.parseSelectionSet();
        out.push({ kind: 'field', alias, name: fieldName, args, selections });
      }
    }
    return out;
  }

  parseValue(): ValueNode {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') return { kind: 'variable', name: this.next().value };
    if (t.kind === 'string') return { kind: 'string', value: t.value };
    if (t.kind === 'int') return { kind: 'int', value: Number(t.value) };
    if (t.kind === 'float') return { kind: 'float', value: Number(t.value) };
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'boolean', value: true };
      if (t.value === 'false') return { kind: 'boolean', value: false };
      if (t.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const values: ValueNode[] = [];
      while (!this.acceptPunct(']')) values.push(this.parseValue());
      return { kind: 'list', values };
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (!this.acceptPunct('}')) {
        const fname = this.next().value;
        this.expectPunct(':');
        fields.push({ name: fname, value: this.parseValue() });
      }
      return { kind: 'object', fields };
    }
    throw new Error(`unexpected value token ${t.value}`);
  }
}

// ---------------------------------------------------------------- executor

interface GqlError {
  message: string;
  path?: Array<string | number>;
}

interface ExecCtx {
  store: Store;
  vars: Record<string, unknown>;
  errors: GqlError[];
}

type AnyObj = Record<string, unknown> & { __type: string };

function valueOf(node: ValueNode, vars: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return vars[node.name];
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((v) => valueOf(v, vars));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = valueOf(f.value, vars);
      return out;
    }
  }
}

function argOf(field: FieldNode, name: string, ctx: ExecCtx): unknown {
  const a = field.args.find((x) => x.name === name);
  if (a === undefined) return undefined;
  return valueOf(a.value, ctx.vars);
}

const ISSUE_FIELDS = ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'url', 'repository'];
const PULL_FIELDS = ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'files', 'author', 'createdAt', 'updatedAt'];
const REPO_FIELDS = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private', 'createdAt', 'updatedAt'];
const SEARCH_ENUM = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE', 'COMMIT'];

function typeFields(type: string): string[] {
  if (type === 'Issue') return ISSUE_FIELDS;
  if (type === 'PullRequest') return PULL_FIELDS;
  if (type === 'Repository') return REPO_FIELDS;
  if (type === 'User') return ['id', 'login', 'name'];
  if (type === 'Ref') return ['name', 'prefix'];
  if (type === 'IssueConnection') return ['nodes', 'edges', 'pageInfo', 'totalCount'];
  if (type === 'PullRequestConnection') return ['nodes', 'edges', 'pageInfo', 'totalCount'];
  if (type === 'PullRequestChangedFileConnection') return ['nodes', 'edges', 'pageInfo', 'totalCount'];
  if (type === 'PullRequestChangedFile') return ['path', 'additions', 'deletions', 'changeType'];
  if (type === 'PageInfo') return ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'];
  if (type === 'IssueComment') return ['id', 'body', 'author', 'createdAt', 'updatedAt'];
  if (type === 'IssueCommentEdge') return ['node', 'cursor'];
  if (type === '__Type') return ['name', 'kind', 'fields', 'enumValues'];
  if (type === 'Query') return ['viewer', 'repository', 'node', '__type', 'search'];
  if (type === 'Mutation') return ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'];
  if (type === 'CreateIssuePayload' || type === 'CloseIssuePayload') return ['issue'];
  if (type === 'CreatePullRequestPayload' || type === 'MergePullRequestPayload') return ['pullRequest'];
  if (type === 'AddCommentPayload') return ['commentEdge'];
  return [];
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function wrapIssue(repo: Repo, issue: Issue): AnyObj {
  return { __type: 'Issue', __repo: repo, __issue: issue };
}

function wrapPull(repo: Repo, pull: Pull): AnyObj {
  return { __type: 'PullRequest', __repo: repo, __pull: pull };
}

function wrapRepo(repo: Repo): AnyObj {
  return { __type: 'Repository', __repo: repo };
}

function cursorFor(n: number): string {
  return Buffer.from(`cursor:${n}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string | undefined): number | null {
  if (cursor === undefined || cursor === null || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m === null ? null : Number(m[1]);
  } catch {
    return null;
  }
}

function resolveIssues(repo: Repo, field: FieldNode, ctx: ExecCtx): AnyObj {
  const first = argOf(field, 'first', ctx);
  const after = argOf(field, 'after', ctx);
  const states = argOf(field, 'states', ctx);
  const orderBy = argOf(field, 'orderBy', ctx) as { field?: string; direction?: string } | undefined;
  let items = repo.issues.slice();
  if (Array.isArray(states) && states.length > 0) {
    const want = new Set(states.map((s) => String(s)));
    items = items.filter((i) => want.has(issueState(i)));
  }
  const desc = orderBy?.direction !== 'ASC';
  items.sort((a, b) => {
    const d = desc ? b.seq - a.seq : a.seq - b.seq;
    if (d !== 0) return d;
    return desc ? b.number - a.number : a.number - b.number;
  });
  const afterN = cursorNumber(typeof after === 'string' ? after : undefined);
  if (afterN !== null) {
    items = items.filter((i) => (desc ? i.number < afterN : i.number > afterN));
  }
  const limit = typeof first === 'number' ? first : items.length;
  const page = items.slice(0, limit);
  const hasNext = items.length > page.length;
  const endCursor = page.length > 0 ? cursorFor(page[page.length - 1]?.number ?? 0) : null;
  return {
    __type: 'IssueConnection',
    __nodes: page.map((i) => wrapIssue(repo, i)),
    __pageInfo: { hasNextPage: hasNext, hasPreviousPage: false, startCursor: page.length > 0 ? cursorFor(page[0]?.number ?? 0) : null, endCursor },
  };
}

function resolvePullRequests(repo: Repo, field: FieldNode): AnyObj {
  const items = repo.pulls.slice().sort((a, b) => b.number - a.number);
  const first = 0;
  void first;
  void field;
  return { __type: 'PullRequestConnection', __nodes: items.map((p) => wrapPull(repo, p)) };
}

function resolveFiles(repo: Repo, pull: Pull, field: FieldNode, ctx: ExecCtx): AnyObj {
  const first = argOf(field, 'first', ctx);
  const headObj = getObject(repo, pull.headSha);
  const base = mergeBase(repo, pull.headSha, pull.baseSha);
  const baseTree = base === null ? null : parseCommit(getObject(repo, base)?.data ?? Buffer.alloc(0)).tree;
  const headTree = headObj !== undefined && headObj.type === 'commit' ? parseCommit(headObj.data).tree : null;
  const diffs = diffTrees(repo, baseTree, headTree).filter((d) => d.status !== 'removed');
  const limit = typeof first === 'number' ? first : diffs.length;
  const nodes = diffs.slice(0, limit).map((d) => ({ __type: 'PullRequestChangedFile', path: d.path }));
  return { __type: 'PullRequestChangedFileConnection', __nodes: nodes };
}

/** Whether merging this pull would conflict (a file changed on both sides differently). */
export function pullIsConflicted(repo: Repo, pull: Pull): boolean {
  const base = mergeBase(repo, pull.headSha, pull.baseSha);
  const headObj = getObject(repo, pull.headSha);
  const baseObj = getObject(repo, pull.baseSha);
  if (headObj === undefined || baseObj === undefined) return false;
  const headTree = parseCommit(headObj.data).tree;
  const baseTree = parseCommit(baseObj.data).tree;
  const baseOfMerge = base === null ? null : parseCommit(getObject(repo, base)?.data ?? Buffer.alloc(0)).tree;
  const baseChanges = diffTrees(repo, baseOfMerge, baseTree);
  const headChanges = diffTrees(repo, baseOfMerge, headTree);
  const headMap = new Map(headChanges.map((d) => [d.path, d.newOid]));
  for (const bc of baseChanges) {
    if (!headMap.has(bc.path)) continue;
    const baseOid = treeLookupOf(repo, baseTree, bc.path);
    const headOid = treeLookupOf(repo, headTree, bc.path);
    if (baseOid !== headOid) return true;
  }
  return false;
}

function treeLookupOf(repo: Repo, tree: string, path: string): string | null {
  const parts = path.split('/');
  let cur = tree;
  for (let i = 0; i < parts.length; i += 1) {
    const obj = getObject(repo, cur);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === parts[i]);
    if (entry === undefined) return null;
    if (i === parts.length - 1) return entry.oid;
    cur = entry.oid;
  }
  return null;
}

function resolveField(parent: AnyObj, field: FieldNode, ctx: ExecCtx): unknown {
  const type = parent.__type;
  const name = field.name;
  const repo = parent.__repo as Repo | undefined;

  if (type === 'CreateIssuePayload' || type === 'CloseIssuePayload') {
    if (name === 'issue') return parent.issue;
  }
  if (type === 'CreatePullRequestPayload' || type === 'MergePullRequestPayload') {
    if (name === 'pullRequest') return parent.pullRequest;
  }
  if (type === 'AddCommentPayload') {
    if (name === 'commentEdge') return parent.commentEdge;
  }

  if (type === 'Query') {
    if (name === 'viewer') return { __type: 'User', login: VIEWER_LOGIN };
    if (name === 'repository') {
      const owner = String(argOf(field, 'owner', ctx) ?? '');
      const rname = String(argOf(field, 'name', ctx) ?? '');
      const found = ctx.store.viewerRepo(owner, rname);
      return found === undefined ? null : wrapRepo(found);
    }
    if (name === 'node') {
      const id = String(argOf(field, 'id', ctx) ?? '');
      const parsed = parseGlobalId(id);
      if (parsed === null) return null;
      if (parsed.kind === 'Repository') {
        for (const r of ctx.store.repos.values()) if (r.id === parsed.n) return wrapRepo(r);
        return null;
      }
      if (parsed.kind === 'Issue') {
        for (const r of ctx.store.repos.values()) {
          const issue = r.issues.find((i) => i.id === parsed.n);
          if (issue !== undefined) return wrapIssue(r, issue);
        }
        return null;
      }
      if (parsed.kind === 'PullRequest') {
        for (const r of ctx.store.repos.values()) {
          const pull = r.pulls.find((p) => p.id === parsed.n);
          if (pull !== undefined) return wrapPull(r, pull);
        }
        return null;
      }
      return null;
    }
    if (name === '__type') {
      const tname = String(argOf(field, 'name', ctx) ?? '');
      if (tname === 'SearchType') return { __type: '__Type', name: tname, kind: 'ENUM', enumValues: SEARCH_ENUM.map((n) => ({ name: n })) };
      const fields = typeFields(tname);
      if (fields.length === 0 && tname !== 'Issue' && tname !== 'PullRequest' && tname !== 'Repository') return null;
      return { __type: '__Type', name: tname, kind: 'OBJECT', fields: fields.map((n) => ({ name: n })) };
    }
    if (name === 'search') return { __type: 'SearchResultItemConnection', __nodes: [] };
  }

  if (type === 'User') {
    if (name === 'login') return parent.login;
    if (name === 'id') return globalId('User', 1000);
    if (name === 'name') return 'ada iversen';
  }

  if (type === 'Repository' && repo !== undefined) {
    if (name === 'id') return globalId('Repository', repo.id);
    if (name === 'databaseId') return repo.id;
    if (name === 'name') return repo.name;
    if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (name === 'owner') return { __type: 'User', login: repo.owner };
    if (name === 'defaultBranchRef') return { __type: 'Ref', name: repo.defaultBranch };
    if (name === 'viewerPermission') return 'ADMIN';
    if (name === 'hasIssuesEnabled') return true;
    if (name === 'description') return repo.description;
    if (name === 'private') return repo.isPrivate;
    if (name === 'issue') {
      const number = Number(argOf(field, 'number', ctx));
      const issue = repo.issues.find((i) => i.number === number);
      return issue === undefined ? null : wrapIssue(repo, issue);
    }
    if (name === 'issues') return resolveIssues(repo, field, ctx);
    if (name === 'pullRequests') return resolvePullRequests(repo, field);
    if (name === 'pullRequest') {
      const number = Number(argOf(field, 'number', ctx));
      const pull = repo.pulls.find((p) => p.number === number);
      return pull === undefined ? null : wrapPull(repo, pull);
    }
  }

  if (type === 'Issue' && repo !== undefined) {
    const issue = parent.__issue as Issue;
    if (name === 'id') return globalId('Issue', issue.id);
    if (name === 'number') return issue.number;
    if (name === 'title') return issue.title;
    if (name === 'state') return issueState(issue);
    if (name === 'body') return issue.body;
    if (name === 'createdAt') return issue.createdAt;
    if (name === 'updatedAt') return issue.updatedAt;
    if (name === 'author') return { __type: 'User', login: VIEWER_LOGIN };
    if (name === 'repository') return wrapRepo(repo);
  }

  if (type === 'PullRequest' && repo !== undefined) {
    const pull = parent.__pull as Pull;
    if (name === 'id') return globalId('PullRequest', pull.id);
    if (name === 'number') return pull.number;
    if (name === 'title') return pull.title;
    if (name === 'body') return pull.body;
    if (name === 'state') return pullState(pull);
    if (name === 'merged') return pull.merged;
    if (name === 'mergeable') return pullIsConflicted(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
    if (name === 'headRefName') return pull.headRef;
    if (name === 'baseRefName') return pull.baseRef;
    if (name === 'headRef') return { __type: 'Ref', name: pull.headRef };
    if (name === 'baseRef') return { __type: 'Ref', name: pull.baseRef };
    if (name === 'files') return resolveFiles(repo, pull, field, ctx);
    if (name === 'author') return { __type: 'User', login: VIEWER_LOGIN };
    if (name === 'createdAt') return pull.createdAt;
    if (name === 'updatedAt') return pull.updatedAt;
  }

  if (type === 'Ref') {
    if (name === 'name') return parent.name;
    if (name === 'prefix') return 'refs/heads/';
  }

  if (type === 'IssueConnection' || type === 'PullRequestConnection' || type === 'PullRequestChangedFileConnection') {
    if (name === 'nodes') return (parent.__nodes as unknown[]) ?? [];
    if (name === 'edges') return ((parent.__nodes as unknown[]) ?? []).map((n) => ({ __type: 'Edge', node: n }));
    if (name === 'pageInfo') return parent.__pageInfo ?? { hasNextPage: false, hasPreviousPage: false, startCursor: null, endCursor: null };
    if (name === 'totalCount') return (parent.__nodes as unknown[])?.length ?? 0;
  }

  if (type === 'PullRequestChangedFile') {
    if (name === 'path') return parent.path;
    if (name === 'additions') return 1;
    if (name === 'deletions') return 0;
    if (name === 'changeType') return 'ADDED';
  }

  if (type === 'PageInfo') {
    if (name === 'hasNextPage') return (parent as unknown as Record<string, unknown>).hasNextPage ?? false;
    if (name === 'hasPreviousPage') return (parent as unknown as Record<string, unknown>).hasPreviousPage ?? false;
    if (name === 'startCursor') return (parent as unknown as Record<string, unknown>).startCursor ?? null;
    if (name === 'endCursor') return (parent as unknown as Record<string, unknown>).endCursor ?? null;
  }

  if (type === '__Type') {
    if (name === 'name') return parent.name;
    if (name === 'kind') return parent.kind;
    if (name === 'fields') return (parent.fields as unknown[]) ?? null;
    if (name === 'enumValues') return (parent.enumValues as unknown[]) ?? null;
  }

  if (type === 'IssueComment') {
    if (name === 'id') return parent.id;
    if (name === 'body') return parent.body;
    if (name === 'author') return { __type: 'User', login: VIEWER_LOGIN };
  }

  if (type === 'IssueCommentEdge' || type === 'Edge') {
    if (name === 'node') return parent.node;
    if (name === 'cursor') return parent.cursor ?? null;
  }

  if (type === 'SearchResultItemConnection') {
    if (name === 'nodes') return [];
    if (name === 'pageInfo') return { hasNextPage: false, hasPreviousPage: false, startCursor: null, endCursor: null };
  }

  return undefined;
}

function typeNameOf(value: unknown): string | null {
  if (value === null || typeof value !== 'object') return null;
  const v = value as AnyObj;
  return typeof v.__type === 'string' ? v.__type : null;
}

function resolveSelections(parent: AnyObj, selections: SelectionNode[], ctx: ExecCtx, path: Array<string | number>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const type = parent.__type;
  for (const sel of selections) {
    if (sel.kind === 'fragment') {
      if (sel.type === type) {
        Object.assign(out, resolveSelections(parent, sel.selections, ctx, path));
      }
      continue;
    }
    const key = sel.alias ?? sel.name;
    const known = typeFields(type).includes(sel.name) || sel.name.startsWith('__');
    if (!known) {
      ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "${type}".`, path: [...path, key] });
      continue;
    }
    const value = resolveField(parent, sel, ctx);
    if (value === undefined) {
      ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "${type}".`, path: [...path, key] });
      continue;
    }
    out[key] = resolveValue(value, sel.selections, ctx, [...path, key]);
  }
  return out;
}

function resolveValue(value: unknown, selections: SelectionNode[] | null, ctx: ExecCtx, path: Array<string | number>): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((v, i) => resolveValue(v, selections, ctx, [...path, i]));
  if (typeof value !== 'object') return value;
  const type = typeNameOf(value);
  if (type === null || selections === null) {
    // A plain object (pageInfo, __Type fields list items, edges): resolve its keys as data.
    if (selections === null) return value;
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      if (sel.kind === 'fragment') continue;
      const key = sel.alias ?? sel.name;
      const raw = (value as Record<string, unknown>)[sel.name];
      if (raw === undefined) {
        // Nested selection on a plain value (e.g. fields { name } on __Type entries).
        if (sel.selections !== null && typeof (value as Record<string, unknown>).name === 'string') {
          out[key] = resolveValue(value, sel.selections, ctx, [...path, key]);
        } else {
          out[key] = null;
        }
        continue;
      }
      out[key] = resolveValue(raw, sel.selections, ctx, [...path, key]);
    }
    return out;
  }
  return resolveSelections(value as AnyObj, selections, ctx, path);
}

// ---------------------------------------------------------------- mutations

function createIssueGql(store: Store, input: Record<string, unknown>): AnyObj {
  const repoId = parseGlobalId(String(input.repositoryId ?? ''));
  let repo: Repo | undefined;
  if (repoId !== null && repoId.kind === 'Repository') {
    for (const r of store.repos.values()) if (r.id === repoId.n) repo = r;
  }
  if (repo === undefined) throw new Error('repository not found');
  const title = String(input.title ?? '');
  const bodyText = typeof input.body === 'string' ? input.body : '';
  const now = store.nowIso();
  const issue: Issue = {
    id: store.nextIssueId++,
    number: repo.nextIssue++,
    title,
    body: bodyText,
    state: 'open',
    labels: [],
    seq: repo.issues.length + 1,
    createdAt: now,
    updatedAt: now,
  };
  repo.issues.push(issue);
  return { __type: 'Issue', __repo: repo, __issue: issue };
}

function closeIssueGql(store: Store, input: Record<string, unknown>): AnyObj {
  const parsed = parseGlobalId(String(input.issueId ?? ''));
  if (parsed === null || parsed.kind !== 'Issue') throw new Error('issue not found');
  for (const r of store.repos.values()) {
    const issue = r.issues.find((i) => i.id === parsed.n);
    if (issue !== undefined) {
      issue.state = 'closed';
      issue.updatedAt = store.nowIso();
      return { __type: 'Issue', __repo: r, __issue: issue };
    }
  }
  throw new Error('issue not found');
}

function createPullGql(store: Store, input: Record<string, unknown>): AnyObj {
  const repoId = parseGlobalId(String(input.repositoryId ?? ''));
  let repo: Repo | undefined;
  if (repoId !== null && repoId.kind === 'Repository') {
    for (const r of store.repos.values()) if (r.id === repoId.n) repo = r;
  }
  if (repo === undefined) throw new Error('repository not found');
  const head = String(input.headRefName ?? '');
  const base = String(input.baseRefName ?? repo.defaultBranch);
  const headSha = repo.refs.get(`refs/heads/${head}`);
  const baseSha = repo.refs.get(`refs/heads/${base}`);
  if (headSha === undefined || baseSha === undefined) throw new Error('head or base branch not found');
  const now = store.nowIso();
  const pull: Pull = {
    id: store.nextPullId++,
    number: repo.nextPull++,
    title: String(input.title ?? ''),
    body: typeof input.body === 'string' ? input.body : '',
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeSha: null,
    headRef: head,
    baseRef: base,
    headSha,
    baseSha,
    seq: repo.pulls.length + 1,
    createdAt: now,
    updatedAt: now,
  };
  repo.pulls.push(pull);
  return { __type: 'PullRequest', __repo: repo, __pull: pull };
}

function resolveMutation(parent: AnyObj, field: FieldNode, ctx: ExecCtx): unknown {
  const input = (argOf(field, 'input', ctx) ?? {}) as Record<string, unknown>;
  if (field.name === 'createIssue') {
    const issue = createIssueGql(ctx.store, input);
    return { __type: 'CreateIssuePayload', issue };
  }
  if (field.name === 'closeIssue') {
    const issue = closeIssueGql(ctx.store, input);
    return { __type: 'CloseIssuePayload', issue };
  }
  if (field.name === 'createPullRequest') {
    const pull = createPullGql(ctx.store, input);
    return { __type: 'CreatePullRequestPayload', pullRequest: pull };
  }
  if (field.name === 'mergePullRequest') {
    const parsed = parseGlobalId(String(input.pullRequestId ?? ''));
    if (parsed === null || parsed.kind !== 'PullRequest') throw new Error('pull request not found');
    for (const r of ctx.store.repos.values()) {
      const pull = r.pulls.find((p) => p.id === parsed.n);
      if (pull !== undefined) {
        const merged = doMergePull(ctx.store, r, pull);
        if (!merged) throw new Error('merge conflict');
        return { __type: 'MergePullRequestPayload', pullRequest: { __type: 'PullRequest', __repo: r, __pull: pull } };
      }
    }
    throw new Error('pull request not found');
  }
  if (field.name === 'addComment') {
    const parsed = parseGlobalId(String(input.subjectId ?? ''));
    if (parsed === null || parsed.kind !== 'Issue') throw new Error('subject not found');
    for (const r of ctx.store.repos.values()) {
      const issue = r.issues.find((i) => i.id === parsed.n);
      if (issue !== undefined) {
        const now = ctx.store.nowIso();
        const comment = { id: ctx.store.nextCommentId++, body: String(input.body ?? ''), seq: (r.comments.get(issue.number)?.length ?? 0) + 1, createdAt: now, updatedAt: now };
        const list = r.comments.get(issue.number) ?? [];
        list.push(comment);
        r.comments.set(issue.number, list);
        const node = { __type: 'IssueComment', id: globalId('IssueComment', comment.id), body: comment.body };
        return { __type: 'AddCommentPayload', commentEdge: { __type: 'IssueCommentEdge', node, cursor: cursorFor(comment.id) } };
      }
    }
    throw new Error('subject not found');
  }
  return undefined;
}

// The merge itself lives in rest.ts (shared with PUT /pulls/{n}/merge).

function executeOperation(store: Store, op: Operation, variables: Record<string, unknown>): { data: unknown; errors?: GqlError[] } {
  const ctx: ExecCtx = { store, vars: variables, errors: [] };
  try {
    if (op.op === 'mutation') {
      const root: AnyObj = { __type: 'Mutation' };
      const out: Record<string, unknown> = {};
      for (const sel of op.selections) {
        if (sel.kind === 'fragment') continue;
        const key = sel.alias ?? sel.name;
        const payload = resolveMutation(root, sel, ctx);
        if (payload === undefined) {
          ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "Mutation".` });
          continue;
        }
        out[key] = resolveValue(payload, sel.selections, ctx, [key]);
      }
      const result: { data: unknown; errors?: GqlError[] } = { data: out };
      if (ctx.errors.length > 0) result.errors = ctx.errors;
      return result;
    }
    const root: AnyObj = { __type: 'Query' };
    const data = resolveSelections(root, op.selections, ctx, []);
    const result: { data: unknown; errors?: GqlError[] } = { data };
    if (ctx.errors.length > 0) result.errors = ctx.errors;
    return result;
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Internal error' }] };
  }
}

export interface GqlOutcome {
  status: number;
  body: unknown;
}

/** Execute one GraphQL request body (a single operation or a batch array). */
export function handleGraphql(store: Store, body: unknown): GqlOutcome {
  if (Array.isArray(body)) {
    const out = body.map((item) => {
      if (typeof item !== 'object' || item === null) return { data: null, errors: [{ message: 'Invalid operation' }] };
      return executeOne(store, item as Record<string, unknown>);
    });
    return { status: 200, body: out };
  }
  if (typeof body === 'object' && body !== null) {
    return { status: 200, body: executeOne(store, body as Record<string, unknown>) };
  }
  return { status: 400, body: { errors: [{ message: 'Invalid GraphQL body' }] } };
}

function executeOne(store: Store, item: Record<string, unknown>): { data: unknown; errors?: GqlError[] } {
  const query = typeof item.query === 'string' ? item.query : '';
  const variables = (item.variables ?? {}) as Record<string, unknown>;
  let ops: Operation[];
  try {
    ops = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Parse error' }] };
  }
  if (ops.length === 0) return { data: null, errors: [{ message: 'Empty document' }] };
  const op = ops[0] as Operation;
  const vars: Record<string, unknown> = { ...variables };
  for (const v of op.vars) {
    if (!(v.name in vars) && v.defaultValue !== undefined) vars[v.name] = valueOf(v.defaultValue, vars);
  }
  return executeOperation(store, op, vars);
}


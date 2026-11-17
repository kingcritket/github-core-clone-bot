// MCP endpoint (POST /mcp): JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28.
// server/discover negotiates the revision; initialize is also answered for the legacy
// handshake era (2025-11-25 and earlier). Tools act on the same store as REST and
// GraphQL, so a repository or issue created here reads back over /api/v3 unchanged.

import { VIEWER_LOGIN, globalId, type Issue, type Pull, type Repo, type Store } from './store.ts';
import { doMergePull, issueJson, pullJson, repoJson } from './rest.ts';

export const MCP_VERSION = '2026-07-28';
export const MCP_SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const SERVER_NAME = 'github-core-clone';
const SERVER_VERSION = '1.0.0';

interface ToolDef {
  name: string;
  description: string;
  properties: Record<string, { type: string; description: string }>;
  required: string[];
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    properties: {
      name: { type: 'string', description: 'The repository name.' },
      owner: { type: 'string', description: 'The owner login (the authenticated user).' },
      description: { type: 'string', description: 'A short description of the repository.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
    },
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
    },
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List issues of a repository, optionally filtered by state.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Issue state: open, closed or all.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch onto a base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      body: { type: 'string', description: 'The pull request body.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash commit onto its base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'integer', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
  },
];

function toolsList(): unknown[] {
  return TOOLS.map((t) => ({
    name: t.name,
    description: t.description,
    inputSchema: { type: 'object', properties: t.properties, required: t.required },
  }));
}

function norm(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]/g, '');
}

function findTool(name: string): ToolDef | undefined {
  const n = norm(name);
  for (const t of TOOLS) {
    if (norm(t.name) === n) return t;
  }
  for (const t of TOOLS) {
    const tn = norm(t.name);
    if (n.length > tn.length && n.endsWith(tn)) return t;
  }
  return undefined;
}

interface RpcOk {
  jsonrpc: '2.0';
  id: unknown;
  result: unknown;
}

interface RpcErr {
  jsonrpc: '2.0';
  id: unknown;
  error: { code: number; message: string; data?: unknown };
}

function rpcOk(id: unknown, result: unknown): RpcOk {
  return { jsonrpc: '2.0', id, result };
}

function rpcErr(id: unknown, code: number, message: string, data?: unknown): RpcErr {
  const error: { code: number; message: string; data?: unknown } = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: '2.0', id, error };
}

function toolResult(content: unknown, isError = false): Record<string, unknown> {
  const text = typeof content === 'string' ? content : JSON.stringify(content);
  const out: Record<string, unknown> = {
    content: [{ type: 'text', text }],
    structuredContent: content,
  };
  if (isError) out.isError = true;
  return out;
}

function getRepo(store: Store, owner: string, repoName: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return store.getRepo(owner, repoName) ?? null;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = findTool(name);
  if (tool === undefined) throw { code: -32602, message: `Unknown tool: ${name}` };
  // Missing required arguments.
  for (const req of tool.required) {
    if (args[req] === undefined || args[req] === null || args[req] === '') {
      return toolResult(`Missing required argument: ${req}`, true);
    }
  }
  const owner = String(args.owner ?? VIEWER_LOGIN);
  const repoName = String(args.repo ?? args.repository ?? args.name ?? '');

  if (tool.name === 'create_repo') {
    const repoNameArg = String(args.name ?? '');
    if (repoNameArg === '') return toolResult('Missing required argument: name', true);
    const existing = store.getRepo(VIEWER_LOGIN, repoNameArg);
    const repo = existing ?? store.createRepo(repoNameArg, args.private === true, typeof args.description === 'string' ? args.description : '');
    const out = repoJson(repo);
    return toolResult(out);
  }

  const repo = getRepo(store, owner, repoName);
  if (repo === null) return toolResult(`Repository not found: ${owner}/${repoName}`, true);

  if (tool.name === 'get_repo') {
    return toolResult(repoJson(repo));
  }

  if (tool.name === 'create_issue') {
    const title = String(args.title ?? '');
    const bodyText = typeof args.body === 'string' ? args.body : '';
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
    return toolResult(issueJson(repo, issue));
  }

  if (tool.name === 'list_issues') {
    const state = typeof args.state === 'string' ? args.state : 'open';
    let items = repo.issues.slice();
    if (state !== 'all') items = items.filter((i) => i.state === state);
    items.sort((a, b) => a.number - b.number);
    return toolResult(items.map((i) => issueJson(repo, i)));
  }

  if (tool.name === 'create_pull_request') {
    const title = String(args.title ?? '');
    const bodyText = typeof args.body === 'string' ? args.body : '';
    const head = String(args.head ?? '');
    const base = String(args.base ?? repo.defaultBranch);
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (headSha === undefined) return toolResult(`Head branch "${head}" does not exist`, true);
    if (baseSha === undefined) return toolResult(`Base branch "${base}" does not exist`, true);
    const now = store.nowIso();
    const pull: Pull = {
      id: store.nextPullId++,
      number: repo.nextPull++,
      title,
      body: bodyText,
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
    return toolResult(pullJson(repo, pull));
  }

  if (tool.name === 'merge_pull_request') {
    const n = Number(args.pull_number ?? args.number ?? 0);
    const pull = repo.pulls.find((p) => p.number === n);
    if (pull === undefined) return toolResult(`Pull request not found: ${n}`, true);
    if (pull.merged || pull.state === 'closed') return toolResult(`Pull request #${n} is already merged or closed`, true);
    const merged = doMergePull(store, repo, pull);
    if (!merged) return toolResult(`Merge conflict: pull request #${n} cannot be merged`, true);
    return toolResult({ sha: pull.mergeSha, merged: true, message: 'Pull request successfully merged', number: pull.number });
  }

  return toolResult(`Unknown tool: ${name}`, true);
}

export interface McpOutcome {
  status: number;
  body: unknown;
}

/**
 * Handle one POST /mcp request. `rawBody` is the exact request bytes (a non-JSON body
 * must answer HTTP 400 with JSON-RPC -32700).
 */
export function handleMcp(store: Store, rawBody: Buffer): McpOutcome {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8'));
  } catch {
    return { status: 400, body: rpcErr(null, -32700, 'Parse error') };
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return { status: 400, body: rpcErr(null, -32600, 'Invalid Request') };
  }
  const req = parsed as Record<string, unknown>;
  const id = req.id ?? null;
  const method = typeof req.method === 'string' ? req.method : '';
  const params = (req.params ?? {}) as Record<string, unknown>;

  // A notification (no id) needs no reply; answer 202 with no body.
  if (req.id === undefined) {
    return { status: 202, body: null };
  }

  if (method === 'server/discover') {
    return {
      status: 200,
      body: rpcOk(id, {
        supportedVersions: MCP_SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      }),
    };
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : MCP_VERSION;
    const negotiated = MCP_SUPPORTED_VERSIONS.includes(requested) ? requested : MCP_VERSION;
    return {
      status: 200,
      body: rpcOk(id, {
        protocolVersion: negotiated,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      }),
    };
  }

  if (method === 'tools/list') {
    return { status: 200, body: rpcOk(id, { tools: toolsList() }) };
  }

  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    // Accept a full_name ("owner/repo") when owner/repo are absent.
    if ((args.owner === undefined || args.repo === undefined) && typeof args.full_name === 'string') {
      const parts = (args.full_name as string).split('/');
      if (parts.length === 2) {
        if (args.owner === undefined) args.owner = parts[0];
        if (args.repo === undefined) args.repo = parts[1];
      }
    }
    try {
      const result = callTool(store, name, args);
      return { status: 200, body: rpcOk(id, result) };
    } catch (err) {
      if (typeof err === 'object' && err !== null && 'code' in err) {
        const e = err as { code: number; message: string };
        return { status: 200, body: rpcErr(id, e.code, e.message) };
      }
      return { status: 200, body: rpcErr(id, -32603, err instanceof Error ? err.message : 'Internal error') };
    }
  }

  if (method === 'ping') {
    return { status: 200, body: rpcOk(id, {}) };
  }

  return { status: 200, body: rpcErr(id, -32601, `Method not found: ${method}`) };
}

export { globalId };

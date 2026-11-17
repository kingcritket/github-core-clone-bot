// REST v3 API (/api/v3): repositories, branches, commits, contents, labels, issues,
// comments, pull requests, files and merges. Every reply is JSON with a non-empty
// X-GitHub-Request-Id (added by app.ts); errors carry {"message": ...}.
// Auth is enforced by app.ts before this runs: a missing token never reaches here.

import {
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  globalId,
  identLine,
  viewerUser,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import {
  buildCommit,
  commitOnBranch,
  diffTrees,
  flattenTree,
  getObject,
  isAncestor,
  isOid,
  mergeBase,
  parseCommit,
  parseTree,
  storeObject,
  treeLookup,
  treeSetPath,
} from './git.ts';

export interface RestResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const ok = (body: unknown, status = 200, headers?: Record<string, string>): RestResult => ({ status, body, headers });
const notFound = (message = 'Not Found'): RestResult => ({ status: 404, body: { message } });
const unprocessable = (message: string): RestResult => ({ status: 422, body: { message } });

// ---------------------------------------------------------------- shapes

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: viewerUser(),
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.updatedAt,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    archived: false,
    disabled: false,
    visibility: repo.isPrivate ? 'private' : 'public',
    fork: false,
    forks_count: 0,
    stargazers_count: 0,
    watchers_count: 0,
    open_issues_count: repo.issues.filter((i) => i.state === 'open').length,
    license: null,
    topics: [],
  };
}

function labelJson(repo: Repo, name: string): Record<string, unknown> {
  const label = repo.labels.get(name);
  return { id: label?.id ?? 0, node_id: globalId('Label', label?.id ?? 0), name, color: label?.color ?? 'ededed', description: '', default: false };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', issue.id),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(repo, name)),
    user: viewerUser(),
    assignee: null,
    assignees: [],
    locked: false,
    comments: repo.comments.get(issue.number)?.length ?? 0,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.state === 'closed' ? issue.updatedAt : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function refJson(repo: Repo, ref: string, sha: string): Record<string, unknown> {
  return {
    label: `${repo.owner}:${ref}`,
    ref,
    sha,
    user: viewerUser(),
    repo: repoJson(repo),
  };
}

export function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', pull.id),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    mergeable: true,
    head: refJson(repo, pull.headRef, pull.headSha),
    base: refJson(repo, pull.baseRef, pull.baseSha),
    user: viewerUser(),
    assignee: null,
    assignees: [],
    locked: false,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.state === 'closed' ? pull.updatedAt : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function commitJson(repo: Repo, sha: string): Record<string, unknown> | null {
  const obj = getObject(repo, sha);
  if (obj === undefined || obj.type !== 'commit') return null;
  const c = parseCommit(obj.data);
  const authorOf = (line: string): { name: string; email: string; date: string } => {
    const m = /^(.*) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(line);
    if (m === null) return { name: VIEWER_NAME, email: VIEWER_EMAIL, date: repo.createdAt };
    return { name: m[1] as string, email: m[2] as string, date: new Date(Number(m[3]) * 1000).toISOString() };
  };
  return {
    sha,
    node_id: globalId('Commit', 0) + sha.slice(0, 8),
    commit: {
      // GitHub presents the message without the trailing newline that the
      // stored commit object carries.
      message: c.message.replace(/\r?\n$/, ''),
      author: authorOf(c.author),
      committer: authorOf(c.committer),
      tree: { sha: c.tree },
    },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID },
    committer: { login: VIEWER_LOGIN, id: VIEWER_ID },
    parents: c.parents.map((p) => ({ sha: p })),
  };
}

// ---------------------------------------------------------------- helpers

function parsePage(query: URLSearchParams): { page: number; perPage: number } {
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const perPage = Math.min(100, Math.max(1, Number(query.get('per_page') ?? '30') || 30));
  return { page, perPage };
}

function pageOf<T>(items: T[], page: number, perPage: number): T[] {
  return items.slice((page - 1) * perPage, page * perPage);
}

function linkHeader(origin: string, path: string, query: URLSearchParams, page: number, perPage: number, hasNext: boolean): Record<string, string> {
  if (!hasNext) return {};
  const q = new URLSearchParams(query);
  q.set('page', String(page + 1));
  q.set('per_page', String(perPage));
  return { link: `<${origin}${path}?${q.toString()}>; rel="next"` };
}

function getRepoOr404(store: Store, owner: string, name: string): Repo | RestResult {
  const repo = store.viewerRepo(owner, name);
  if (repo === undefined) return notFound();
  return repo;
}

function getIssueOr404(repo: Repo, n: number): Issue | RestResult {
  const issue = repo.issues.find((i) => i.number === n);
  if (issue === undefined) return notFound('Issue not found');
  return issue;
}

function getPullOr404(repo: Repo, n: number): Pull | RestResult {
  const pull = repo.pulls.find((p) => p.number === n);
  if (pull === undefined) return notFound('Pull request not found');
  return pull;
}

/** Line counts for a changed file: multiset difference of lines. */
function lineCounts(oldContent: Buffer | null, newContent: Buffer | null): { additions: number; deletions: number } {
  const linesOf = (b: Buffer | null): string[] => (b === null ? [] : b.toString('utf8').split('\n'));
  const count = (lines: string[]): Map<string, number> => {
    const m = new Map<string, number>();
    for (const l of lines) m.set(l, (m.get(l) ?? 0) + 1);
    return m;
  };
  const oldLines = count(linesOf(oldContent));
  const newLines = count(linesOf(newContent));
  let additions = 0;
  let deletions = 0;
  for (const [line, n] of newLines) additions += Math.max(0, n - (oldLines.get(line) ?? 0));
  for (const [line, n] of oldLines) deletions += Math.max(0, n - (newLines.get(line) ?? 0));
  return { additions, deletions };
}

function blobContent(repo: Repo, oid: string | null): Buffer | null {
  if (oid === null) return null;
  const obj = getObject(repo, oid);
  if (obj === undefined || obj.type !== 'blob') return null;
  return obj.data;
}

// ---------------------------------------------------------------- the router

export function handleRest(
  store: Store,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: Record<string, unknown> | null,
  origin: string,
): RestResult {
  const path = `/api/v3/${segments.join('/')}`;
  const b = body ?? {};

  // GET /api/v3/ — the API root.
  if (segments.length === 0) {
    if (method !== 'GET') return notFound();
    return ok(
      { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' },
      200,
      { 'x-oauth-scopes': 'repo, read:org' },
    );
  }

  // GET /api/v3/meta
  if (segments.length === 1 && segments[0] === 'meta') {
    if (method !== 'GET') return notFound();
    return ok({ installed_version: '1.0.0' });
  }

  // GET /api/v3/user
  if (segments.length === 1 && segments[0] === 'user') {
    if (method !== 'GET') return notFound();
    return ok(viewerUser());
  }

  // /api/v3/user/repos
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'GET') {
      const { page, perPage } = parsePage(query);
      const all = store.viewerRepos();
      const items = pageOf(all, page, perPage);
      const hasNext = page * perPage < all.length;
      return ok(
        items.map((r) => repoJson(r)),
        200,
        linkHeader(origin, path, query, page, perPage, hasNext),
      );
    }
    if (method === 'POST') {
      const name = typeof b.name === 'string' ? b.name.trim() : '';
      if (name === '') return unprocessable('name is required');
      if (store.getRepo(VIEWER_LOGIN, name) !== undefined) {
        return unprocessable('Repository already exists');
      }
      const repo = store.createRepo(name, b.private === true, typeof b.description === 'string' ? b.description : '');
      return ok(repoJson(repo), 201);
    }
    return notFound();
  }

  // /api/v3/repos/{owner}/{repo}...
  if (segments.length >= 3 && segments[0] === 'repos') {
    const owner = decodeURIComponent(segments[1] as string);
    const name = decodeURIComponent(segments[2] as string);
    const rest = segments.slice(3);
    const found = getRepoOr404(store, owner, name);
    if ('status' in found) return found;
    const repo = found;

    if (rest.length === 0) {
      if (method === 'GET') return ok(repoJson(repo));
      if (method === 'PATCH') {
        if (typeof b.description === 'string') repo.description = b.description;
        if (typeof b.name === 'string' && b.name.trim() !== '' && b.name !== repo.name) {
          store.deleteRepo(owner, repo.name);
          repo.name = b.name.trim();
          store.repos.set(store.key(owner, repo.name), repo);
        }
        repo.updatedAt = store.nowIso();
        return ok(repoJson(repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, name);
        return { status: 204, body: null };
      }
      return notFound();
    }

    // POST /git/refs
    if (rest.length === 2 && rest[0] === 'git' && rest[1] === 'refs') {
      if (method !== 'POST') return notFound();
      const ref = typeof b.ref === 'string' ? b.ref : '';
      const sha = typeof b.sha === 'string' ? b.sha.toLowerCase() : '';
      if (!ref.startsWith('refs/') || !isOid(sha)) return unprocessable('Invalid ref or sha');
      repo.refs.set(ref, sha);
      return ok({ ref, node_id: globalId('Ref', 0) + ref.replace(/\W/g, ''), object: { sha, type: 'commit' }, url: '' }, 201);
    }

    // GET /branches
    if (rest.length === 1 && rest[0] === 'branches') {
      if (method !== 'GET') return notFound();
      const branches: unknown[] = [];
      for (const [ref, sha] of [...repo.refs.entries()].sort()) {
        if (!ref.startsWith('refs/heads/')) continue;
        branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha }, protected: false });
      }
      return ok(branches);
    }

    // GET /commits
    if (rest.length === 1 && rest[0] === 'commits') {
      if (method !== 'GET') return notFound();
      const shaParam = query.get('sha');
      const tip = shaParam !== null ? repo.refs.get(`refs/heads/${shaParam}`) ?? (isOid(shaParam) ? shaParam : undefined) : undefined;
      const start = tip ?? repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      const seen = new Set<string>();
      const stack: string[] = start !== undefined ? [start] : [];
      const commits: string[] = [];
      while (stack.length > 0) {
        const cur = stack.pop() as string;
        if (seen.has(cur)) continue;
        seen.add(cur);
        const obj = getObject(repo, cur);
        if (obj === undefined || obj.type !== 'commit') continue;
        commits.push(cur);
        for (const p of parseCommit(obj.data).parents) stack.push(p);
      }
      commits.sort((a, c) => (repo.commitSeq.get(c) ?? 0) - (repo.commitSeq.get(a) ?? 0));
      const { page, perPage } = parsePage(query);
      return ok(pageOf(commits, page, perPage).map((s) => commitJson(repo, s)).filter((c) => c !== null));
    }

    // /contents/{path...}
    if (rest.length >= 1 && rest[0] === 'contents') {
      return handleContents(store, repo, method, rest.slice(1).map((s) => decodeURIComponent(s)).join('/'), query, b);
    }

    // POST /labels
    if (rest.length === 1 && rest[0] === 'labels') {
      if (method !== 'POST') return notFound();
      const labelName = typeof b.name === 'string' ? b.name.trim() : '';
      if (labelName === '') return unprocessable('name is required');
      const color = typeof b.color === 'string' ? b.color : 'ededed';
      if (!repo.labels.has(labelName)) {
        repo.labels.set(labelName, { id: repo.nextLabelId++, name: labelName, color });
      }
      return ok(labelJson(repo, labelName), 201);
    }

    // /issues...
    if (rest.length >= 1 && rest[0] === 'issues') {
      return handleIssues(store, repo, method, rest.slice(1), query, b);
    }

    // /pulls...
    if (rest.length >= 1 && rest[0] === 'pulls') {
      return handlePulls(store, repo, method, rest.slice(1), query, b);
    }

    return notFound();
  }

  return notFound();
}

// ---------------------------------------------------------------- contents

function handleContents(
  store: Store,
  repo: Repo,
  method: string,
  path: string,
  query: URLSearchParams,
  b: Record<string, unknown>,
): RestResult {
  const refName = query.get('ref') ?? repo.defaultBranch;
  const tip = repo.refs.get(`refs/heads/${refName}`);
  const treeOfTip = (oid: string | undefined): string | null => {
    if (oid === undefined) return null;
    const obj = getObject(repo, oid);
    if (obj === undefined || obj.type !== 'commit') return null;
    return parseCommit(obj.data).tree;
  };

  if (method === 'GET') {
    const tree = treeOfTip(tip);
    if (tree === null) return notFound('Not Found');
    const entry = treeLookup(repo, tree, path);
    if (entry === null) {
      // Maybe a directory: list its entries.
      const dirEntries = listDir(repo, tree, path);
      if (dirEntries === null) return notFound('Not Found');
      return ok(dirEntries);
    }
    if (entry.mode === '40000') {
      const dirEntries = listDir(repo, tree, path);
      if (dirEntries === null) return notFound('Not Found');
      return ok(dirEntries);
    }
    const blob = getObject(repo, entry.oid);
    if (blob === undefined) return notFound('Not Found');
    return ok({
      type: 'file',
      encoding: 'base64',
      size: blob.data.length,
      name: path.split('/').pop() ?? path,
      path,
      content: blob.data.toString('base64'),
      sha: entry.oid,
    });
  }

  if (method === 'PUT') {
    const message = typeof b.message === 'string' ? b.message : 'Update file';
    const contentB64 = typeof b.content === 'string' ? b.content : '';
    let content: Buffer;
    try {
      content = Buffer.from(contentB64.replace(/\s/g, ''), 'base64');
    } catch {
      return unprocessable('Invalid content encoding');
    }
    const branch = typeof b.branch === 'string' && b.branch !== '' ? b.branch : repo.defaultBranch;
    const branchRef = `refs/heads/${branch}`;
    let branchTip = repo.refs.get(branchRef);
    if (branchTip === undefined) {
      // A new branch starts at the default branch tip.
      branchTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (branchTip !== undefined) repo.refs.set(branchRef, branchTip);
    }
    const curTree = treeOfTip(branchTip);
    const curEntry = curTree === null ? null : treeLookup(repo, curTree, path);
    const sha = typeof b.sha === 'string' ? b.sha.toLowerCase() : undefined;
    if (curEntry !== null) {
      // Update: a sha is required and must match the current blob.
      if (sha !== undefined && sha !== curEntry.oid) {
        return { status: 409, body: { message: 'sha does not match the current blob' } };
      }
      const { commitOid, blobOid } = commitOnBranch(store, repo, branch, path, content, message);
      return ok({ content: { sha: blobOid, encoding: 'base64', size: content.length, name: path.split('/').pop() ?? path, path }, commit: { sha: commitOid } }, 200);
    }
    const { commitOid, blobOid } = commitOnBranch(store, repo, branch, path, content, message);
    return ok({ content: { sha: blobOid, encoding: 'base64', size: content.length, name: path.split('/').pop() ?? path, path }, commit: { sha: commitOid } }, 201);
  }

  if (method === 'DELETE') {
    const branch = typeof b.branch === 'string' && b.branch !== '' ? b.branch : repo.defaultBranch;
    const branchTip = repo.refs.get(`refs/heads/${branch}`);
    const curTree = treeOfTip(branchTip);
    const curEntry = curTree === null ? null : treeLookup(repo, curTree, path);
    if (curEntry === null) return notFound('Not Found');
    const sha = typeof b.sha === 'string' ? b.sha.toLowerCase() : undefined;
    if (sha !== undefined && sha !== curEntry.oid) {
      return { status: 409, body: { message: 'sha does not match the current blob' } };
    }
    const message = typeof b.message === 'string' ? b.message : 'Delete file';
    const { commitOid } = commitOnBranch(store, repo, branch, path, null, message);
    return ok({ commit: { sha: commitOid } });
  }

  return notFound();
}

/** List a directory's entries at `path` ('' = root), or null when it is not a directory. */
function listDir(repo: Repo, treeOid: string, path: string): unknown[] | null {
  const parts = path === '' ? [] : path.split('/').filter((p) => p !== '');
  let cur = treeOid;
  for (const part of parts) {
    const obj = getObject(repo, cur);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === part);
    if (entry === undefined || entry.mode !== '40000') return null;
    cur = entry.oid;
  }
  const obj = getObject(repo, cur);
  if (obj === undefined || obj.type !== 'tree') return null;
  return parseTree(obj.data).map((e) => ({
    type: e.mode === '40000' ? 'dir' : 'file',
    name: e.name,
    path: path === '' ? e.name : `${path}/${e.name}`,
    sha: e.oid,
    size: e.mode === '40000' ? 0 : (getObject(repo, e.oid)?.data.length ?? 0),
  }));
}

// ---------------------------------------------------------------- issues

function handleIssues(
  store: Store,
  repo: Repo,
  method: string,
  rest: string[],
  query: URLSearchParams,
  b: Record<string, unknown>,
): RestResult {
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') ?? 'desc';
      let items = repo.issues.slice();
      if (state !== 'all') items = items.filter((i) => i.state === state);
      if (sort === 'created') {
        items.sort((a, c) => (direction === 'asc' ? a.seq - c.seq : c.seq - a.seq));
      }
      const { page, perPage } = parsePage(query);
      return ok(pageOf(items, page, perPage).map((i) => issueJson(repo, i)));
    }
    if (method === 'POST') {
      const title = typeof b.title === 'string' ? b.title : '';
      const bodyText = typeof b.body === 'string' ? b.body : '';
      const labels = Array.isArray(b.labels) ? (b.labels as unknown[]).map(String) : [];
      for (const label of labels) {
        if (!repo.labels.has(label)) return unprocessable(`Label "${label}" does not exist`);
      }
      const now = store.nowIso();
      const issue: Issue = {
        id: store.nextIssueId++,
        number: repo.nextIssue++,
        title,
        body: bodyText,
        state: 'open',
        labels,
        seq: repo.issues.length + 1,
        createdAt: now,
        updatedAt: now,
      };
      repo.issues.push(issue);
      return ok(issueJson(repo, issue), 201);
    }
    return notFound();
  }

  const n = Number(rest[0]);
  if (!Number.isInteger(n) || n < 1) return notFound();
  if (rest.length === 1) {
    const found = getIssueOr404(repo, n);
    if ('status' in found) return found;
    if (method === 'GET') return ok(issueJson(repo, found));
    if (method === 'PATCH') {
      if (typeof b.state === 'string' && (b.state === 'open' || b.state === 'closed')) found.state = b.state;
      if (typeof b.title === 'string') found.title = b.title;
      if (typeof b.body === 'string') found.body = b.body;
      found.updatedAt = store.nowIso();
      return ok(issueJson(repo, found));
    }
    return notFound();
  }

  if (rest.length === 2 && rest[1] === 'comments') {
    const found = getIssueOr404(repo, n);
    if ('status' in found) return found;
    if (method === 'GET') {
      const list = repo.comments.get(n) ?? [];
      return ok(list.map((c) => ({
        id: c.id,
        node_id: globalId('IssueComment', c.id),
        body: c.body,
        user: viewerUser(),
        created_at: c.createdAt,
        updated_at: c.updatedAt,
      })));
    }
    if (method === 'POST') {
      const bodyText = typeof b.body === 'string' ? b.body : '';
      const now = store.nowIso();
      const comment = { id: store.nextCommentId++, body: bodyText, seq: (repo.comments.get(n)?.length ?? 0) + 1, createdAt: now, updatedAt: now };
      const list = repo.comments.get(n) ?? [];
      list.push(comment);
      repo.comments.set(n, list);
      return ok({
        id: comment.id,
        node_id: globalId('IssueComment', comment.id),
        body: comment.body,
        user: viewerUser(),
        created_at: comment.createdAt,
        updated_at: comment.updatedAt,
      }, 201);
    }
    return notFound();
  }

  return notFound();
}

// ---------------------------------------------------------------- pulls

function handlePulls(
  store: Store,
  repo: Repo,
  method: string,
  rest: string[],
  query: URLSearchParams,
  b: Record<string, unknown>,
): RestResult {
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let items = repo.pulls.slice();
      if (state !== 'all') items = items.filter((p) => p.state === state);
      items.sort((a, c) => a.number - c.number);
      const { page, perPage } = parsePage(query);
      return ok(pageOf(items, page, perPage).map((p) => pullJson(repo, p)));
    }
    if (method === 'POST') {
      const title = typeof b.title === 'string' ? b.title : '';
      const bodyText = typeof b.body === 'string' ? b.body : '';
      const head = typeof b.head === 'string' ? b.head : '';
      const base = typeof b.base === 'string' ? b.base : repo.defaultBranch;
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined) return unprocessable(`Head branch "${head}" does not exist`);
      if (baseSha === undefined) return unprocessable(`Base branch "${base}" does not exist`);
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
      return ok(pullJson(repo, pull), 201);
    }
    return notFound();
  }

  const n = Number(rest[0]);
  if (!Number.isInteger(n) || n < 1) return notFound();
  const found = getPullOr404(repo, n);
  if ('status' in found) return found;
  const pull = found;

  if (rest.length === 1) {
    if (method === 'GET') return ok(pullJson(repo, pull));
    return notFound();
  }

  if (rest.length === 2 && rest[1] === 'files') {
    if (method !== 'GET') return notFound();
    const headObj = getObject(repo, pull.headSha);
    const base = mergeBase(repo, pull.headSha, pull.baseSha);
    const baseTree = base === null ? null : parseCommit(getObject(repo, base)?.data ?? Buffer.alloc(0)).tree;
    const headTree = headObj !== undefined && headObj.type === 'commit' ? parseCommit(headObj.data).tree : null;
    const diffs = diffTrees(repo, baseTree, headTree);
    const files = diffs
      .filter((d) => d.status !== 'removed')
      .map((d) => {
        const { additions, deletions } = lineCounts(blobContent(repo, d.oldOid), blobContent(repo, d.newOid));
        return {
          filename: d.path,
          status: d.status === 'added' ? 'added' : 'modified',
          additions,
          deletions,
          changes: additions + deletions,
          sha: d.newOid ?? '',
        };
      });
    return ok(files);
  }

  if (rest.length === 2 && rest[1] === 'merge') {
    if (method !== 'PUT') return notFound();
    if (pull.merged || pull.state === 'closed') {
      return { status: 405, body: { message: 'Pull request is already merged or closed' } };
    }
    const merged = doMergePull(store, repo, pull);
    if (!merged) {
      return { status: 405, body: { message: 'Merge conflict: the pull request cannot be merged' } };
    }
    return ok({ sha: pull.mergeSha, merged: true, message: 'Pull request successfully merged' });
  }

  return notFound();
}

/**
 * Squash-merge a pull request: one commit on the base branch whose tree is the head tip's
 * tree. Returns false when the pull cannot merge (missing head/base, or a file changed on
 * both sides of the merge-base to different content). Shared by REST and GraphQL.
 */
export function doMergePull(store: Store, repo: Repo, pull: Pull): boolean {
  if (pull.merged || pull.state === 'closed') return false;
  // The base is the CURRENT tip of the base branch, not the sha recorded when the
  // pull was created: the branch may have advanced since (e.g. another PR merged
  // first), and a conflict is judged against that current tip.
  const currentBaseSha = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
  const headObj = getObject(repo, pull.headSha);
  const baseObj = getObject(repo, currentBaseSha);
  if (headObj === undefined || baseObj === undefined) return false;
  const headTree = parseCommit(headObj.data).tree;
  const baseTree = parseCommit(baseObj.data).tree;
  const base = mergeBase(repo, pull.headSha, currentBaseSha);
  const baseOfMerge = base === null ? null : parseCommit(getObject(repo, base)?.data ?? Buffer.alloc(0)).tree;
  // Conflict: a file changed on both sides of the merge-base to different content.
  const baseChanges = diffTrees(repo, baseOfMerge, baseTree);
  const headChanges = diffTrees(repo, baseOfMerge, headTree);
  const headMap = new Map(headChanges.map((d) => [d.path, d.newOid]));
  for (const bc of baseChanges) {
    if (!headMap.has(bc.path)) continue;
    const baseOid = treeLookup(repo, baseTree, bc.path)?.oid ?? null;
    const headOid = treeLookup(repo, headTree, bc.path)?.oid ?? null;
    if (baseOid !== headOid) return false;
  }
  const { line } = identLine(store);
  const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}\n\n${pull.title}\n`;
  const commitData = buildCommit(headTree, [currentBaseSha], line, message);
  const mergeSha = storeObject(repo, 'commit', commitData);
  if (!repo.commitSeq.has(mergeSha)) repo.commitSeq.set(mergeSha, repo.commitSeq.size + 1);
  repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = store.nowIso();
  pull.mergeSha = mergeSha;
  pull.updatedAt = pull.mergedAt;
  return true;
}


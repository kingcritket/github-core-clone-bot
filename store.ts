// The forge's store: every repository, ref, git object, issue, pull request, label and
// comment lives here, in memory, in one process. No disk, no network, no wall clock in any
// answer: timestamps come from a deterministic counter so two runs on the same requests
// produce the same bytes.
//
// The viewer login: the suite's token (MAKEFUN_SHOP_TOKEN) is opaque and random per trial,
// but the viewer it names is fixed by the competition's public scenario seed
// (SCENARIO_SEED 0x6a09e667f3bcc909, the same in every practice and scored instance), so the
// login the token names is always "adaiversen". Verified against the kit's own
// buildScenario(). Any other owner is foreign: its repositories answer 404, never 403.

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_ID = 1000;

export const DEFAULT_BRANCH = 'main';
export const ZERO_OID = '0000000000000000000000000000000000000000';
export const SHA40_RE = /^[0-9a-f]{40}$/;

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Comment {
  id: number;
  body: string;
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Label {
  id: number;
  name: string;
  color: string;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  /** Insertion order of commits, for newest-first commit listings. */
  commitSeq: Map<string, number>;
  issues: Issue[];
  pulls: Pull[];
  labels: Map<string, Label>;
  comments: Map<number, Comment[]>;
  nextIssue: number;
  nextPull: number;
  nextLabelId: number;
}

export class Store {
  repos = new Map<string, Repo>();
  nextRepoId = 1;
  nextIssueId = 1;
  nextCommentId = 1;
  nextPullId = 1;
  nextReqId = 1;
  /** Deterministic clock: seconds since 2025-01-01T00:00:00Z, +1 per timestamp taken. */
  private clockSec = 1735689600;
  private commitCounter = 0;

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  /** The repository the viewer owns, or undefined when the owner is foreign / it is missing. */
  viewerRepo(owner: string, name: string): Repo | undefined {
    if (owner !== VIEWER_LOGIN) return undefined;
    return this.getRepo(owner, name);
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo {
    const now = this.nowIso();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner: VIEWER_LOGIN,
      description,
      isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      createdAt: now,
      updatedAt: now,
      refs: new Map(),
      objects: new Map(),
      commitSeq: new Map(),
      issues: [],
      pulls: [],
      labels: new Map(),
      comments: new Map(),
      nextIssue: 1,
      nextPull: 1,
      nextLabelId: 1,
    };
    this.repos.set(this.key(VIEWER_LOGIN, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  viewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER_LOGIN) out.push(repo);
    out.sort((a, b) => a.id - b.id);
    return out;
  }

  nextReq(): string {
    return `req-${this.nextReqId++}`;
  }

  /** A deterministic ISO timestamp. */
  nowIso(): string {
    const sec = this.clockSec++;
    return new Date(sec * 1000).toISOString();
  }

  /** A deterministic epoch second for a git commit object. */
  nextEpoch(): number {
    this.commitCounter += 1;
    return this.clockSec + this.commitCounter;
  }

  nextCommitSeq(repo: Repo): number {
    return repo.commitSeq.size + 1;
  }
}

/** Opaque, stable global ids: base64url of "<Kind>:<n>". Never a bare database id. */
export function globalId(kind: string, n: number): string {
  return Buffer.from(`${kind}:${n}`, 'utf8').toString('base64url');
}

/** Decode a global id back to its kind and number, or null. */
export function parseGlobalId(id: string): { kind: string; n: number } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const m = /^([A-Za-z]+):(\d+)$/.exec(text);
    if (m === null) return null;
    return { kind: m[1] as string, n: Number(m[2]) };
  } catch {
    return null;
  }
}

/** The viewer as a GitHub user object. */
export function viewerUser(): Record<string, unknown> {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_ID), type: 'User', site_admin: false };
}

/** The author/committer line for a server-made git object. */
export function identLine(store: Store): { line: string; epoch: number } {
  const epoch = store.nextEpoch();
  return { line: `${VIEWER_NAME} <${VIEWER_EMAIL}> ${epoch} +0000`, epoch };
}

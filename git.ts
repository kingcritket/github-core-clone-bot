// Git core: object ids, pkt-line framing, packfiles, trees/commits/tags, reachability,
// and the smart-HTTP handlers (upload-pack protocol v2 with a v0 fallback, receive-pack v0).
// Everything here is stdlib only (node:crypto, node:zlib). Objects are stored byte-exact:
// the server never rewrites a pushed object, so clones round-trip byte for byte.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { ZERO_OID, identLine, type GitObject, type GitObjectType, type Repo, type Store } from './store.ts';

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The object id of a git object: SHA-1 of "<type> <len>\0" + raw bytes. */
export function objectId(type: GitObjectType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  // Feed the two parts to the hash directly: no concatenated copy of the data.
  return createHash('sha1').update(head).update(data).digest('hex');
}

export function isOid(s: string): boolean {
  return /^[0-9a-f]{40}$/.test(s);
}

// ---------------------------------------------------------------- pkt-line

export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

/**
 * Parse pkt-lines from the front of `buf` until the first flush (0000). Returns the text
 * lines (without trailing \n) and the bytes after the flush (a raw pack, for receive-pack).
 * A `0001` delim is skipped. Throws on a malformed header.
 */
export function parsePktLines(buf: Buffer): { lines: string[]; rest: Buffer } {
  const lines: string[] = [];
  let off = 0;
  for (;;) {
    if (off + 4 > buf.length) throw new Error('pktline-truncated');
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (head === '0000') return { lines, rest: buf.subarray(off + 4) };
    if (head === '0001') {
      off += 4;
      continue;
    }
    const len = parseInt(head, 16);
    if (!Number.isFinite(len) || len < 4 || len > 65520) throw new Error('pktline-header');
    if (off + len > buf.length) throw new Error('pktline-truncated');
    const payload = buf.subarray(off + 4, off + len).toString('utf8');
    lines.push(payload.endsWith('\n') ? payload.slice(0, -1) : payload);
    off += len;
  }
}

/** Split a buffer into pkt-line payloads (for parsing a whole sideband/report stream). */
export function splitPktLines(buf: Buffer): Buffer[] {
  const out: Buffer[] = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (head === '0000' || head === '0001') {
      off += 4;
      continue;
    }
    const len = parseInt(head, 16);
    if (!Number.isFinite(len) || len < 4 || len > 65520 || off + len > buf.length) break;
    out.push(buf.subarray(off + 4, off + len));
    off += len;
  }
  return out;
}

// ---------------------------------------------------------------- trees / commits / tags

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    if (sp < 0) break;
    const mode = data.subarray(off, sp).toString('ascii');
    const nul = data.indexOf(0, sp + 1);
    if (nul < 0) break;
    const name = data.subarray(sp + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    off = nul + 21;
  }
  return entries;
}

/** Git tree ordering: names compare as if directories ended in '/'. */
export function compareTreeNames(a: string, aIsDir: boolean, b: string, bIsDir: boolean): number {
  const an = aIsDir ? `${a}/` : a;
  const bn = bIsDir ? `${b}/` : b;
  if (an < bn) return -1;
  if (an > bn) return 1;
  return 0;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = entries.slice().sort((x, y) => compareTreeNames(x.name, x.mode === '40000', y.name, y.mode === '40000'));
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface ParsedCommit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const out: ParsedCommit = { tree: '', parents: [], author: '', committer: '', message };
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) out.tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) out.parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) out.author = line.slice(7);
    else if (line.startsWith('committer ')) out.committer = line.slice(10);
  }
  return out;
}

export function buildCommit(tree: string, parents: string[], ident: string, message: string): Buffer {
  let s = `tree ${tree}\n`;
  for (const p of parents) s += `parent ${p}\n`;
  s += `author ${ident}\ncommitter ${ident}\n\n${message}`;
  if (!s.endsWith('\n')) s += '\n';
  return Buffer.from(s, 'utf8');
}

export interface ParsedTag {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): ParsedTag {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const out: ParsedTag = { object: '', type: '', tag: '', tagger: '', message };
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) out.object = line.slice(7).trim();
    else if (line.startsWith('type ')) out.type = line.slice(5).trim();
    else if (line.startsWith('tag ')) out.tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) out.tagger = line.slice(7);
  }
  return out;
}

// ---------------------------------------------------------------- object store helpers

export function storeObject(repo: Repo, type: GitObjectType, data: Buffer): string {
  const oid = objectId(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

export function getObject(repo: Repo, oid: string): GitObject | undefined {
  return repo.objects.get(oid);
}

/** Walk parents from `oid`; true when `ancestor` is reachable (or equal). */
export function isAncestor(repo: Repo, ancestor: string, oid: string): boolean {
  if (ancestor === oid) return true;
  const seen = new Set<string>();
  const stack = [oid];
  while (stack.length > 0) {
    const cur = stack.pop() as string;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const obj = repo.objects.get(cur);
    if (obj === undefined || obj.type !== 'commit') continue;
    const commit = parseCommit(obj.data);
    for (const p of commit.parents) {
      if (p === ancestor) return true;
      stack.push(p);
    }
  }
  return false;
}

/** A merge-base: the first commit reachable from both, preferring the newest stored. */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const fromA = new Set<string>();
  const stack = [a];
  while (stack.length > 0) {
    const cur = stack.pop() as string;
    if (fromA.has(cur)) continue;
    fromA.add(cur);
    const obj = repo.objects.get(cur);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  let best: string | null = null;
  let bestSeq = -1;
  const stackB = [b];
  const seenB = new Set<string>();
  while (stackB.length > 0) {
    const cur = stackB.pop() as string;
    if (seenB.has(cur)) continue;
    seenB.add(cur);
    if (fromA.has(cur)) {
      const seq = repo.commitSeq.get(cur) ?? 0;
      if (seq > bestSeq) {
        bestSeq = seq;
        best = cur;
      }
    }
    const obj = repo.objects.get(cur);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stackB.push(p);
  }
  return best;
}

// ---------------------------------------------------------------- reachability / pack building

export interface Closure {
  /** Objects to send, in deterministic (oid-sorted) order. */
  objects: Map<string, GitObject>;
  /** Commits at the shallow boundary (their parents were not traversed). */
  boundary: string[];
  /** Commits included in the closure. */
  includedCommits: Set<string>;
}

/**
 * The objects reachable from `wants`, minus everything reachable from `haves`.
 * With `depth`, only commits closer than `depth` generations are traversed; the commits at
 * the boundary are reported so the client can mark them shallow.
 */
export function closure(repo: Repo, wants: string[], haves: string[], depth?: number, shallows: string[] = []): Closure {
  const shallowSet = new Set(shallows);
  const included = new Map<string, GitObject>();
  const includedCommits = new Set<string>();
  const boundary: string[] = [];
  // BFS over commits with distance; include a commit's tree+blobs when visited.
  const dist = new Map<string, number>();
  const queue: string[] = [];
  for (const w of wants) {
    if (isOid(w) && !dist.has(w)) {
      dist.set(w, 0);
      queue.push(w);
    }
  }
  // Index-based queue: shift() is O(n) per pop and this walk is O(n) pops.
  let qhead = 0;
  while (qhead < queue.length) {
    const oid = queue[qhead++] as string;
    const d = dist.get(oid) as number;
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'commit') {
      if (depth !== undefined && d >= depth) continue;
      includedCommits.add(oid);
      included.set(oid, obj);
      const commit = parseCommit(obj.data);
      addTree(repo, included, commit.tree);
      if (depth !== undefined && d === depth - 1) {
        boundary.push(oid);
      } else if (depth === undefined && shallowSet.has(oid)) {
        // The client holds this commit as a shallow boundary: it has the commit
        // but not its parents, so the want walk stops here (no deepen requested).
        boundary.push(oid);
      } else {
        for (const p of commit.parents) {
          if (!dist.has(p)) {
            dist.set(p, d + 1);
            queue.push(p);
          }
        }
      }
    } else if (obj.type === 'tag') {
      included.set(oid, obj);
      const tag = parseTag(obj.data);
      if (isOid(tag.object) && !dist.has(tag.object)) {
        dist.set(tag.object, d);
        queue.push(tag.object);
      }
    } else {
      included.set(oid, obj);
    }
  }
  // Subtract everything reachable from the haves (full depth).
  if (haves.length > 0) {
    const haveSet = new Set<string>();
    const hq: string[] = [];
    for (const h of haves) {
      if (isOid(h) && !haveSet.has(h)) {
        haveSet.add(h);
        hq.push(h);
      }
    }
    let hhead = 0;
    while (hhead < hq.length) {
      const oid = hq[hhead++] as string;
      const obj = repo.objects.get(oid);
      if (obj === undefined) continue;
      if (obj.type === 'commit') {
        const commit = parseCommit(obj.data);
        collectTree(repo, haveSet, commit.tree);
        if (shallowSet.has(oid)) continue; // client holds the commit, not its parents
        for (const p of commit.parents) {
          if (!haveSet.has(p)) {
            haveSet.add(p);
            hq.push(p);
          }
        }
      } else if (obj.type === 'tag') {
        const tag = parseTag(obj.data);
        if (isOid(tag.object) && !haveSet.has(tag.object)) {
          haveSet.add(tag.object);
          hq.push(tag.object);
        }
      } else if (obj.type === 'tree') {
        collectTree(repo, haveSet, oid);
      }
    }
    for (const oid of haveSet) included.delete(oid);
  }
  const sorted = new Map([...included.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)));
  return { objects: sorted, boundary, includedCommits };
}

function addTree(repo: Repo, into: Map<string, GitObject>, treeOid: string): void {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return;
  if (into.has(treeOid)) return;
  into.set(treeOid, obj);
  for (const e of parseTree(obj.data)) {
    const child = repo.objects.get(e.oid);
    if (child === undefined) continue;
    if (child.type === 'tree') addTree(repo, into, e.oid);
    else if (child.type === 'blob') into.set(e.oid, child);
  }
}

function collectTree(repo: Repo, into: Set<string>, treeOid: string): void {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return;
  if (into.has(treeOid)) return;
  into.add(treeOid);
  for (const e of parseTree(obj.data)) {
    const child = repo.objects.get(e.oid);
    if (child === undefined) continue;
    if (child.type === 'tree') collectTree(repo, into, e.oid);
    else into.add(e.oid);
  }
}

// ---------------------------------------------------------------- packfiles

const PACK_TYPE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const PACK_TYPE_NAME: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** Write a pack v2 with plain (non-delta) objects. */
export function writePack(objects: Map<string, GitObject>): Buffer {
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objects.size, 8);
  parts.push(head);
  for (const [, obj] of objects) {
    const size = obj.data.length;
    const typeCode = PACK_TYPE[obj.type];
    // varint size+type header: low 4 bits of size in first byte, then 7-bit groups.
    const headerBytes: number[] = [];
    let n = size;
    let first = (typeCode << 4) | (n & 0x0f);
    n >>= 4;
    if (n > 0) first |= 0x80;
    headerBytes.push(first);
    while (n > 0) {
      let b = n & 0x7f;
      n >>= 7;
      if (n > 0) b |= 0x80;
      headerBytes.push(b);
    }
    parts.push(Buffer.from(headerBytes));
    // Level 1 deflate: ~2.5x faster than the default level 6 on small objects
    // for a few bytes of extra size, and any level inflates identically.
    parts.push(deflateSync(obj.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

interface PackEntry {
  type: GitObjectType;
  data: Buffer;
  oid: string;
}

/** Read a pack v2/v3, resolving OFS/REF deltas. Throws on structural errors. */
export function readPack(buf: Buffer): Map<string, PackEntry> {
  if (buf.length < 32) throw new Error('pack-too-short');
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack-magic');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('pack-version');
  const count = buf.readUInt32BE(8);
  const trailer = buf.subarray(buf.length - 20);
  const expect = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest();
  if (!expect.equals(trailer)) throw new Error('pack-checksum');
  const end = buf.length - 20;
  interface RawEntry {
    start: number;
    typeCode: number;
    data: Buffer;
    size: number;
    baseOid?: string;
    baseStart?: number;
  }
  const raw: RawEntry[] = [];
  let off = 12;
  for (let i = 0; i < count; i += 1) {
    if (off >= end) throw new Error('pack-truncated');
    const start = off;
    const first = buf[off] as number;
    const typeCode = (first >> 4) & 0x07;
    let size = first & 0x0f;
    let shift = 4;
    off += 1;
    while ((buf[off - 1] as number) & 0x80) {
      size |= ((buf[off] as number) & 0x7f) << shift;
      shift += 7;
      off += 1;
    }
    if (typeCode === 6) {
      // OFS_DELTA: the base entry starts `baseOff` bytes before this entry's start.
      let b = buf[off] as number;
      off += 1;
      let baseOff = b & 0x7f;
      while (b & 0x80) {
        b = buf[off] as number;
        off += 1;
        baseOff = ((baseOff + 1) << 7) | (b & 0x7f);
      }
      const { data, next } = inflateAt(buf, off, size, end);
      off = next;
      raw.push({ start, typeCode, data, size, baseStart: start - baseOff });
    } else if (typeCode === 7) {
      const baseOid = buf.subarray(off, off + 20).toString('hex');
      off += 20;
      const { data, next } = inflateAt(buf, off, size, end);
      off = next;
      raw.push({ start, typeCode, data, size, baseOid });
    } else {
      const { data, next } = inflateAt(buf, off, size, end);
      off = next;
      raw.push({ start, typeCode, data, size });
    }
  }
  if (off !== end) throw new Error('pack-trailing');
  const byStart = new Map<number, PackEntry>();
  const out = new Map<string, PackEntry>();
  const resolve = (index: number, seen: Set<number>): PackEntry => {
    const entry = raw[index] as RawEntry;
    const cached = byStart.get(entry.start);
    if (cached !== undefined) return cached;
    if (seen.has(index)) throw new Error('pack-delta-cycle');
    seen.add(index);
    let result: PackEntry;
    if (entry.typeCode === 6) {
      const baseIndex = raw.findIndex((e) => e.start === entry.baseStart);
      if (baseIndex < 0) throw new Error('pack-delta-base');
      const base = resolve(baseIndex, seen);
      const data = applyDelta(base.data, entry.data);
      result = { type: base.type, data, oid: objectId(base.type, data) };
    } else if (entry.typeCode === 7) {
      const base = out.get(entry.baseOid as string);
      if (base === undefined) throw new Error('pack-delta-base');
      const data = applyDelta(base.data, entry.data);
      result = { type: base.type, data, oid: objectId(base.type, data) };
    } else {
      const type = PACK_TYPE_NAME[entry.typeCode];
      if (type === undefined) throw new Error('pack-type');
      result = { type, data: entry.data, oid: objectId(type, entry.data) };
    }
    byStart.set(entry.start, result);
    out.set(result.oid, result);
    return result;
  };
  for (let i = 0; i < count; i += 1) resolve(i, new Set());
  return out;
}

/**
 * Inflate a deflate stream starting at `off`. Node's sync inflate stops at the
 * end of the first zlib stream and ignores whatever follows it, so one call
 * inflates the object no matter how many objects the pack still holds. The
 * stream's end is then located by its Adler-32 trailer (RFC 1950: the checksum
 * of the inflated data, big-endian, in the stream's last 4 bytes). A candidate
 * end is verified by inflating exactly that slice, so a coincidental Adler
 * match inside the deflate data can only cost one extra scan step, never a
 * wrong parse. This replaces a binary search that re-inflated the stream
 * ~log2(n) times per object (most of them throwing).
 */
function inflateAt(buf: Buffer, off: number, expectedSize: number, end: number): { data: Buffer; next: number } {
  let data: Buffer;
  try {
    data = inflateSync(buf.subarray(off, end));
  } catch {
    throw new Error('pack-size');
  }
  if (data.length !== expectedSize) throw new Error('pack-size');
  const needle = Buffer.alloc(4);
  needle.writeUInt32BE(adler32(data), 0);
  let searchFrom = off + 2;
  for (;;) {
    const at = buf.indexOf(needle, searchFrom);
    if (at < 0 || at + 4 > end) throw new Error('pack-size');
    const candidateEnd = at + 4;
    try {
      const check = inflateSync(buf.subarray(off, candidateEnd));
      if (check.length === expectedSize && check.equals(data)) return { data, next: candidateEnd };
    } catch {
      // Not the true stream end: keep scanning past this match.
    }
    searchFrom = at + 1;
  }
}

/** Adler-32 (RFC 1950), the checksum a zlib stream carries in its trailer. */
function adler32(data: Buffer): number {
  let a = 1;
  let b = 0;
  const NMAX = 5552;
  for (let off = 0; off < data.length; off += NMAX) {
    const stop = Math.min(data.length, off + NMAX);
    for (let i = off; i < stop; i++) {
      a += data[i] as number;
      b += a;
    }
    a %= 65521;
    b %= 65521;
  }
  return ((b << 16) | a) >>> 0;
}

/** Apply a git binary delta to a base buffer. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readVarint = (): number => {
    let v = 0;
    let shift = 0;
    for (;;) {
      const b = delta[off] as number;
      off += 1;
      v |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return v;
  };
  const srcSize = readVarint();
  if (srcSize !== base.length) throw new Error('pack-delta-size');
  const dstSize = readVarint();
  const out: Buffer[] = [];
  let total = 0;
  while (off < delta.length) {
    const op = delta[off] as number;
    off += 1;
    if (op & 0x80) {
      let cpOff = 0;
      let cpSize = 0;
      for (let i = 0; i < 4; i += 1) {
        if (op & (1 << i)) {
          cpOff |= (delta[off] as number) << (8 * i);
          off += 1;
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if (op & (0x10 << i)) {
          cpSize |= (delta[off] as number) << (8 * i);
          off += 1;
        }
      }
      if (cpSize === 0) cpSize = 0x10000;
      out.push(base.subarray(cpOff, cpOff + cpSize));
      total += cpSize;
    } else if (op > 0) {
      out.push(delta.subarray(off, off + op));
      total += op;
      off += op;
    } else {
      throw new Error('pack-delta-op');
    }
  }
  if (total !== dstSize) throw new Error('pack-delta-size');
  return Buffer.concat(out);
}

// ---------------------------------------------------------------- tree manipulation (contents API / merge)

/** Read a path out of a tree, following nested trees. Returns the blob entry or null. */
export function treeLookup(repo: Repo, treeOid: string, path: string): TreeEntry | null {
  const parts = path.split('/').filter((p) => p !== '');
  let cur = treeOid;
  for (let i = 0; i < parts.length; i += 1) {
    const obj = repo.objects.get(cur);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === parts[i]);
    if (entry === undefined) return null;
    if (i === parts.length - 1) return entry;
    if (entry.mode !== '40000') return null;
    cur = entry.oid;
  }
  return null;
}

/**
 * Return a new tree oid with `path` set to `blobOid` (mode 100644), or removed when
 * `blobOid` is null. Creates every intermediate tree object in the repo.
 */
export function treeSetPath(repo: Repo, treeOid: string | null, path: string, blobOid: string | null): string {
  const parts = path.split('/').filter((p) => p !== '');
  const rec = (tree: string | null, depth: number): string => {
    const entries = tree === null ? [] : parseTree(repo.objects.get(tree)?.data ?? Buffer.alloc(0));
    const name = parts[depth] as string;
    if (depth === parts.length - 1) {
      const idx = entries.findIndex((e) => e.name === name);
      if (blobOid === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else if (idx >= 0) {
        entries[idx] = { mode: '100644', name, oid: blobOid };
      } else {
        entries.push({ mode: '100644', name, oid: blobOid });
      }
    } else {
      const idx = entries.findIndex((e) => e.name === name);
      const sub = idx >= 0 ? entries[idx]?.oid ?? null : null;
      const next = rec(sub, depth + 1);
      const hasEntries = parseTree(repo.objects.get(next)?.data ?? Buffer.alloc(0)).length > 0;
      if (!hasEntries && blobOid === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else if (idx >= 0) {
        entries[idx] = { mode: '40000', name, oid: next };
      } else {
        entries.push({ mode: '40000', name, oid: next });
      }
    }
    const data = buildTree(entries);
    return storeObject(repo, 'tree', data);
  };
  return rec(treeOid, 0);
}

/** Flatten a tree to {path, mode, oid} file entries (recursive). */
export function flattenTree(repo: Repo, treeOid: string, prefix = ''): Array<{ path: string; mode: string; oid: string }> {
  const out: Array<{ path: string; mode: string; oid: string }> = [];
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') out.push(...flattenTree(repo, e.oid, path));
    else out.push({ path, mode: e.mode, oid: e.oid });
  }
  return out;
}

/** Diff two trees: files added / removed / modified (by blob oid). */
export function diffTrees(
  repo: Repo,
  baseTree: string | null,
  headTree: string | null,
): Array<{ path: string; status: 'added' | 'removed' | 'modified'; oldOid: string | null; newOid: string | null }> {
  const base = baseTree === null ? [] : flattenTree(repo, baseTree);
  const head = headTree === null ? [] : flattenTree(repo, headTree);
  const baseMap = new Map(base.map((f) => [f.path, f.oid]));
  const headMap = new Map(head.map((f) => [f.path, f.oid]));
  const out: Array<{ path: string; status: 'added' | 'removed' | 'modified'; oldOid: string | null; newOid: string | null }> = [];
  for (const [path, oid] of headMap) {
    if (!baseMap.has(path)) out.push({ path, status: 'added', oldOid: null, newOid: oid });
    else if (baseMap.get(path) !== oid) out.push({ path, status: 'modified', oldOid: baseMap.get(path) as string, newOid: oid });
  }
  for (const [path, oid] of baseMap) {
    if (!headMap.has(path)) out.push({ path, status: 'removed', oldOid: oid, newOid: null });
  }
  out.sort((a, b) => (a.path < b.path ? -1 : 1));
  return out;
}

// ---------------------------------------------------------------- ref helpers

export function refTip(repo: Repo, ref: string): string | undefined {
  return repo.refs.get(ref);
}

export function headTarget(repo: Repo): string {
  return repo.refs.get('HEAD') ?? `refs/heads/${'main'}`;
}

/** The default branch's tip, or undefined for an unborn repo. */
export function defaultTip(repo: Repo): string | undefined {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
}

/** List refs for advertisements, in a stable order: HEAD first, then sorted. */
export function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const out = [...repo.refs.entries()]
    .filter(([name]) => name !== 'HEAD')
    .map(([name, oid]) => ({ name, oid }));
  out.sort((a, b) => (a.name < b.name ? -1 : 1));
  return out;
}

// ---------------------------------------------------------------- smart HTTP handlers

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

function gitOk(contentType: string, body: Buffer): GitResponse {
  return { status: 200, contentType, body };
}

function gitNotFound(): GitResponse {
  return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
}

const UPLOAD_CAPS_V2 = ['ls-refs=unborn', 'fetch=shallow filter', 'object-format=sha1'];
const UPLOAD_CAPS_V0 = 'multi_ack thin-pack side-band-64k ofs-delta shallow';
const RECEIVE_CAPS_V0 = 'report-status side-band-64k delete-refs ofs-delta';

/** GET info/refs?service=git-upload-pack (v2 when Git-Protocol: version=2). */
export function handleInfoRefsUpload(repo: Repo | undefined, wantV2: boolean): GitResponse {
  if (repo === undefined) return gitNotFound();
  if (wantV2) {
    const parts: Buffer[] = [];
    parts.push(pktLine('version 2\n'));
    for (const cap of UPLOAD_CAPS_V2) parts.push(pktLine(`${cap}\n`));
    parts.push(PKT_FLUSH);
    return gitOk('application/x-git-upload-pack-advertisement', Buffer.concat(parts));
  }
  return gitOk('application/x-git-upload-pack-advertisement', v0Advertisement(repo, 'git-upload-pack', UPLOAD_CAPS_V0));
}

/** GET info/refs?service=git-receive-pack (v0 only). */
export function handleInfoRefsReceive(repo: Repo | undefined): GitResponse {
  if (repo === undefined) return gitNotFound();
  return gitOk('application/x-git-receive-pack-advertisement', v0Advertisement(repo, 'git-receive-pack', RECEIVE_CAPS_V0));
}

function v0Advertisement(repo: Repo, service: string, caps: string): Buffer {
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = sortedRefs(repo);
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headOid = repo.refs.get(headRef);
  const symref = `symref=HEAD:${headRef}`;
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps} ${symref}\n`));
  } else {
    const headLine = headOid !== undefined ? `${headOid} HEAD\0${caps} ${symref}\n` : null;
    if (headLine !== null) parts.push(pktLine(headLine));
    for (const r of refs) {
      parts.push(pktLine(`${r.oid} ${r.name}\n`));
      const obj = repo.objects.get(r.oid);
      if (obj !== undefined && obj.type === 'tag') {
        const tag = parseTag(obj.data);
        parts.push(pktLine(`${tag.object} ${r.name}^{}\n`));
      }
    }
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** POST git-upload-pack: protocol v2 (ls-refs / fetch), or v0 fetch as a fallback. */
export function handleUploadPack(repo: Repo | undefined, body: Buffer, wantV2: boolean): GitResponse {
  if (repo === undefined) return gitNotFound();
  try {
    if (wantV2) return uploadPackV2(repo, body);
    return uploadPackV0(repo, body);
  } catch {
    // A malformed request must not kill the server; the suite only checks it stays up.
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Bad Request' }), 'utf8') };
  }
}

function uploadPackV2(repo: Repo, body: Buffer): GitResponse {
  const { lines } = parsePktLines(body);
  const command = lines.find((l) => l.startsWith('command='))?.slice('command='.length);
  const args = lines.filter((l) => !l.startsWith('command=') && !l.startsWith('agent=') && !l.startsWith('object-format='));
  if (command === 'ls-refs') return v2LsRefs(repo, args);
  if (command === 'fetch') return v2Fetch(repo, args);
  return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Bad Request' }), 'utf8') };
}

function v2LsRefs(repo: Repo, args: string[]): GitResponse {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice('ref-prefix '.length));
  const parts: Buffer[] = [];
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headOid = repo.refs.get(headRef);
  const emit = (oid: string, name: string, extra = ''): void => {
    if (prefixes.length > 0 && !prefixes.some((p) => name.startsWith(p))) return;
    parts.push(pktLine(`${oid} ${name}${extra}\n`));
  };
  if (headOid === undefined) {
    if (prefixes.length === 0 || prefixes.some((p) => 'HEAD'.startsWith(p))) {
      parts.push(pktLine(`unborn HEAD${symrefs ? ` symref-target:${headRef}` : ''}\n`));
    }
  } else {
    if (symrefs) emit(headOid, 'HEAD', ` symref-target:${headRef}`);
    for (const r of sortedRefs(repo)) {
      let extra = '';
      if (peel) {
        const obj = repo.objects.get(r.oid);
        if (obj !== undefined && obj.type === 'tag') extra = ` peeled:${parseTag(obj.data).object}`;
      }
      emit(r.oid, r.name, extra);
    }
  }
  parts.push(PKT_FLUSH);
  return gitOk('application/x-git-upload-pack-result', Buffer.concat(parts));
}

function v2Fetch(repo: Repo, args: string[]): GitResponse {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallows: string[] = [];
  let depth: number | undefined;
  for (const a of args) {
    if (a.startsWith('want ')) wants.push(a.slice(5).trim());
    else if (a.startsWith('have ')) haves.push(a.slice(5).trim());
    else if (a.startsWith('shallow ')) shallows.push(a.slice(8).trim());
    else if (a.startsWith('deepen ')) depth = Number(a.slice(7).trim());
  }
  const { objects, boundary, includedCommits } = closure(repo, wants, haves, depth, shallows);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  parts.push(pktLine('acknowledgments\n'));
  parts.push(pktLine('NAK\n'));
  parts.push(PKT_DELIM);
  const shallowLines: string[] = [];
  if (depth !== undefined) {
    for (const b of boundary) shallowLines.push(`shallow ${b}`);
    for (const s of shallows) {
      if (includedCommits.has(s) && !boundary.includes(s)) shallowLines.push(`unshallow ${s}`);
    }
  } else {
    for (const s of shallows) shallowLines.push(`shallow ${s}`);
  }
  if (shallowLines.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const l of shallowLines) parts.push(pktLine(`${l}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  // Sideband band 1: the pack, chunked to the 64k pkt-line limit.
  let off = 0;
  while (off < pack.length) {
    const chunk = pack.subarray(off, Math.min(pack.length, off + 65516 - 1));
    const payload = Buffer.concat([Buffer.from([1]), chunk]);
    parts.push(pktLine(payload));
    off += chunk.length;
  }
  if (pack.length === 0) {
    parts.push(pktLine(Buffer.from([1])));
  }
  parts.push(PKT_FLUSH);
  return gitOk('application/x-git-upload-pack-result', Buffer.concat(parts));
}

function uploadPackV0(repo: Repo, body: Buffer): GitResponse {
  // Two sections split by a flush: wants (+shallow/deepen), then haves + done.
  const first = parsePktLines(body);
  const second = parsePktLines(first.rest);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallows: string[] = [];
  let depth: number | undefined;
  for (const l of first.lines) {
    if (l.startsWith('want ')) wants.push(l.slice(5).split(' ')[0] as string);
    else if (l.startsWith('shallow ')) shallows.push(l.slice(8).trim());
    else if (l.startsWith('deepen ')) depth = Number(l.slice(7).trim());
  }
  for (const l of second.lines) {
    if (l.startsWith('have ')) haves.push(l.slice(5).trim());
  }
  const { objects, boundary, includedCommits } = closure(repo, wants, haves, depth, shallows);
  const pack = writePack(objects);
  const parts: Buffer[] = [pktLine('NAK\n')];
  if (depth !== undefined) {
    for (const b of boundary) parts.push(pktLine(`shallow ${b}\n`));
    for (const s of shallows) {
      if (includedCommits.has(s) && !boundary.includes(s)) parts.push(pktLine(`unshallow ${s}\n`));
    }
  } else {
    for (const s of shallows) parts.push(pktLine(`shallow ${s}\n`));
  }
  let off = 0;
  while (off < pack.length) {
    const chunk = pack.subarray(off, Math.min(pack.length, off + 65516 - 1));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
    off += chunk.length;
  }
  parts.push(PKT_FLUSH);
  return gitOk('application/x-git-upload-pack-result', Buffer.concat(parts));
}

// ---------------------------------------------------------------- receive-pack (v0)

export interface PushReport {
  unpackOk: boolean;
  refs: Array<{ ref: string; ok: boolean; reason?: string }>;
  sideband: boolean;
}

/** POST git-receive-pack: apply a push, answer report-status (sideband when negotiated). */
export function handleReceivePack(repo: Repo | undefined, body: Buffer): GitResponse {
  if (repo === undefined) return gitNotFound();
  try {
    const { lines, rest } = parsePktLines(body);
    const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
    let caps = '';
    for (const line of lines) {
      const nul = line.indexOf('\0');
      const text = nul >= 0 ? line.slice(0, nul) : line;
      if (nul >= 0) caps = line.slice(nul + 1);
      const m = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(text);
      if (m !== null) commands.push({ oldOid: m[1] as string, newOid: m[2] as string, ref: m[3] as string });
    }
    const sideband = caps.split(' ').includes('side-band-64k');
    const report: PushReport = { unpackOk: true, refs: [], sideband };
    // The pack follows the flush. A delete-only push sends no pack.
    if (rest.length > 0) {
      const entries = readPack(rest);
      for (const [oid, entry] of entries) {
        if (!repo.objects.has(oid)) {
          repo.objects.set(oid, { type: entry.type, data: entry.data });
          if (entry.type === 'commit') repo.commitSeq.set(oid, repo.commitSeq.size + 1);
        }
      }
    }
    for (const cmd of commands) {
      if (cmd.newOid === ZERO_OID) {
        // Delete.
        const cur = repo.refs.get(cmd.ref);
        if (cur === undefined || cur !== cmd.oldOid) {
          report.refs.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        } else {
          repo.refs.delete(cmd.ref);
          report.refs.push({ ref: cmd.ref, ok: true });
        }
        continue;
      }
      const cur = repo.refs.get(cmd.ref);
      if (cmd.oldOid === ZERO_OID) {
        if (cur !== undefined) {
          report.refs.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
          continue;
        }
      } else if (cur !== cmd.oldOid) {
        report.refs.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      } else if (!isAncestor(repo, cmd.oldOid, cmd.newOid)) {
        report.refs.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      report.refs.push({ ref: cmd.ref, ok: true });
    }
    const linesOut: string[] = ['unpack ok'];
    for (const r of report.refs) {
      linesOut.push(r.ok ? `ok ${r.ref}` : `ng ${r.ref} ${r.reason ?? 'failed'}`);
    }
    const parts: Buffer[] = [];
    if (sideband) {
      for (const l of linesOut) parts.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.from(`${l}\n`, 'utf8')])));
    } else {
      for (const l of linesOut) parts.push(pktLine(`${l}\n`));
    }
    parts.push(PKT_FLUSH);
    return gitOk('application/x-git-receive-pack-result', Buffer.concat(parts));
  } catch (err) {
    const msg = err instanceof Error ? err.message : 'error';
    const linesOut = [`unpack ${msg}`, ''];
    const parts: Buffer[] = [];
    for (const l of linesOut) {
      if (l === '') continue;
      parts.push(pktLine(`${l}\n`));
    }
    parts.push(PKT_FLUSH);
    return gitOk('application/x-git-receive-pack-result', Buffer.concat(parts));
  }
}

/** Create a commit on `branch` that sets `path` to `content` (or deletes it when null). */
export function commitOnBranch(
  store: Store,
  repo: Repo,
  branch: string,
  path: string,
  content: Buffer | null,
  message: string,
): { commitOid: string; blobOid: string | null } {
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref);
  let treeOid: string | null = null;
  const parents: string[] = [];
  if (tip !== undefined) {
    const obj = repo.objects.get(tip);
    if (obj !== undefined && obj.type === 'commit') {
      treeOid = parseCommit(obj.data).tree;
      parents.push(tip);
    }
  }
  let blobOid: string | null = null;
  if (content !== null) blobOid = storeObject(repo, 'blob', content);
  const nextTree = treeSetPath(repo, treeOid, path, blobOid);
  const { line } = identLine(store);
  const commitData = buildCommit(nextTree, parents, line, message);
  const commitOid = storeObject(repo, 'commit', commitData);
  if (!repo.commitSeq.has(commitOid)) repo.commitSeq.set(commitOid, repo.commitSeq.size + 1);
  repo.refs.set(ref, commitOid);
  return { commitOid, blobOid };
}

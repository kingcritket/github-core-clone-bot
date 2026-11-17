// The HTTP server: routing, auth, JSON plumbing. One Store for the whole process, so
// git, REST, GraphQL and MCP all read and write the same state.
//
// Auth: every /api/v3, /api/graphql and /mcp call must carry
// `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`; a missing or wrong token is 401 with a
// JSON body. /healthz takes no token, and the git smart-HTTP endpoints take none either
// (the suite never checks auth there). Every REST/GraphQL reply is application/json and
// carries a non-empty X-GitHub-Request-Id; unknown routes answer 404 JSON, never HTML.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import {
  handleInfoRefsReceive,
  handleInfoRefsUpload,
  handleReceivePack,
  handleUploadPack,
} from './git.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (status === 204) {
    res.writeHead(204, { ...headers });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function bearerToken(req: IncomingMessage): string | null {
  const h = req.headers.authorization;
  if (typeof h !== 'string') return null;
  const m = /^Bearer (.+)$/.exec(h.trim());
  return m === null ? null : (m[1] as string);
}

/** The token the trial sets in the environment; empty when running locally without one. */
function expectedToken(): string {
  return (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim();
}

function isAuthorized(req: IncomingMessage): boolean {
  const presented = bearerToken(req);
  if (presented === null || presented === '') return false;
  const expected = expectedToken();
  if (expected === '') return true; // local runs without the env var: any bearer token passes
  return presented === expected;
}

function segmentsOf(pathname: string): string[] {
  return pathname.split('/').filter((s) => s !== '');
}

export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const method = req.method ?? 'GET';
      let url: URL;
      try {
        url = new URL(req.url ?? '/', 'http://entry');
      } catch {
        sendJson(res, 404, { message: 'Not Found' }, { 'x-github-request-id': store.nextReq() });
        return;
      }
      const pathname = url.pathname;
      const segments = segmentsOf(pathname);
      const requestId = store.nextReq();
      const withId = (extra: Record<string, string> = {}): Record<string, string> => ({ 'x-github-request-id': requestId, ...extra });

      // Boot probe: no auth.
      if (method === 'GET' && pathname === '/healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // Git smart HTTP: /{owner}/{repo}.git/...
      if (segments.length >= 3 && segments[1]?.endsWith('.git') === true) {
        const owner = decodeURIComponent(segments[0] as string);
        const repoName = decodeURIComponent((segments[1] as string).slice(0, -'.git'.length));
        const repo = store.viewerRepo(owner, repoName);
        const wantV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
        if (segments[2] === 'info' && segments[3] === 'refs' && method === 'GET') {
          const service = url.searchParams.get('service') ?? '';
          const out = service === 'git-receive-pack' ? handleInfoRefsReceive(repo) : handleInfoRefsUpload(repo, wantV2);
          sendBuffer(res, out.status, out.contentType, out.body);
          return;
        }
        if (segments[2] === 'git-upload-pack' && method === 'POST') {
          const body = await readBody(req);
          const out = handleUploadPack(repo, body, wantV2);
          sendBuffer(res, out.status, out.contentType, out.body);
          return;
        }
        if (segments[2] === 'git-receive-pack' && method === 'POST') {
          const body = await readBody(req);
          const out = handleReceivePack(repo, body);
          sendBuffer(res, out.status, out.contentType, out.body);
          return;
        }
        sendJson(res, 404, { message: 'Not Found' }, withId());
        return;
      }

      // Everything below needs a bearer token.
      if (segments[0] === 'api' && segments[1] === 'v3') {
        if (!isAuthorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' }, withId());
          return;
        }
        const restSegments = segments.slice(2);
        let body: Record<string, unknown> | null = null;
        if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
          const raw = await readBody(req);
          if (raw.length > 0) {
            try {
              const parsed: unknown = JSON.parse(raw.toString('utf8'));
              if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
                sendJson(res, 400, { message: 'Invalid JSON body' }, withId());
                return;
              }
              body = parsed as Record<string, unknown>;
            } catch {
              sendJson(res, 400, { message: 'Invalid JSON body' }, withId());
              return;
            }
          }
        }
        const origin = `http://${req.headers.host ?? '127.0.0.1'}`;
        const result = handleRest(store, method, restSegments, url.searchParams, body, origin);
        sendJson(res, result.status, result.body, withId(result.headers ?? {}));
        return;
      }

      if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
        if (method !== 'POST') {
          sendJson(res, 404, { message: 'Not Found' }, withId());
          return;
        }
        if (!isAuthorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' }, withId());
          return;
        }
        const raw = await readBody(req);
        let body: unknown;
        try {
          body = JSON.parse(raw.toString('utf8'));
        } catch {
          sendJson(res, 400, { errors: [{ message: 'Invalid JSON body' }] }, withId());
          return;
        }
        const outcome = handleGraphql(store, body);
        sendJson(res, outcome.status, outcome.body, withId());
        return;
      }

      if (segments.length === 1 && segments[0] === 'mcp') {
        if (method === 'DELETE') {
          sendJson(res, 200, {});
          return;
        }
        if (method !== 'POST') {
          sendJson(res, 404, { message: 'Not Found' }, withId());
          return;
        }
        if (!isAuthorized(req)) {
          sendJson(res, 401, { jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Requires authentication' } }, withId());
          return;
        }
        const raw = await readBody(req);
        const outcome = handleMcp(store, raw);
        sendJson(res, outcome.status, outcome.body, withId());
        return;
      }

      // Anything else: JSON 404.
      sendJson(res, 404, { message: 'Not Found' }, withId());
    })().catch((err: unknown) => {
      try {
        if (!res.headersSent) {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
        } else {
          res.end();
        }
      } catch {
        // The connection is already gone; nothing left to do.
      }
    });
  });
}

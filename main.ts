// main.ts — the entrypoint of this forge (v3, Go core).
//
// The trial runs `node /bot/main.ts`. This file is a thin launcher: it starts the
// Go forge binary (`./forge`, a statically linked Linux amd64 binary committed in
// this repo as vendored source), reads the ephemeral port the binary prints on
// stdout, registers that port with the trial environment, and waits until the
// environment says the trial is finished — then stops the forge and exits.
//
// Everything GitHub-shaped — repos, REST, GraphQL, MCP, git smart HTTP — lives in
// the Go binary (main.go, store.go, git.go, rest.go, graphql.go, mcp.go, app.go in
// this repo). Nothing in this file implements any of that; it only performs the
// handshake above and then gets out of the way.
//
// Written in plain JavaScript-compatible TypeScript (no type annotations) so it
// runs under `node /bot/main.ts` exactly like the v2 entrypoint did.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

// The env's origin, e.g. http://store:8080 inside a trial container. Trailing
// slashes are stripped so the /register and /done paths below join cleanly.
const ENV_ORIGIN = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

// One register or done request may take at most this long before it is given
// up on and retried.
const REQUEST_BUDGET_MS = 30_000;

// Retry spacing: start short and double each time, never waiting longer than
// the cap. The env may still be booting when we first knock, so the first few
// waits are deliberately small.
const FIRST_WAIT_MS = 250;
const LONGEST_WAIT_MS = 2_000;

// How long to wait for the forge binary to print its PORT line.
const FORGE_START_TIMEOUT_MS = 30_000;

/** Print a message to stderr and leave with a failure status. */
function fail(message) {
  process.stderr.write(`[entry] ${message}\n`);
  process.exit(1);
}

// Without an env origin there is nobody to register with and nothing to wait
// for, so there is no point starting the server at all.
if (ENV_ORIGIN === '') {
  fail('MAKEFUN_ENV_URL is not set: no environment to register with');
}

const pause = (ms) => new Promise((wake) => setTimeout(wake, ms));

/** How long to wait after the `failures`-th failed attempt: doubling, capped. */
function waitAfter(failures) {
  return Math.min(LONGEST_WAIT_MS, FIRST_WAIT_MS * 2 ** Math.min(failures, 8));
}

// The forge binary lives beside this file in the repo (committed as vendored
// source). Resolve it from this file's own URL, never from the working dir.
const FORGE_PATH = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

let forge = null;

/** Start the Go forge and resolve with the ephemeral port it prints. */
function startForge() {
  return new Promise((resolve, reject) => {
    const child = spawn(FORGE_PATH, [], { stdio: ['ignore', 'pipe', 'pipe'] });
    forge = child;
    let buffer = '';
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        child.kill();
        reject(new Error('forge did not print its port in time'));
      }
    }, FORGE_START_TIMEOUT_MS);
    child.stdout.on('data', (chunk) => {
      if (settled) return;
      buffer += chunk.toString('utf8');
      const nl = buffer.indexOf('\n');
      if (nl < 0) return;
      const line = buffer.slice(0, nl).trim();
      const m = /^PORT (\d+)$/.exec(line);
      if (m === null) {
        clearTimeout(timer);
        settled = true;
        child.kill();
        reject(new Error(`forge printed an unexpected first line: ${line}`));
        return;
      }
      clearTimeout(timer);
      settled = true;
      resolve(Number(m[1]));
    });
    child.stderr.on('data', (chunk) => {
      process.stderr.write(`[forge] ${chunk.toString('utf8')}`);
    });
    child.on('error', (err) => {
      if (!settled) {
        clearTimeout(timer);
        settled = true;
        reject(err);
      }
    });
    child.on('exit', (code) => {
      if (!settled) {
        clearTimeout(timer);
        settled = true;
        reject(new Error(`forge exited early with code ${code}`));
      }
    });
  });
}

/**
 * Tell the env which port the forge is listening on. The env answers 200 when
 * it records the port, and 409 when another entrypoint already registered
 * first — either way this process is done here and just waits for the trial to
 * end. A 400 or 405 means the request itself is wrong, which retrying cannot
 * fix, so that is fatal. Anything else — the env unreachable, a timeout, an
 * unexpected status — is treated as "not ready yet" and retried.
 */
async function announce(port) {
  let failures = 0;
  for (;;) {
    let answer;
    try {
      answer = await fetch(`${ENV_ORIGIN}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
    } catch {
      // The env is not answering yet. Wait a little longer each time and knock again.
      await pause(waitAfter(failures));
      failures += 1;
      continue;
    }
    if (answer.status === 200 || answer.status === 409) return;
    if (answer.status === 400 || answer.status === 405) {
      fail(`register rejected with HTTP ${answer.status}: ${await answer.text()}`);
    }
    await pause(waitAfter(failures));
    failures += 1;
  }
}

/**
 * Ask the env, over and over, whether the trial is over. A healthy answer of
 * {"done": true} ends the loop; {"done": false} just means "not yet". A failed
 * request or a malformed answer is not treated as done either — the loop keeps
 * going, with the wait between attempts growing while the env stays silent
 * and resetting to the short wait once it answers again.
 */
async function waitForFinish() {
  let failures = 0;
  for (;;) {
    try {
      const answer = await fetch(`${ENV_ORIGIN}/done`, {
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      if (answer.ok) {
        const payload = await answer.json();
        if (payload.done === true) return;
        failures = 0;
      }
    } catch {
      // No answer: the trial is certainly not over, so keep asking.
    }
    await pause(waitAfter(failures));
    failures += 1;
  }
}

/**
 * The whole life of the process: start the Go forge on an ephemeral port,
 * announce the port, then wait for the env to declare the trial finished
 * before stopping the forge.
 */
async function run() {
  const port = await startForge();
  process.stdout.write(`[entry] forge listening on ephemeral port ${port}\n`);
  await announce(port);
  process.stdout.write('[entry] registered with the environment; waiting for it to finish\n');
  await waitForFinish();
  if (forge !== null) forge.kill();
  process.stdout.write('[entry] environment finished; forge stopped\n');
}

process.on('SIGTERM', () => {
  if (forge !== null) forge.kill();
  process.exit(0);
});
process.on('SIGINT', () => {
  if (forge !== null) forge.kill();
  process.exit(0);
});

try {
  await run();
} catch (error) {
  if (forge !== null) forge.kill();
  fail(error instanceof Error ? error.message : String(error));
}
process.exit(0);

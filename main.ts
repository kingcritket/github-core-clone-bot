// main.ts — the entrypoint of this forge, and the only file that talks to the
// make.fun trial environment. The environment (env) runs the trial: it owns port
// 8080, drives the suite against whatever this process serves, and decides when
// the trial is over. This process has a short job list, in this order:
//
//   1. Start the forge on a port the kernel picks for us.
//   2. Tell the env that port, so the suite knows where to send its requests.
//   3. Wait quietly until the env says the trial is finished, then stop.
//
// Everything GitHub-shaped — repos, REST, GraphQL, MCP, git smart HTTP — lives
// in app.ts and the modules behind it. Nothing in this file implements any of
// that; it only performs the handshake above and then gets out of the way.

import { createApp } from './app.ts';

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

/** Print a message to stderr and leave with a failure status. */
function fail(message: string): never {
  process.stderr.write(`[entry] ${message}\n`);
  process.exit(1);
}

// Without an env origin there is nobody to register with and nothing to wait
// for, so there is no point starting the server at all.
if (ENV_ORIGIN === '') {
  fail('MAKEFUN_ENV_URL is not set: no environment to register with');
}

const pause = (ms: number): Promise<void> =>
  new Promise((wake) => setTimeout(wake, ms));

/** How long to wait after the `failures`-th failed attempt: doubling, capped. */
function waitAfter(failures: number): number {
  return Math.min(LONGEST_WAIT_MS, FIRST_WAIT_MS * 2 ** Math.min(failures, 8));
}

/**
 * Tell the env which port the forge is listening on. The env answers 200 when
 * it records the port, and 409 when another entrypoint already registered
 * first — either way this process is done here and just waits for the trial to
 * end. A 400 or 405 means the request itself is wrong, which retrying cannot
 * fix, so that is fatal. Anything else — the env unreachable, a timeout, an
 * unexpected status — is treated as "not ready yet" and retried.
 */
async function announce(port: number): Promise<void> {
  let failures = 0;
  for (;;) {
    let answer: Response;
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
async function waitForFinish(): Promise<void> {
  let failures = 0;
  for (;;) {
    try {
      const answer = await fetch(`${ENV_ORIGIN}/done`, {
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      if (answer.ok) {
        const payload = (await answer.json()) as { done?: unknown };
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
 * The whole life of the process: bind the forge to an ephemeral port (never
 * 8080 — that port belongs to the env), announce the port, then wait for the
 * env to declare the trial finished before closing the server.
 */
async function run(): Promise<void> {
  const server = createApp();
  const port = await new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      const bound = server.address();
      if (bound === null || typeof bound === 'string') {
        reject(new Error('server did not bind a TCP port'));
        return;
      }
      resolve(bound.port);
    });
  });

  process.stdout.write(`[entry] forge listening on ephemeral port ${port}\n`);
  await announce(port);
  process.stdout.write('[entry] registered with the environment; waiting for it to finish\n');
  await waitForFinish();
  await new Promise<void>((resolve) => server.close(() => resolve()));
  process.stdout.write('[entry] environment finished; forge stopped\n');
}

try {
  await run();
} catch (error) {
  fail(error instanceof Error ? error.message : String(error));
}
process.exit(0);

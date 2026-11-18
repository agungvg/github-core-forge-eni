// main.ts — the handshake between this forge and the trial harness.
//
// The harness (the "env") is the only client that matters at boot. It owns
// port 8080, waits for this process to announce the port it bound, probes
// GET /healthz on that port, runs the whole conformance suite and the timed
// workloads against it, and finally answers GET /done with {"done": true}.
// Everything in this file is that handshake; the forge itself is built by
// app.ts and the modules behind it.
//
// The order of business:
//   1. Read MAKEFUN_ENV_URL. With no env to talk to there is nothing to do,
//      so the process exits 1 right away.
//   2. Build the forge with createApp() and listen on port 0, letting the
//      kernel pick a free port. Port 8080 belongs to the env and is never
//      bound here.
//   3. POST {"port": <the bound port>} to <env>/register, retrying until the
//      env takes it. A 409 means another entry registered first; that is not
//      an error for this process, which simply waits like any other.
//   4. Poll GET <env>/done until it answers {"done": true}, then close the
//      server and exit 0. The process never leaves early on its own say-so.

import { createApp } from './app.ts';

// The env's base URL, trailing slashes stripped so the paths below join up.
const envBase = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envBase.length === 0) {
  process.stderr.write('entry: MAKEFUN_ENV_URL is unset, so there is no env to register with\n');
  process.exit(1);
}

// One request to the env may take this long before it is given up on and
// retried. Retries start this far apart, double each time, and never wait
// longer than the cap, so a still-starting env is polled often.
const CALL_TIMEOUT_MS = 30_000;
const BACKOFF_FIRST_MS = 250;
const BACKOFF_LIMIT_MS = 2_000;

function pause(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// How long to wait after the nth failed attempt: exponential from
// BACKOFF_FIRST_MS, capped at BACKOFF_LIMIT_MS.
function backoffAfter(failures: number): number {
  const grown = BACKOFF_FIRST_MS * 2 ** Math.min(failures, 8);
  return Math.min(BACKOFF_LIMIT_MS, grown);
}

// Thrown by a step when the env's answer means the contract itself is wrong
// (a bad request, a wrong method); unlike a dropped connection or a slow env,
// that will not heal by retrying, so it ends the process.
class FatalEnvError extends Error {}

// What one attempt of a retried step reports: either the step is finished,
// or it should be tried again after a backoff.
type StepOutcome = 'settled' | 'retry';

// Run `step` until it reports settled, backing off between failures. A step
// that throws FatalEnvError ends the run at once; anything else — a refused
// connection, a timeout, a 5xx — is treated as "the env is not ready yet".
async function keepTrying(label: string, step: () => Promise<StepOutcome>): Promise<void> {
  let failures = 0;
  for (;;) {
    try {
      if ((await step()) === 'settled') return;
    } catch (err) {
      if (err instanceof FatalEnvError) throw err;
      process.stderr.write(`entry: ${label} failed (${err instanceof Error ? err.message : String(err)}), retrying\n`);
    }
    failures += 1;
    await pause(backoffAfter(failures));
  }
}

// Announce the bound port to the env. 200 is the normal answer and 409 means
// a first registration already won; 400 and 405 are contract errors and end
// the program, while any other status is retried like a network failure.
async function announcePort(port: number): Promise<StepOutcome> {
  const res = await fetch(`${envBase}/register`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ port }),
    signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
  });
  if (res.status === 200 || res.status === 409) return 'settled';
  if (res.status === 400 || res.status === 405) {
    throw new FatalEnvError(`register answered HTTP ${res.status}: ${await res.text()}`);
  }
  return 'retry';
}

// One poll of GET /done. Settled only when the body says the trial is over;
// a non-OK answer, a malformed body, or a failed request all mean "not yet".
async function pollDone(): Promise<StepOutcome> {
  const res = await fetch(`${envBase}/done`, { signal: AbortSignal.timeout(CALL_TIMEOUT_MS) });
  if (res.ok) {
    const body = (await res.json()) as { done?: unknown };
    if (body.done === true) return 'settled';
  }
  return 'retry';
}

// The whole life of the process: bind, register, wait for done, shut down.
async function run(): Promise<void> {
  const server = createApp();
  const port = await new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      const bound = server.address();
      if (bound === null || typeof bound === 'string') {
        reject(new Error('the forge did not bind an ephemeral TCP port'));
        return;
      }
      resolve(bound.port);
    });
  });
  process.stdout.write(`entry: forge listening on port ${port}\n`);

  await keepTrying('register', () => announcePort(port));
  process.stdout.write('entry: registered with the env, waiting for done\n');

  await keepTrying('done poll', pollDone);
  server.close();
  process.stdout.write('entry: env said done, exiting\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`entry: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}

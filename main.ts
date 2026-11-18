// Launcher for the Go forge binary.
//
// The trial pod mounts /bot read-only (and may mount it noexec), and the
// committed binary may not carry the exec bit, so the binary is copied to
// a fresh tmp dir, chmod'ed executable, and run from there. All stdio and
// env are inherited; the exit code / signal propagates. If the tmp copy
// cannot run (e.g. tmp is noexec), fall back to the committed binary
// directly. The Go binary itself does the whole env handshake
// ($MAKEFUN_ENV_URL/register, /healthz, long-poll /done).
import { spawn } from 'node:child_process';
import { copyFileSync, chmodSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const committed = path.join(here, 'forge-go');

let done = false;
function finish(code: number | null, signal: NodeJS.Signals | null): void {
  if (done) return;
  done = true;
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
}

function run(binPath: string, fallback: (() => void) | null): void {
  const child = spawn(binPath, [], { stdio: 'inherit', env: process.env });
  child.on('error', (err: NodeJS.ErrnoException) => {
    if (fallback && (err.code === 'EACCES' || err.code === 'EPERM')) {
      fallback();
      return;
    }
    console.error('forge launch failed:', err.message);
    process.exit(1);
  });
  child.on('exit', (code, signal) => finish(code, signal));
}

let staged: string | null = null;
try {
  const dir = mkdtempSync(path.join(tmpdir(), 'forge-'));
  const dst = path.join(dir, 'forge-go');
  copyFileSync(committed, dst);
  chmodSync(dst, 0o755);
  staged = dst;
} catch {
  staged = null;
}

if (staged) {
  run(staged, () => run(committed, null));
} else {
  try {
    chmodSync(committed, 0o755);
  } catch {
    // read-only /bot: chmod is best-effort only.
  }
  run(committed, null);
}

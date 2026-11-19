// Node entrypoint for the Go forge.
//
// The trial pod can mount /bot read-only, and it can mount it noexec too;
// the committed forge-go binary may also arrive without its execute bit.
// So this launcher stages the binary into a fresh temporary directory,
// marks the copy executable, and runs it there. Stdio and the environment
// are inherited untouched, and the child's exit code — or its signal — is
// passed straight back to the harness. If the staged copy refuses to
// start (a noexec /tmp is the usual cause), the launcher retries against
// the committed binary directly. Everything after that — the register
// POST, /healthz, the long poll on /done — belongs to the Go binary.
import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
const root = path.dirname(fileURLToPath(import.meta.url));
const shipped = path.join(root, 'forge-go');

let settled = false;

function finish(code: number | null, signal: NodeJS.Signals | null): void {
  if (settled) return;
  settled = true;
  if (signal !== null) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
}

function start(binary: string, onDenied: (() => void) | null): void {
  const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
  child.on('error', (err: NodeJS.ErrnoException) => {
    if (onDenied !== null && (err.code === 'EACCES' || err.code === 'EPERM')) {
      onDenied();
      return;
    }
    console.error('forge launch failed:', err.message);
    process.exit(1);
  });
  child.on('exit', (code, signal) => finish(code, signal));
}

function stage(): string | null {
  try {
    const dir = mkdtempSync(path.join(tmpdir(), 'forge-'));
    const copy = path.join(dir, 'forge-go');
    copyFileSync(shipped, copy);
    chmodSync(copy, 0o755);
    return copy;
  } catch {
    return null;
  }
}

const staged = stage();
if (staged !== null) {
  start(staged, () => start(shipped, null));
} else {
  try { chmodSync(shipped, 0o755); } catch { /* /bot may be read-only */ }
  start(shipped, null);
}

// node's automatic contexts and server_name (spec §6.2, TASK-04). `node:os` is imported here, by the node
// entry only — no browser or edge bundle resolves this module (package.json's exports conditions, T11).

import os from 'node:os';
import type { AutoContexts } from '../core/types.js';

function cutCodePoints(s: string, max: number): string {
  const cps = Array.from(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

/** `os.platform()` mapped to Sentry's OS names; anything else falls back to `os.type()` (spec §6.2). */
function osName(): string {
  try {
    const platform = os.platform();
    if (platform === 'linux') return 'Linux';
    if (platform === 'darwin') return 'macOS';
    if (platform === 'win32') return 'Windows';
    return os.type();
  } catch {
    return '';
  }
}

/** node's automatic `contexts.runtime` / `contexts.os` and `server_name` (spec §6.2). Never throws. */
export function nodeAutoContexts(serverNameOption: unknown): AutoContexts {
  const contexts: Record<string, Record<string, unknown>> = {};
  try {
    contexts.runtime = { name: 'node', version: process.version };
  } catch {
    // Skipped.
  }
  let hostname = '';
  try {
    hostname = os.hostname();
  } catch {
    hostname = '';
  }
  try {
    const name = osName();
    const kernel = os.release();
    if (name !== '' || kernel !== '') contexts.os = { name, kernel_version: kernel };
  } catch {
    // Skipped.
  }
  const serverName = typeof serverNameOption === 'string' && serverNameOption !== '' ? serverNameOption : hostname;
  const out: AutoContexts = {};
  if (Object.keys(contexts).length > 0) out.contexts = contexts;
  if (serverName !== '') out.server_name = cutCodePoints(serverName, 256);
  return out;
}

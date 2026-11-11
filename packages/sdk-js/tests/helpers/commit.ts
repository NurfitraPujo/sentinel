// The single "write locally, compare under CI" rule every generated fixture and sidecar shares (spec §7.3):
// deterministic output is rewritten only when its content changed, and under `CI` it must already equal what the
// suite computed — a missing or drifted file fails the test, naming the path, rather than being silently
// rewritten. Extracted from the tunnel-fixtures suite so the golden corpus and its estimate sidecars follow the
// same rule (the golden corpus was not under this gate before, spec §7.3).

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { expect } from 'vitest';

/** Write (locally) or compare (under CI) one committed JSON file at the exact `path` given. */
export function commit(path: string, value: unknown): void {
  const content = JSON.stringify(value, null, 2) + '\n';
  const committed = existsSync(path) ? readFileSync(path, 'utf8') : undefined;
  if (process.env.CI) {
    expect(committed, `${path} is missing — run the sdk-js suite locally and commit it`).toBeDefined();
    expect(content, `${path} drifted from the suite's real output — regenerate and commit it`).toBe(committed);
    return;
  }
  mkdirSync(dirname(path), { recursive: true });
  if (committed !== content) writeFileSync(path, content);
}

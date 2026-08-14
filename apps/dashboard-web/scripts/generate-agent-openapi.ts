import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { generateAgentOpenApiDocument, toYaml } from '../src/lib/server/agent-api-spec/generate';

/**
 * N6: CLI entry for `pnpm openapi:agent`. Thin -- all the actual conversion logic lives in
 * `src/lib/server/agent-api-spec/generate.ts` so `openapi-drift.test.ts` can import the exact same
 * code path without going through the filesystem write below.
 */

// This file lives at apps/dashboard-web/scripts/ -- three levels up is the repo root.
const outPath = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../docs/agents/openapi.agent.yaml');
const doc = generateAgentOpenApiDocument();
writeFileSync(outPath, toYaml(doc), 'utf8');
// eslint-disable-next-line no-console
console.log(`Wrote ${outPath}`);

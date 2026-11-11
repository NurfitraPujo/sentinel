import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';
import { generateAgentOpenApiDocument } from './generate';

/**
 * N6: `docs/agents/openapi.agent.yaml` is GENERATED (see generate.ts / scripts/generate-agent-openapi.ts).
 * This test regenerates the document in memory from the SAME registry the committed file was built
 * from, and asserts deep equality against what's actually committed -- so editing a schema or the
 * registry without running `pnpm openapi:agent` fails CI here, not silently.
 */

const COMMITTED_PATH = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../../../../../../docs/agents/openapi.agent.yaml'
);

describe('docs/agents/openapi.agent.yaml drift', () => {
	it('matches the document generated from src/lib/server/agent-api-spec/', () => {
		const committedRaw = readFileSync(COMMITTED_PATH, 'utf8');
		const committedDoc = parse(committedRaw);
		const freshDoc = generateAgentOpenApiDocument();

		try {
			expect(freshDoc).toEqual(committedDoc);
		} catch (err) {
			throw new Error(
				'docs/agents/openapi.agent.yaml is out of date with src/lib/server/agent-api-spec/ -- ' +
					'run `pnpm openapi:agent` and commit the result.\n\n' +
					(err instanceof Error ? err.message : String(err))
			);
		}
	});
});

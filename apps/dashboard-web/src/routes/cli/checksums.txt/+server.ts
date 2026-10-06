import { readFile } from 'node:fs/promises';
import path from 'node:path';
import type { RequestHandler } from './$types';
import { CLI_MISSING_MESSAGE, cliDir } from '$lib/server/cli-dist';

// GET /cli/checksums.txt — public. sha256sum format: `<hex>  sentinel-<os>-<arch>` per line.
export const GET: RequestHandler = async () => {
	let body: string;
	try {
		body = await readFile(path.join(cliDir(), 'checksums.txt'), 'utf-8');
	} catch {
		return new Response(CLI_MISSING_MESSAGE, { status: 503, headers: { 'content-type': 'text/plain; charset=utf-8' } });
	}
	return new Response(body, {
		headers: {
			'content-type': 'text/plain; charset=utf-8',
			'cache-control': 'public, max-age=0, must-revalidate',
		},
	});
};

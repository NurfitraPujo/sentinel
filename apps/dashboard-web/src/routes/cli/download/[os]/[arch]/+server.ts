import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import path from 'node:path';
import { Readable } from 'node:stream';
import type { RequestHandler } from './$types';
import { CLI_MISSING_MESSAGE, cliBinaryName, cliDir, resolvePlatform } from '$lib/server/cli-dist';

const text = (body: string, status: number) =>
	new Response(body, { status, headers: { 'content-type': 'text/plain; charset=utf-8' } });

// GET /cli/download/<os>/<arch> — public. Only the four allow-listed pairs map to a file name; request
// input never reaches the filesystem path.
export const GET: RequestHandler = async ({ params }) => {
	const platform = resolvePlatform(params.os, params.arch);
	if (!platform) return text('unknown platform (supported: linux|darwin / amd64|arm64)\n', 404);

	const file = path.join(cliDir(), cliBinaryName(platform.os, platform.arch));
	let size: number;
	try {
		const st = await stat(file);
		if (!st.isFile()) throw new Error('not a file');
		size = st.size;
	} catch {
		return text(CLI_MISSING_MESSAGE, 503);
	}
	return new Response(Readable.toWeb(createReadStream(file)) as ReadableStream, {
		headers: {
			'content-type': 'application/octet-stream',
			'content-disposition': 'attachment; filename="sentinel"',
			'content-length': String(size),
			// Binary is immutable per image build but the URL is stable across deploys: revalidate.
			'cache-control': 'public, max-age=0, must-revalidate',
			'x-content-type-options': 'nosniff',
		},
	});
};

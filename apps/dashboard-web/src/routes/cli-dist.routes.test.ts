import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { mkdtempSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

// The CLI distribution routes are public (needed before login). These tests call the handlers directly
// with no session/auth mocks at all — if a handler ever started requiring auth it would throw here.
// That they are not swallowed by orgHandle for signed-in users is covered in hooks.server.test.ts and
// tests/route-manifest-drift.test.ts.

import { GET as getInstall } from './install.sh/+server';
import { GET as getDownload } from './cli/download/[os]/[arch]/+server';
import { GET as getChecksums } from './cli/checksums.txt/+server';
import { publicOrigin } from '$lib/server/cli-dist';

let dir: string;
const BIN = 'fake-binary-bytes';

beforeAll(() => {
	dir = mkdtempSync(path.join(tmpdir(), 'cli-dist-'));
	writeFileSync(path.join(dir, 'sentinel-linux-amd64'), BIN);
	writeFileSync(path.join(dir, 'checksums.txt'), 'abc123  sentinel-linux-amd64\n');
});
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const withDir = (d: string) => vi.stubEnv('SENTINEL_CLI_DIR', d);
const dl = (os: string, arch: string) => getDownload({ params: { os, arch } } as any);

describe('GET /install.sh', () => {
	it('renders its own origin and is a valid POSIX script', async () => {
		vi.stubEnv('AUTH_URL', '');
		const res = await getInstall({ url: new URL('https://sentinel.example.com/install.sh') } as any);
		expect(res.status).toBe(200);
		expect(res.headers.get('content-type')).toContain('text/x-shellscript');
		const body = await res.text();
		expect(body.startsWith('#!/bin/sh\n')).toBe(true);
		expect(body).toContain("SENTINEL_ORIGIN='https://sentinel.example.com'");
		expect(body).toContain('sentinel login --url $SENTINEL_ORIGIN');
		// sentinel-cli-browser-login: the next step is the browser login -- no admin, no key to paste.
		expect(body).toContain('approve the code in your browser');
		expect(body).not.toContain('paste your agent key');
		const f = path.join(dir, 'install.sh');
		writeFileSync(f, body);
		execFileSync('sh', ['-n', f]); // throws on a syntax error
		// main() wrapper: the last non-empty line invokes it, so a truncated download runs nothing.
		expect(body.trimEnd().split('\n').pop()).toBe('main "$@"');
		expect(body).toMatch(/trap '[^']*\.sentinel\.new\.\$\$[^']*' EXIT INT TERM/);
		const lines = body.split('\n');
		for (const cut of [Math.floor(lines.length / 3), Math.floor(lines.length / 2), lines.length - 3]) {
			const trunc = path.join(dir, 'trunc.sh');
			writeFileSync(trunc, lines.slice(0, cut).join('\n'));
			let out = '';
			try {
				out = execFileSync('sh', [trunc], { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'pipe'], env: { PATH: '/usr/bin:/bin', HOME: dir } });
			} catch (e: any) {
				out = String(e.stdout ?? '');
			}
			expect(out).not.toContain('Downloading');
		}
	});

	it('prefers the configured AUTH_URL over a (possibly forged) request origin', async () => {
		vi.stubEnv('AUTH_URL', 'https://sentinel.example.com');
		const res = await getInstall({ url: new URL('http://evil.example.net/install.sh') } as any);
		const body = await res.text();
		expect(body).toContain("SENTINEL_ORIGIN='https://sentinel.example.com'");
		expect(body).not.toContain('evil.example.net');
		vi.unstubAllEnvs();
	});

	it('only ever yields a bare http(s) origin that is shell-safe', () => {
		// path/query are dropped by URL.origin, so injected shell text never survives
		expect(publicOrigin("http://x.example/'; rm -rf ~; '", undefined)).toBe('http://x.example');
		expect(publicOrigin("http://x'y.example", undefined)).toBeNull();
		expect(publicOrigin('javascript:alert(1)', undefined)).toBeNull();
		expect(publicOrigin('http://localhost:3000', undefined)).toBe('http://localhost:3000');
	});
});

describe('GET /install.sh — profile', () => {
	it('prints --profile from SENTINEL_CLI_PROFILE and omits it when unset or invalid', async () => {
		vi.stubEnv('AUTH_URL', 'https://sentinel.staging.example.com');
		const render = async () =>
			(await (await getInstall({ url: new URL('https://x.example/install.sh') } as any)).text());
		vi.stubEnv('SENTINEL_CLI_PROFILE', 'staging');
		expect(await render()).toContain('sentinel login --url $SENTINEL_ORIGIN --profile staging');
		vi.stubEnv('SENTINEL_CLI_PROFILE', '');
		const none = await render();
		expect(none).toContain('sentinel login --url $SENTINEL_ORIGIN ');
		expect(none).not.toContain('--profile');
		vi.stubEnv('SENTINEL_CLI_PROFILE', "x'; rm -rf ~");
		expect(await render()).not.toContain('--profile');
		vi.unstubAllEnvs();
	});
});

describe('GET /cli/download/<os>/<arch>', () => {
	it('streams the binary with the right headers', async () => {
		withDir(dir);
		const res = await dl('linux', 'amd64');
		expect(res.status).toBe(200);
		expect(res.headers.get('content-type')).toBe('application/octet-stream');
		expect(res.headers.get('content-disposition')).toBe('attachment; filename="sentinel"');
		expect(res.headers.get('cache-control')).toContain('must-revalidate');
		expect(await res.text()).toBe(BIN);
	});

	it.each([
		['windows', 'amd64'],
		['linux', '386'],
		['..', 'amd64'],
		['linux', '../../etc/passwd'],
		['linux/../..', 'amd64'],
		['LINUX', 'amd64'],
		['', ''],
	])('404s %j/%j (allow-list, no traversal)', async (os, arch) => {
		withDir(dir);
		expect((await dl(os, arch)).status).toBe(404);
	});

	it('503s with build instructions when the file is absent (allow-listed platform)', async () => {
		withDir(dir); // darwin binaries were never written here
		const res = await dl('darwin', 'arm64');
		expect(res.status).toBe(503);
		expect(await res.text()).toContain('Dockerfile');
	});

	it('503s when the CLI dir does not exist at all', async () => {
		withDir(path.join(dir, 'nope'));
		expect((await dl('linux', 'amd64')).status).toBe(503);
	});
});

describe('GET /cli/checksums.txt', () => {
	it('serves the checksum file', async () => {
		withDir(dir);
		const res = await getChecksums({} as any);
		expect(res.status).toBe(200);
		expect(await res.text()).toBe('abc123  sentinel-linux-amd64\n');
	});
	it('503s when absent', async () => {
		const empty = path.join(dir, 'empty');
		mkdirSync(empty);
		withDir(empty);
		expect((await getChecksums({} as any)).status).toBe(503);
	});
});

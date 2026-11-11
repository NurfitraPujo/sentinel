import { describe, it, expect } from 'vitest';
import { validateWebhookUrl } from './agent-webhook-url';

describe('validateWebhookUrl', () => {
	it.each([
		'https://example.com/hook',
		'https://example.com:8443/hook?x=1',
		'http://localhost/hook',
		'http://localhost:3000/hook',
		'http://127.0.0.1/hook',
		'http://127.0.0.1:9000/hook',
		'https://localhost/hook',
	])('accepts %s', (url) => {
		expect(validateWebhookUrl(url)).toEqual({ valid: true });
	});

	it('rejects a malformed URL', () => {
		expect(validateWebhookUrl('not a url').valid).toBe(false);
	});

	it('rejects plain http:// for a non-localhost host', () => {
		const result = validateWebhookUrl('http://example.com/hook');
		expect(result.valid).toBe(false);
		expect(result.error).toMatch(/https/);
	});

	it('rejects a non-http(s) scheme', () => {
		const result = validateWebhookUrl('ftp://example.com/hook');
		expect(result.valid).toBe(false);
	});

	it('rejects userinfo in the URL', () => {
		const result = validateWebhookUrl('https://user:pass@example.com/hook');
		expect(result.valid).toBe(false);
		expect(result.error).toMatch(/userinfo/);
	});

	it.each([
		['10.0.0.5', '10.x private range'],
		['10.255.255.255', '10.x private range upper bound'],
		['172.16.0.1', '172.16-31 private range lower bound'],
		['172.31.255.255', '172.16-31 private range upper bound'],
		['192.168.1.1', '192.168 private range'],
		['169.254.1.1', 'link-local'],
		['127.0.0.2', 'loopback other than 127.0.0.1'],
	])('rejects literal private/loopback IPv4 %s (%s) over https', (ip) => {
		const result = validateWebhookUrl(`https://${ip}/hook`);
		expect(result.valid).toBe(false);
	});

	it('rejects 172.15.x.x and 172.32.x.x (just outside the private range)', () => {
		expect(validateWebhookUrl('https://172.15.0.1/hook').valid).toBe(true);
		expect(validateWebhookUrl('https://172.32.0.1/hook').valid).toBe(true);
	});

	it.each([['https://[::1]/hook', 'IPv6 loopback'], ['https://[fc00::1]/hook', 'fc00::/7 unique local'], ['https://[fd12::1]/hook', 'fd.. unique local']])(
		'rejects literal private/loopback IPv6 %s (%s)',
		(url) => {
			const result = validateWebhookUrl(url);
			expect(result.valid).toBe(false);
		}
	);

	it.each([
		['https://[::ffff:127.0.0.1]/hook', 'IPv4-mapped IPv6 loopback (dotted form)'],
		['https://[::ffff:7f00:1]/hook', 'IPv4-mapped IPv6 loopback (hex form)'],
		['https://[::ffff:169.254.169.254]/hook', 'IPv4-mapped IPv6 link-local (cloud metadata)'],
		['https://[::ffff:10.0.0.5]/hook', 'IPv4-mapped IPv6 10.x private range'],
		['https://[::ffff:192.168.1.1]/hook', 'IPv4-mapped IPv6 192.168 private range'],
		['https://0.0.0.0/hook', 'unspecified IPv4 address'],
		['https://[::]/hook', 'unspecified IPv6 address'],
	])('rejects %s (%s)', (url) => {
		const result = validateWebhookUrl(url);
		expect(result.valid).toBe(false);
	});
});

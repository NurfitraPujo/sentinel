import { describe, it, expect } from 'vitest';
import { approveRequestIsSameOrigin, cliApproveSignInUrl, parseApproveOrganizationId, safeCliApproveNext } from './cli-approve';
import { cleanProfile, cleanText, formatUserCode, generateUserCode, normalizeUserCode, USER_CODE_ALPHABET } from './cli-device';

describe('safeCliApproveNext (the /signin?next= allow-list)', () => {
	it.each(['/cli/approve', '/cli/approve?code=BCDF-GHJK', '/cli/approve?code=BCDF%2DGHJK'])('accepts %s', (next) => {
		expect(safeCliApproveNext(next)).toMatch(/^\/cli\/approve(\?code=BCDF-GHJK)?$/);
	});

	it.each([
		'https://evil.example/cli/approve',
		'//evil.example/cli/approve',
		'/\\evil.example',
		'/cli/approve/../../evil',
		'/cli/approve?code=BCDF-GHJK&next=https://evil.example',
		'/cli/approve?code=AAAA-AAAA',
		'/cli/approvex',
		// Malformed escapes: decodeURIComponent throws on these; they must be refused, not 500 the sign-in page.
		'/cli/approve?code=%',
		'/cli/approve?code=%E0%A4%A',
		'/cli/approve?code=BCDF-GHJK%',
		'/',
		'',
		null,
		42,
	])('refuses %s', (next) => {
		expect(safeCliApproveNext(next)).toBeNull();
	});

	it('builds the sign-in URL the approval page links to', () => {
		expect(cliApproveSignInUrl('BCDFGHJK')).toBe('/signin?next=%2Fcli%2Fapprove%3Fcode%3DBCDF-GHJK');
		expect(cliApproveSignInUrl(null)).toBe('/signin?next=%2Fcli%2Fapprove');
		expect(safeCliApproveNext(decodeURIComponent(cliApproveSignInUrl('BCDFGHJK').split('next=')[1]))).toBe('/cli/approve?code=BCDF-GHJK');
	});
});

describe('approveRequestIsSameOrigin', () => {
	const req = (origin?: string) => new Request('http://dash/cli/approve', { method: 'POST', headers: origin ? { origin } : {} });
	it('requires an Origin equal to the server origin', () => {
		expect(approveRequestIsSameOrigin(req('http://dash'), 'http://dash')).toBe(true);
		expect(approveRequestIsSameOrigin(req('http://evil'), 'http://dash')).toBe(false);
		expect(approveRequestIsSameOrigin(req(), 'http://dash')).toBe(false);
	});
});

describe('user codes', () => {
	it('are 8 letters of the unambiguous consonant alphabet', () => {
		for (let i = 0; i < 200; i++) {
			const c = generateUserCode();
			expect(c).toMatch(/^[BCDFGHJKLMNPQRSTVWXZ]{8}$/);
		}
		expect(USER_CODE_ALPHABET).not.toMatch(/[AEIOU0-9]/);
	});

	it('normalize what people type and refuse anything else', () => {
		expect(normalizeUserCode('bcdf-ghjk')).toBe('BCDFGHJK');
		expect(normalizeUserCode(' BCDF GHJK ')).toBe('BCDFGHJK');
		for (const bad of ['BCDF-GHJ', 'BCDF-GHJKL', 'ABCD-EFGH', '1234-5678', "BCDF'GHJK", '', null, 'B'.repeat(40)]) {
			expect(normalizeUserCode(bad)).toBeNull();
		}
		expect(formatUserCode('BCDFGHJK')).toBe('BCDF-GHJK');
	});

	it('text cleaning strips control characters and bounds length; profiles follow the CLI rule', () => {
		expect(cleanText('a\u0000b\u001b[1mc d', 100)).toBe('a b [1mc d');
		expect(cleanText('x'.repeat(10), 4)).toBe('xxxx');
		expect(cleanText(5, 4)).toBe('');
		expect(cleanProfile('production')).toBe('production');
		expect(cleanProfile('Prod')).toBe('');
	});
});

describe('parseApproveOrganizationId', () => {
	it('accepts a UUID and refuses everything else, so nothing malformed reaches a uuid column', () => {
		expect(parseApproveOrganizationId('0E2E0000-0000-4000-8000-000000000001')).toBe('0E2E0000-0000-4000-8000-000000000001');
		for (const bad of ['org-1', '', ' 0e2e0000-0000-4000-8000-000000000001', '0e2e0000000040008000000000000001', null, undefined, 7]) {
			expect(parseApproveOrganizationId(bad)).toBeNull();
		}
	});
});

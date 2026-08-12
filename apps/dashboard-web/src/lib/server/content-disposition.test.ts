import { describe, it, expect } from 'vitest';
import { buildContentDisposition } from './content-disposition';

// R3 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a Japanese filename previously reached
// `headers.set('Content-Disposition', \`attachment; filename="${safeFilename}"\`)` unchanged
// except for a `["\r\n]` strip -- Node's Headers implementation throws on a non-ISO-8859-1 value,
// a permanent 500. This proves the fix builds a header value `new Headers().set(...)` accepts,
// with both the ASCII fallback and the RFC 5987 extended parameter.
describe('buildContentDisposition (R3)', () => {
	it('never throws when Headers.set consumes it, for a non-ASCII filename', () => {
		const value = buildContentDisposition('attachment', 'スクショ.png');
		expect(() => new Headers().set('Content-Disposition', value)).not.toThrow();
	});

	it('includes an ASCII fallback filename= and an RFC 5987 filename*=UTF-8\'\' parameter', () => {
		const value = buildContentDisposition('attachment', 'スクショ.png');
		expect(value).toMatch(/filename="_+\.png"/); // non-ASCII characters stripped in the fallback
		expect(value).toContain(`filename*=UTF-8''${encodeURIComponent('スクショ.png')}`);
	});

	it('strips backslashes from both the fallback and the extended value', () => {
		const value = buildContentDisposition('attachment', 'evil\\name.png');
		expect(value).not.toContain('\\');
		expect(() => new Headers().set('Content-Disposition', value)).not.toThrow();
	});

	it('preserves a plain ASCII filename unchanged in the fallback', () => {
		const value = buildContentDisposition('inline', 'report.pdf');
		expect(value).toBe(`inline; filename="report.pdf"; filename*=UTF-8''report.pdf`);
	});
});

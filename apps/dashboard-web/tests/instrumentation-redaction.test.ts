// Regression coverage for Defect 1: HttpInstrumentation records the raw incoming query string as the
// `url.query` span attribute, and this app's Auth.js magic-link callback
// (/auth/callback/email?token=...&email=...) carries a live login token and the user's email in exactly
// that query string. Reproduced: curl against that URL produced a span with
// `url.query = "token=MAGICLINKSECRET123&email=victim%40example.com"`. redactQueryString is the pure
// function instrumentation.mjs uses (via its requestHook) to overwrite that attribute before export —
// asserted here directly since the redaction itself is the security-relevant part, independent of how
// OTel wires the hook.
//
// Importing instrumentation.mjs here does run its top-level bootstrap code, but in this test process
// neither OTEL_EXPORTER_OTLP_ENDPOINT nor OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set, so it takes the
// "tracing disabled" branch (one log line, no SDK, no signal handlers, no network) — see
// instrumentation.mjs's own degradation comment.
import { describe, expect, it } from 'vitest';
import { redactQueryString } from '../instrumentation.mjs';

describe('redactQueryString', () => {
	it('replaces every value while keeping key names, for a magic-link token + email', () => {
		const raw = 'token=MAGICLINKSECRET123&email=victim%40example.com';

		const redacted = redactQueryString(raw);

		expect(redacted).not.toContain('MAGICLINKSECRET123');
		expect(redacted).not.toContain('victim');
		expect(redacted).not.toContain('example.com');
		expect(redacted).toBe('token=REDACTED&email=REDACTED');
	});

	it('preserves key order and duplicate keys, redacting each value independently', () => {
		expect(redactQueryString('a=1&a=2&b=3')).toBe('a=REDACTED&a=REDACTED&b=REDACTED');
	});

	it('leaves a bare flag with no "=" untouched — nothing to redact', () => {
		expect(redactQueryString('debug')).toBe('debug');
	});

	it('is a no-op on an empty or absent query string', () => {
		expect(redactQueryString('')).toBe('');
		expect(redactQueryString(undefined)).toBe(undefined);
		expect(redactQueryString(null)).toBe(null);
	});

	it('redacts a value that itself contains "=" (base64url-ish token) without truncating the key', () => {
		expect(redactQueryString('token=abc=def')).toBe('token=REDACTED');
	});
});

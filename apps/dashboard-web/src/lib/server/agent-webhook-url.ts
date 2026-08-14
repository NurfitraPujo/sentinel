/**
 * N3a: URL validation for agent webhook registration (server-side only -- routes call this before
 * ever persisting a URL via createAgentWebhook/updateAgentWebhook).
 *
 * Rules:
 *  - https:// is required, EXCEPT http://localhost and http://127.0.0.1 (and their :port forms),
 *    which are allowed to support local development against the delivery worker.
 *  - Any URL whose hostname is a literal private/loopback/link-local IP is rejected outright
 *    (SSRF hardening: an attacker registering a webhook could otherwise point delivery at the
 *    dashboard's own internal network).
 *  - userinfo (`user:pass@host`) in the URL is rejected -- it has no legitimate use for a webhook
 *    endpoint and is a classic URL-parsing confusion vector.
 *
 * Explicitly OUT OF SCOPE for v1: DNS rebinding (a hostname that resolves to a public IP at
 * validation time but a private one at delivery time). Closing that requires the delivery worker
 * itself to re-resolve and re-check the IP immediately before each connection (and pin it), which
 * is delivery-path work, not registration-path work -- tracked separately, not solved here.
 */

const LOCALHOST_HOSTS = new Set(['localhost', '127.0.0.1']);

// String-prefix/pattern checks against literal IPv4/IPv6 private, loopback, and link-local
// ranges. Deliberately NOT a DNS lookup (see the DNS-rebinding note above) -- this only catches
// the literal-IP case.
function isPrivateOrLinkLocalIp(hostname: string): boolean {
	// URL#hostname returns IPv6 literals wrapped in brackets (e.g. "[::1]") -- strip them so the
	// checks below match the bare address.
	let h = hostname.toLowerCase().replace(/^\[|\]$/g, '');

	// Unspecified address (dotted or IPv6 "::") -- not routable, but resolvable/bindable in ways
	// that are dangerous in an SSRF context.
	if (h === '0.0.0.0' || h === '::') return true;

	// IPv6 loopback / unique local (fc00::/7 covers fc.. and fd..).
	if (h === '::1' || h.startsWith('fc') || h.startsWith('fd')) {
		// Only treat as fc00::/7 if it actually looks like an IPv6 literal (contains ':').
		if (h.includes(':')) return true;
	}

	// IPv4-mapped IPv6 (::ffff:a.b.c.d, either dotted-quad or fully-hex form like ::ffff:7f00:1).
	// URL#hostname normalizes bracketed literals to lowercase hex, so "[::ffff:127.0.0.1]" becomes
	// "::ffff:7f00:1" here -- unwrap it back to a dotted-quad and re-run the IPv4 checks below.
	if (h.startsWith('::ffff:')) {
		const rest = h.slice('::ffff:'.length);
		if (/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(rest)) {
			h = rest;
		} else {
			const hexMatch = rest.match(/^([0-9a-f]{1,4}):([0-9a-f]{1,4})$/);
			if (hexMatch) {
				const word1 = parseInt(hexMatch[1], 16);
				const word2 = parseInt(hexMatch[2], 16);
				h = [
					(word1 >> 8) & 0xff,
					word1 & 0xff,
					(word2 >> 8) & 0xff,
					word2 & 0xff
				].join('.');
			}
		}
	}

	// IPv4 ranges as simple string/octet checks.
	if (/^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h)) return true;
	if (/^192\.168\.\d{1,3}\.\d{1,3}$/.test(h)) return true;
	if (/^169\.254\.\d{1,3}\.\d{1,3}$/.test(h)) return true;
	const m172 = h.match(/^172\.(\d{1,3})\.\d{1,3}\.\d{1,3}$/);
	if (m172) {
		const second = Number(m172[1]);
		if (second >= 16 && second <= 31) return true;
	}
	// 127.0.0.0/8 other than the explicitly-allowed 127.0.0.1 is still loopback.
	if (/^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h)) return true;

	return false;
}

export interface WebhookUrlValidationResult {
	valid: boolean;
	error?: string;
}

export function validateWebhookUrl(rawUrl: string): WebhookUrlValidationResult {
	let parsed: URL;
	try {
		parsed = new URL(rawUrl);
	} catch {
		return { valid: false, error: 'url must be a valid URL' };
	}

	if (parsed.username || parsed.password) {
		return { valid: false, error: 'url must not contain userinfo (user:pass@host)' };
	}

	const hostname = parsed.hostname;
	const isLocalDev = LOCALHOST_HOSTS.has(hostname.toLowerCase());

	if (parsed.protocol === 'http:') {
		if (!isLocalDev) {
			return { valid: false, error: 'url must use https:// (http:// is only allowed for localhost/127.0.0.1)' };
		}
		// http://localhost and http://127.0.0.1 are allowed for dev even though they are also
		// technically "loopback" -- fall through without the private-IP rejection below.
		return { valid: true };
	}

	if (parsed.protocol !== 'https:') {
		return { valid: false, error: 'url must use https://' };
	}

	if (isPrivateOrLinkLocalIp(hostname)) {
		return { valid: false, error: 'url must not point at a private, loopback, or link-local address' };
	}

	return { valid: true };
}

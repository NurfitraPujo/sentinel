import { describe, expect, it } from 'vitest';
import { clientIp } from './client-ip';

// sentinel-cli-browser-login (security review, repair round 1): behind ALB -> Envoy the per-IP limits and the approval
// page's "requesting IP" need adapter-node's ADDRESS_HEADER/XFF_DEPTH, and with those set getClientAddress() THROWS when
// the header is missing or short. clientIp must degrade to the socket peer, never throw, and never collapse callers into
// one shared '' bucket.
const peer = (remoteAddress?: string) => ({ req: { socket: { remoteAddress } } });

describe('clientIp', () => {
	it('uses what adapter-node resolved (the X-Forwarded-For entry at XFF_DEPTH)', () => {
		expect(clientIp({ getClientAddress: () => ' 203.0.113.7 ', platform: peer('10.0.0.9') })).toBe('203.0.113.7');
	});

	it('falls back to the socket peer when adapter-node throws (header absent, or fewer entries than XFF_DEPTH)', () => {
		const throwing = () => {
			throw new Error('Address header was specified with ADDRESS_HEADER=x-forwarded-for but is absent from request');
		};
		expect(clientIp({ getClientAddress: throwing, platform: peer('10.0.0.9') })).toBe('10.0.0.9');
	});

	it("never throws and never returns '' -- 'unknown' when there is no peer either", () => {
		const throwing = () => {
			throw new Error('XFF_DEPTH is 2, but only found 1 addresses');
		};
		expect(clientIp({ getClientAddress: throwing })).toBe('unknown');
		expect(clientIp({ getClientAddress: () => '', platform: peer(undefined) })).toBe('unknown');
	});

	it('bounds the value it returns', () => {
		expect(clientIp({ getClientAddress: () => 'x'.repeat(500) })).toHaveLength(64);
	});
});

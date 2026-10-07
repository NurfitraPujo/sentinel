import type { RequestEvent } from '@sveltejs/kit';

/**
 * The caller's IP for rate limits, audit rows and the `sentinel login` approval page.
 *
 * `getClientAddress()` is adapter-node's resolution, configured by `ADDRESS_HEADER` / `XFF_DEPTH` (helm
 * `config.dashboard.clientIp`). Behind a proxy without those, it is the proxy's address -- every caller then shares one
 * per-IP bucket. With them set, adapter-node THROWS when the header is absent or holds fewer entries than `XFF_DEPTH`
 * (a pod-network request that skipped the load balancer, a health probe). Throwing there would turn a limiter into a
 * 500, and an empty string would put all such callers in one shared bucket, so this falls back to the TCP peer
 * (`platform.req.socket`, which adapter-node provides) and finally to the literal 'unknown'. It never throws.
 */
export function clientIp(event: Pick<RequestEvent, 'getClientAddress'> & { platform?: unknown }): string {
	try {
		const resolved = event.getClientAddress();
		if (resolved) return resolved.trim().slice(0, 64);
	} catch {
		// fall through to the socket peer
	}
	const req = (event.platform as { req?: { socket?: { remoteAddress?: string } } } | undefined)?.req;
	const peer = req?.socket?.remoteAddress;
	return peer ? peer.slice(0, 64) : 'unknown';
}

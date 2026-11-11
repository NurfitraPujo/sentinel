// sentinel-sentry-compat-ingest spec §3.4 / §8.2: the numeric Sentry project id of a Sentinel project.
// Core's mobile relay is configured with it, and the ingestor's Sentry-compatible routes accept a request
// only when its path id equals the one derived from the presenting key's project.

const CANONICAL_UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const COMPACT_UUID = /^[0-9a-fA-F]{32}$/;

/**
 * §3.4's derivation: the decimal value of the first 13 hex digits of a project UUID, dashes removed. Throws on
 * anything that is not a 32-hex or canonical 8-4-4-4-12 UUID. Must equal auth.SentryProjectID (Go) on
 * tests/fixtures/sentry/project-id-vectors.json.
 *
 * 13 hex digits are 52 bits, below 2^53, so Number.parseInt holds the value exactly.
 */
export function sentryProjectId(uuid: string): string {
	let hex: string;
	if (CANONICAL_UUID.test(uuid)) {
		hex = uuid.replaceAll('-', '');
	} else if (COMPACT_UUID.test(uuid)) {
		hex = uuid;
	} else {
		throw new Error('sentryProjectId: not a UUID');
	}
	return String(Number.parseInt(hex.slice(0, 13).toLowerCase(), 16));
}

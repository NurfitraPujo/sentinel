/**
 * sentinel-reporting-and-dashboard D14: "No query interpolates a user-supplied string."
 *
 * Databend is queried over its HTTP API with SQL text, so there are no bound parameters. Every value that reaches
 * SQL therefore goes through one of the functions below, which either return a safe literal or throw. Ids must
 * match the UUID pattern; enums come from allow-lists; timestamps are server-generated (a Date, never a string from
 * the request); a free string (an environment or service name) must be in the caller-supplied allow-list AND is
 * escaped. Nothing else builds SQL.
 */

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export class LiteralError extends Error {}

/** A UUID as a quoted SQL literal. */
export function uuidLiteral(v: string): string {
	if (!UUID_RE.test(v)) throw new LiteralError(`not a uuid: ${JSON.stringify(v).slice(0, 80)}`);
	return `'${v.toLowerCase()}'`;
}

export function isUuid(v: unknown): v is string {
	return typeof v === 'string' && UUID_RE.test(v);
}

/** A string that MUST be one of `allowed`, as a quoted literal (single quotes and backslashes escaped regardless). */
export function enumLiteral(v: string, allowed: readonly string[]): string {
	if (!allowed.includes(v)) throw new LiteralError(`not an allowed value: ${JSON.stringify(v).slice(0, 80)}`);
	return `'${v.replace(/\\/g, '\\\\').replace(/'/g, "''")}'`;
}

/** An `IN (...)` list of allow-listed strings. An empty list is the always-false predicate's job, not this one's. */
export function enumList(values: readonly string[], allowed: readonly string[]): string {
	if (values.length === 0) throw new LiteralError('empty enum list');
	return values.map((v) => enumLiteral(v, allowed)).join(', ');
}

/** A server-generated instant as a Databend timestamp literal (UTC, second precision). */
export function tsLiteral(d: Date): string {
	if (!(d instanceof Date) || Number.isNaN(d.getTime())) throw new LiteralError('invalid Date');
	return `'${d.toISOString().slice(0, 19).replace('T', ' ')}'`;
}

/** A positive integer as a literal (limits, bucket sizes). */
export function intLiteral(n: number, max = 1_000_000): string {
	if (!Number.isInteger(n) || n < 0 || n > max) throw new LiteralError(`not a bounded integer: ${n}`);
	return String(n);
}

import { RANGES, type Range } from '../../dashboard-format';
export { RANGES, type Range };
export const RANGE_MS: Record<Range, number> = {
	'24h': 24 * 3_600_000,
	'7d': 7 * 24 * 3_600_000,
	'28d': 28 * 24 * 3_600_000,
	'90d': 90 * 24 * 3_600_000,
};

/** `range` is an allow-listed enum; anything else is the default. */
export function coerceRange(v: string | null | undefined): Range {
	return (RANGES as readonly string[]).includes(v ?? '') ? (v as Range) : '7d';
}

// sentinel-occurrence-detail-panels spec §3.3 / §12.4: re-measure the issue page's occurrence payload.
//
//   node sentinel-occurrence-detail-panels/measure-payload.mjs            (from the repository root)
//
// Needs Node >= 18 and nothing else. It prints:
//   1. the fixture table: payload-fixture.mjs's FLUTTER_* rows, JSON.stringify UTF-8 bytes;
//   2. the ingest-bounds maxima (apps/ingestor-go/validation/bounds.go:20-92) of every heavy column,
//      and the worst-case light payload of 100 listed rows;
//   3. the same fixture sizes through devalue.stringify, which is what SvelteKit sends, when devalue can
//      be found, tried in this order: DEVALUE_PATH=/path/to/devalue/index.js; apps/dashboard-web's own
//      devalue devDependency (spec §2, added by this feature); the devalue @sveltejs/kit resolves (works
//      on any checkout after `pnpm install` in apps/dashboard-web, before the devDependency exists).
//      Without any of them that section says so and the rest still runs.
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import {
	FLUTTER_OCCURRENCE,
	FLUTTER_DETAIL,
	LIGHT_KEYS,
	HEAVY_KEYS,
	PRE_V2_KEYS,
} from './payload-fixture.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const bytes = (v) => Buffer.byteLength(JSON.stringify(v), 'utf8');
const pick = (o, keys) => Object.fromEntries(keys.map((k) => [k, o[k]]));
const n = (x) => x.toLocaleString('en-US').replace(/,/g, ' ');
const pad = (s, w) => String(s).padStart(w);

// ---- 1. the fixture ------------------------------------------------------------------------------
const today = bytes(pick(FLUTTER_OCCURRENCE, PRE_V2_KEYS));
const light = bytes(pick(FLUTTER_OCCURRENCE, LIGHT_KEYS));
const heavy = bytes(pick(FLUTTER_DETAIL, HEAVY_KEYS));
// The detail read (§3.1) returns the whole row: the 18 list keys plus the six heavy ones.
const detailRow = bytes({ ...FLUTTER_OCCURRENCE, ...FLUTTER_DETAIL });
const perHeavy = HEAVY_KEYS.map((k) => `${k} ${n(bytes(FLUTTER_DETAIL[k]))}`).join(', ');

console.log('## 1. Fixture (FLUTTER_OCCURRENCE / FLUTTER_DETAIL), JSON.stringify UTF-8 bytes\n');
const rows = [
	['one listed row today (12 columns)', today],
	['the six light columns', light],
	[`the six heavy columns (${perHeavy})`, heavy],
	['the detail row (18 list keys + 6 heavy)', detailRow],
	['100 rows today', 100 * today],
	['100 rows x (today + light + heavy)', 100 * (today + light + heavy)],
	['100 rows x (today + light) + 1 detail row  <- this spec', 100 * (today + light) + detailRow],
];
for (const [label, b] of rows) console.log(`${pad(n(b), 11)}  ${label}`);
console.log(
	`\nthis spec vs today: +${(((100 * (today + light) + detailRow) / (100 * today) - 1) * 100).toFixed(1)} %;` +
		` all columns vs today: x${((100 * (today + light + heavy)) / (100 * today)).toFixed(1)}`
);

// ---- 2. ingest-bounds maxima -----------------------------------------------------------------------
const x = (len, c = 'x') => c.repeat(len);
const uniq = (i, len) => (String(i).padStart(3, '0') + x(len)).slice(0, len);

// Grow one string field until the serialised whole reaches `cap` bytes without passing it.
function fillTo(cap, build) {
	let lo = 0;
	let hi = cap;
	while (lo < hi) {
		const mid = Math.ceil((lo + hi) / 2);
		if (bytes(build(mid)) <= cap) lo = mid;
		else hi = mid - 1;
	}
	return build(lo);
}

const maxBreadcrumbs = fillTo(128 * 1024, (m) =>
	Array.from({ length: 100 }, (_, i) => ({
		timestamp: '2026-09-25T01:58:01.123456789Z',
		type: x(64),
		category: x(128),
		level: 'warning',
		message: x(Math.min(m, 8192)),
		data: { d: x(Math.max(0, Math.min(m - 8192, 4096 - 8))) },
	}))
);
const maxContexts = fillTo(64 * 1024, (m) =>
	Object.fromEntries(Array.from({ length: 32 }, (_, i) => [uniq(i, 64), { v: x(m) }]))
);
const maxRequest = {
	method: x(16),
	url: x(2048),
	query_string: x(4096),
	headers: Object.fromEntries(Array.from({ length: 64 }, (_, i) => [uniq(i, 256), x(4096)])),
	data: x(16 * 1024 - 2),
	env: Object.fromEntries(Array.from({ length: 64 }, (_, i) => [uniq(i, 256), x(1024)])),
};
const maxUser = {
	id: x(255),
	username: x(128),
	email: x(254),
	ip_address: x(45),
	geo: { city: x(128), region: x(128), subdivision: x(128), country_code: x(8) },
};
const maxTags = Object.fromEntries(Array.from({ length: 50 }, (_, i) => [uniq(i, 32), x(200)]));
const heavyMax = {
	breadcrumbs: bytes(maxBreadcrumbs),
	contexts: bytes(maxContexts),
	request: bytes(maxRequest),
	user: bytes(maxUser),
	tags: bytes(maxTags),
};
const heavyMaxTotal = Object.values(heavyMax).reduce((a, b) => a + b, 0);

console.log('\n## 2. Ingest-bounds maxima (bounds.go:20-92), ASCII, JSON bytes\n');
for (const [k, b] of Object.entries(heavyMax)) console.log(`${pad(n(b), 11)}  ${k}`);
console.log(`${pad(n(heavyMaxTotal), 11)}  heavy columns without exception, one occurrence`);
console.log('             exception: bounded only by the event body, <= 1 MiB single-event / <= 5 MiB batch body');
console.log(`${pad(n(100 * heavyMaxTotal), 11)}  x 100 rows, without exception ("all columns for all 100")`);

// Light columns, worst case per row. `level` is one of five values at ingest (T17): 'warning' is the longest.
const lightMax = (unit) => ({
	level: 'warning',
	message: x(10000, unit),
	serverName: x(256, unit),
	transaction: x(200, unit),
	dist: x(64, unit),
	sdk: { name: x(128, unit), version: x(64, unit) },
});
console.log('\nLight columns, 100 listed rows at their ingest maxima:');
for (const [label, unit] of [
	['ASCII (1 byte per code point)', 'x'],
	['4-byte UTF-8 (every code point outside the BMP)', '\u{1F600}'],
	['every code point a JSON-escaped control character (6 bytes)', '\u0001'],
]) {
	console.log(`${pad(n(100 * bytes(lightMax(unit))), 11)}  ${label}`);
}
console.log(`${pad(n(100 * 64 * 1024), 11)}  for comparison: today's metadata bound alone (64 KiB x 100), before its markers`);

// ---- 3. devalue ----------------------------------------------------------------------------------
async function loadDevalue() {
	const candidates = [];
	if (process.env.DEVALUE_PATH) candidates.push(pathToFileURL(process.env.DEVALUE_PATH).href);
	const dw = createRequire(join(here, '..', 'apps', 'dashboard-web', 'package.json'));
	try {
		// apps/dashboard-web's own devDependency (spec §2 / §11 add it, pinned to kit's version).
		candidates.push(pathToFileURL(dw.resolve('devalue')).href);
	} catch {
		// not a direct dependency yet: fall through
	}
	try {
		// Before that devDependency lands, pnpm links devalue only for @sveltejs/kit, its real
		// consumer, so resolve it from kit's own location: the exact copy SvelteKit serialises with.
		const kit = createRequire(dw.resolve('@sveltejs/kit/package.json'));
		candidates.push(pathToFileURL(kit.resolve('devalue')).href);
	} catch {
		// not installed: fall through
	}
	for (const c of candidates) {
		try {
			return await import(c);
		} catch {
			// try the next one
		}
	}
	return null;
}

const devalue = await loadDevalue();
console.log('\n## 3. devalue.stringify (what SvelteKit serialises)\n');
if (!devalue) {
	console.log('devalue not found: run `pnpm install` in apps/dashboard-web, or set DEVALUE_PATH.');
} else {
	const dv = (v) => Buffer.byteLength(devalue.stringify(v), 'utf8');
	const dvRows = [
		['one listed row today (12 columns)', dv(pick(FLUTTER_OCCURRENCE, PRE_V2_KEYS)), today],
		['the six light columns', dv(pick(FLUTTER_OCCURRENCE, LIGHT_KEYS)), light],
		['the six heavy columns', dv(pick(FLUTTER_DETAIL, HEAVY_KEYS)), heavy],
	];
	for (const [label, b, json] of dvRows) {
		console.log(`${pad(n(b), 11)}  ${label}  (${((b / json - 1) * 100).toFixed(1)} % vs JSON)`);
	}
	let threw = null;
	try {
		devalue.stringify(JSON.parse('{"tags":{"__proto__":"x"}}'));
	} catch (e) {
		threw = e.message;
	}
	console.log(`\ndevalue.stringify of a JSONB body with a "__proto__" key: ${threw ? `throws "${threw}"` : 'does not throw'}`);
}

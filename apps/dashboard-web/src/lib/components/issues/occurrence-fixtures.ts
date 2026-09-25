// sentinel-occurrence-detail-panels §12.2: the shared test fixtures. IMPORTED ONLY BY TESTS, never by
// app code. Rows are literal, in the shape the load returns (list row, light columns, detail row), and
// their JSONB bodies are in the stored snake_case (spec T9-T17).
//
// FLUTTER_OCCURRENCE / FLUTTER_DETAIL (and the helpers above them) are copied VERBATIM from
// sentinel-occurrence-detail-panels/payload-fixture.mjs: same keys, same values, types added only.
// M1 (occurrence-fixtures.test.ts) pins their size to spec §3.3, so the two copies cannot drift apart.

/* eslint-disable @typescript-eslint/no-explicit-any */
export type Row = Record<string, any>;
type Frame = Record<string, any>;
type Crumb = Record<string, any>;

export const OCCURRED_AT = '2026-09-25T02:00:00.000Z';
const T = Date.parse(OCCURRED_AT);

const APP_SOURCE = [
	'  Future<void> confirm(Order order) async {',
	'    final body = order.toJson();',
	'    final res = await _api.post(\'/v2/orders/${order.id}/confirm\', body);',
	'    if (res.statusCode != 200) {',
	'      _log.warning(\'confirm failed: ${res.statusCode}\');',
	'      throw CheckoutException(\'payment confirmation failed for order ${order.id}\', res);',
	'    }',
	'    _analytics.track(\'order_confirmed\', {\'id\': order.id});',
	'    await _cart.clear();',
	'    notifyListeners();',
	'  }',
];

/** Frame i of the Flutter stack. Frames 0-2 are app frames with source context; 5, 10, 15 are app frames without. */
function flutterFrame(i: number): Frame {
	if (i <= 2) {
		const line = [102, 57, 31][i];
		const file = ['lib/checkout/checkout_service.dart', 'lib/checkout/checkout_bloc.dart', 'lib/checkout/checkout_screen.dart'][i];
		const fn = ['CheckoutService.confirm', 'CheckoutBloc._onConfirm', '_CheckoutScreenState._submit'][i];
		return {
			file,
			line,
			function: fn,
			in_app: true,
			column: 7,
			module: 'package:daya/' + file.slice(4).replace(/\.dart$/, ''),
			abs_path: 'package:daya/' + file.slice(4),
			pre_context: APP_SOURCE.slice(0, 5),
			context_line: APP_SOURCE[5],
			post_context: APP_SOURCE.slice(6, 11),
		};
	}
	if (i === 5 || i === 10 || i === 15) {
		return {
			file: `lib/core/app_${i}.dart`,
			line: 20 + i,
			function: `AppShell.handler${i}`,
			in_app: true,
			column: 5,
			module: `package:daya/core/app_${i}`,
		};
	}
	return {
		file: i % 2 === 0 ? 'dart:async/zone.dart' : 'package:flutter/src/widgets/framework.dart',
		line: 1000 + i * 17,
		function: i % 2 === 0 ? '_rootRunUnary' : 'StatefulElement.performRebuild',
		in_app: false,
	};
}

function chainFrames(n: number, prefix: string): Frame[] {
	return Array.from({ length: n }, (_, i) => ({
		file: `lib/net/${prefix}_${i}.dart`,
		line: 10 + i,
		function: `${prefix}.step${i}`,
		in_app: i < 2,
	}));
}

const CATEGORIES = ['navigation', 'http', 'ui.click', 'console'];
const TYPES: Record<string, string> = { navigation: 'navigation', http: 'http', 'ui.click': 'user', console: 'debug' };

/** Crumb i of 100, one every 1.2 s, the last (i = 99) at the occurrence itself. */
function flutterCrumb(i: number): Crumb {
	const category = CATEGORIES[i % 4];
	const crumb: Crumb = {
		timestamp: new Date(T - (99 - i) * 1200).toISOString(),
		type: TYPES[category],
		category,
		level: category === 'console' ? 'debug' : category === 'http' && i % 8 === 1 ? 'error' : 'info',
	};
	if (category === 'navigation') {
		crumb.message = `/checkout/step-${i}`;
		crumb.data = { from: `/cart/${i}`, to: `/checkout/step-${i}` };
	} else if (category === 'http') {
		const failed = crumb.level === 'error';
		crumb.message = `POST https://api.daya.id/v2/orders/88412/confirm [${failed ? 502 : 200}]`;
		crumb.data = {
			method: 'POST',
			url: 'https://api.daya.id/v2/orders/88412/confirm',
			status_code: failed ? 502 : 200,
			reason: failed ? 'Bad Gateway' : 'OK',
		};
	} else if (category === 'ui.click') {
		crumb.message = 'ElevatedButton#confirm';
		crumb.data = { target: 'ElevatedButton#confirm', view: 'CheckoutScreen' };
	} else {
		crumb.message = `[cart] recalculated total, ${i} items`;
	}
	return crumb;
}

export const FLUTTER_OCCURRENCE: Row = {
	// the 12 pre-v2 list columns (spec T4)
	id: '0b7e3f2a-5c1d-4e8f-9a6b-3c2d1e0f9a8b',
	issueId: '6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b',
	environment: 'production',
	platform: 'dart',
	releaseVersion: '2.14.0+2140',
	stacktrace: Array.from({ length: 20 }, (_, i) => flutterFrame(i)),
	metadata: { user_id: 'u_29181', screen: 'CheckoutScreen', build_flavor: 'prod' },
	traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
	spanId: '00f067aa0ba902b7',
	eventId: 'c2a1f0e4-8b7d-4c3a-9e2f-1a0b9c8d7e6f',
	projectId: '9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d',
	createdAt: OCCURRED_AT,
	// the six light v2 columns (spec §3.1)
	level: 'error',
	message: 'CheckoutException: payment confirmation failed for order 88412',
	serverName: null,
	transaction: 'CheckoutScreen',
	dist: '2140',
	sdk: { name: 'sentinel.dart.flutter', version: '0.3.0' },
};

/** The six heavy columns (spec §3.1) plus `id`; the detail read's own row adds the 18 list keys (§3.1). */
export const FLUTTER_DETAIL: Row = {
	id: FLUTTER_OCCURRENCE.id as string,
	user: {
		id: 'u_29181',
		username: 'rina.s',
		email: 'rina@example.com',
		ip_address: '203.0.113.42',
		geo: { city: 'Jakarta', subdivision: 'Jakarta', region: 'Indonesia', country_code: 'ID' },
	},
	tags: {
		'app.version': '2.14.0',
		'device.family': 'Pixel',
		environment: 'production',
		flavor: 'prod',
		level: 'error',
		locale: 'id_ID',
		'os.name': 'Android',
		release: '2.14.0+2140',
		runtime: 'Dart',
		screen: 'CheckoutScreen',
	},
	contexts: {
		device: {
			type: 'device',
			name: 'Pixel 7',
			family: 'Pixel',
			model: 'Pixel 7',
			model_id: 'GVU6C',
			manufacturer: 'Google',
			brand: 'google',
			arch: 'arm64-v8a',
			simulator: false,
			orientation: 'portrait',
			screen_width_pixels: 1080,
			screen_height_pixels: 2400,
			screen_density: 2.625,
			memory_size: 8000000000,
			free_memory: 2100000000,
			battery_level: 81,
			charging: false,
			online: true,
			low_memory: false,
			processor_count: 8,
			custom_field: 'custom value',
		},
		os: { type: 'os', name: 'Android', version: '14', build: 'UQ1A.240205.004', kernel_version: '5.10.198', rooted: false },
		app: {
			type: 'app',
			app_name: 'Daya',
			app_version: '2.14.0',
			app_build: '2140',
			app_identifier: 'id.daya.app',
			build_type: 'release',
			app_start_time: '2026-09-25T01:58:10Z',
			in_foreground: true,
		},
		runtime: { type: 'runtime', name: 'Dart', version: '3.3.0 (stable)', raw_description: '3.3.0 (stable) on "android_arm64"' },
		culture: { type: 'culture', locale: 'id_ID', timezone: 'Asia/Jakarta', is_24_hour_format: true },
		trace: { type: 'trace', trace_id: '4bf92f3577b34da6a3ce929d0e0e4736', span_id: '00f067aa0ba902b7', op: 'ui.load', status: 'ok' },
		flutter: { has_render_view: true, renderer: 'Impeller' },
	},
	request: {
		method: 'POST',
		url: 'https://api.daya.id/v2/orders/88412/confirm',
		query_string: 'retry=1&source=app',
		headers: {
			Accept: 'application/json',
			Authorization: '***REDACTED***',
			'Content-Type': 'application/json',
			'User-Agent': 'Daya/2.14.0 (Android 14; Pixel 7)',
			'x-request-id': 'r-7f3a9c',
		},
		data: {
			order_id: 88412,
			amount: { currency: 'IDR', value: 249000 },
			items: [
				{ sku: 'SKU-1', qty: 1 },
				{ sku: 'SKU-2', qty: 2 },
			],
			card: '***REDACTED***',
		},
	},
	breadcrumbs: Array.from({ length: 100 }, (_, i) => flutterCrumb(i)),
	exception: {
		// stored oldest first (spec T12): the last value is the one raised last
		values: [
			{
				type: 'SocketException',
				value: 'Connection reset by peer (OS Error: errno = 104), address = api.daya.id, port = 443',
				mechanism: { type: 'chained', handled: true },
				stacktrace: chainFrames(6, 'Socket'),
			},
			{
				type: 'ApiException',
				value: 'POST /v2/orders/88412/confirm failed',
				mechanism: { type: 'chained', handled: true },
				stacktrace: chainFrames(8, 'ApiClient'),
			},
			{
				type: 'CheckoutException',
				value: 'payment confirmation failed for order 88412',
				mechanism: { type: 'generic', handled: false },
				// a fresh copy, not the same array: rows read from Postgres never share references, and devalue
				// deduplicates shared ones, which would understate section 3 of the measurement
				stacktrace: Array.from({ length: 20 }, (_, i) => flutterFrame(i)),
			},
		],
	},
};

export const LIGHT_KEYS = ['level', 'message', 'serverName', 'transaction', 'dist', 'sdk'] as const;
export const HEAVY_KEYS = ['user', 'tags', 'contexts', 'request', 'breadcrumbs', 'exception'] as const;
export const PRE_V2_KEYS = [
	'id', 'issueId', 'environment', 'platform', 'releaseVersion', 'stacktrace',
	'metadata', 'traceId', 'spanId', 'eventId', 'projectId', 'createdAt',
] as const;

/** The 17 v2 columns, by their Drizzle names (spec T7). */
export const V2_KEYS = [
	'level', 'message', 'errorClass', 'logger', 'serverName', 'transaction', 'dist', 'source',
	'traceFlags', 'receivedAt', 'sdk', 'eventUser', 'tags', 'contexts', 'request', 'breadcrumbs', 'exception',
] as const;

/**
 * The shape the real load returns: `createdAt` a `Date` (spec T29, review finding
 * odp-sr3-crumb-anchor-date). Fixtures stay ISO strings so M1 and payload-fixture.mjs are unchanged; a
 * test that must see the production shape wraps its rows in this.
 */
export function loaded<T extends Row>(row: T): T {
	return { ...row, createdAt: row.createdAt == null ? null : new Date(row.createdAt) };
}

/** A FLUTTER-shaped whole detail row (the 18 list keys plus the six heavy ones), as the detail read returns it. */
export function flutterDetailRow(id: string = FLUTTER_OCCURRENCE.id as string): Row {
	return { ...FLUTTER_OCCURRENCE, ...FLUTTER_DETAIL, id };
}

const ISSUE_ID = '6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b';
const PROJECT_ID = '9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d';

// ---------------------------------------------------------------------------------------------------
// Pre-v2 rows (spec §10): the 12 pre-v2 keys only; `in_app` is on every frame, as the processor has
// always stored it (T10), and one frame is symbolicated with `raw`, so the badge and raw toggle show.

export const PRE_V2_OCCURRENCE: Row = {
	id: '1a2b3c4d-0000-4000-8000-000000000001',
	issueId: ISSUE_ID,
	environment: 'production',
	platform: 'javascript',
	releaseVersion: '1.0.0',
	stacktrace: [
		{
			file: 'src/app/handler.ts',
			line: 12,
			column: 4,
			function: 'handle',
			in_app: true,
			symbolicated: true,
			raw: { file: 'https://cdn.example.com/app.min.js', line: 1, column: 5021, function: 'h' },
		},
		{ file: 'node_modules/express/lib/router/layer.js', line: 95, function: 'Layer.handle', in_app: false },
		{ file: 'node:internal/process/task_queues', line: 105, function: 'processTicksAndRejections', in_app: false },
	],
	metadata: { request_id: 'req-1', attempt: 2 },
	traceId: '0af7651916cd43dd8448eb211c80319c',
	spanId: 'b7ad6b7169203331',
	eventId: 'e0000000-0000-4000-8000-000000000001',
	projectId: PROJECT_ID,
	createdAt: '2026-09-24T10:15:30.000Z',
};

/** The same pre-v2 card, carrying all 17 v2 keys as `null`: a row written before the v2 migration. */
export const PRE_V2_OCCURRENCE_NULLS: Row = {
	...PRE_V2_OCCURRENCE,
	id: '1a2b3c4d-0000-4000-8000-000000000002',
	eventId: 'e0000000-0000-4000-8000-000000000002',
	metadata: {},
	traceId: null,
	spanId: null,
	createdAt: '2026-09-24T09:00:00.000Z',
	...Object.fromEntries(V2_KEYS.map((k) => [k, null])),
};

// ---------------------------------------------------------------------------------------------------
// A v2 event whose SDK sent nothing but the (defaulted) level.

export const V2_BARE_OCCURRENCE: Row = {
	id: '2b3c4d5e-0000-4000-8000-000000000001',
	issueId: ISSUE_ID,
	environment: 'production',
	platform: 'go',
	releaseVersion: null,
	stacktrace: [{ file: 'main.go', line: 10, function: 'main.main', in_app: true }],
	metadata: {},
	traceId: null,
	spanId: null,
	eventId: null,
	projectId: PROJECT_ID,
	createdAt: '2026-09-25T01:00:00.000Z',
	level: 'error',
	message: null,
	serverName: null,
	transaction: null,
	dist: null,
	sdk: null,
};

export const V2_BARE_DETAIL: Row = {
	...V2_BARE_OCCURRENCE,
	...Object.fromEntries(HEAVY_KEYS.map((k) => [k, null])),
};

// ---------------------------------------------------------------------------------------------------
// The §4.3 inventory: every event string E1-E38 the feature renders, each holding `fill(id)`.

export const INVENTORY_IDS = Array.from({ length: 38 }, (_, i) => `E${i + 1}`);

export function inventoryOccurrence(fill: (id: string) => string): { occurrence: Row; detail: Row } {
	const occurrence: Row = {
		id: '3c4d5e6f-0000-4000-8000-000000000001',
		issueId: ISSUE_ID,
		environment: 'production',
		platform: 'dart',
		releaseVersion: fill('E3'),
		stacktrace: [
			{
				function: fill('E9'),
				file: fill('E9'),
				line: 10,
				in_app: true,
				module: fill('E10'),
				pre_context: [fill('E11')],
				context_line: fill('E11'),
				post_context: [fill('E11')],
			},
		],
		metadata: {},
		traceId: null,
		spanId: null,
		eventId: fill('E5'),
		projectId: PROJECT_ID,
		createdAt: OCCURRED_AT,
		level: fill('E2'),
		message: fill('E1'),
		serverName: fill('E6'),
		transaction: fill('E7'),
		dist: fill('E4'),
		sdk: { name: fill('E8'), version: fill('E8') },
	};
	const chainValue = () => ({
		type: fill('E14'),
		value: fill('E15'),
		mechanism: { type: fill('E16'), handled: true },
		stacktrace: [{ function: fill('E9'), file: fill('E9'), line: 1, in_app: true }],
	});
	const detail: Row = {
		...occurrence,
		tags: { [fill('E12')]: fill('E13') },
		exception: { values: [chainValue(), chainValue()] },
		breadcrumbs: [
			{
				timestamp: fill('E17'),
				level: fill('E18'),
				category: fill('E19'),
				type: fill('E20'),
				message: fill('E21'),
				data: { url: fill('E22'), note: fill('E22') },
			},
		],
		request: {
			method: fill('E23'),
			url: fill('E24'),
			query_string: fill('E25'),
			headers: { [fill('E26')]: fill('E27') },
			data: fill('E28'),
		},
		user: {
			id: fill('E29'),
			username: fill('E30'),
			email: fill('E31'),
			ip_address: fill('E32'),
			geo: { city: fill('E33'), subdivision: fill('E33'), region: fill('E33'), country_code: fill('E33') },
		},
		contexts: {
			device: { model: fill('E35') },
			[fill('E34')]: { [fill('E36')]: fill('E37') },
			os: fill('E38'),
		},
	};
	return { occurrence, detail };
}

// Hostile content (spec §4.2, U1): markup in every field, and a clickable `javascript:` URL in every
// field that looks like a URL. The `#E…` suffix makes each field's text unique, so U1 can prove each
// one was RENDERED (as text), not merely that nothing executed.
export const PAYLOAD = '<script>alert(1)</script><img src=x onerror=alert(1)>';
export const URL_PAYLOAD = 'javascript:alert(1)//' + PAYLOAD;
export const URL_LIKE = new Set(['E9', 'E13', 'E22', 'E24', 'E27', 'E37']);
export const hostileFill = (id: string) => (URL_LIKE.has(id) ? URL_PAYLOAD : PAYLOAD) + '#' + id;

const hostile = inventoryOccurrence(hostileFill);
export const HOSTILE_OCCURRENCE: Row = hostile.occurrence;
export const HOSTILE_DETAIL: Row = {
	...hostile.detail,
	// two hostile KEYS beside the inventory, so a key/attribute sink is exercised too
	request: { ...hostile.detail.request, headers: { ...hostile.detail.request.headers, [PAYLOAD]: 'hostile-name' } },
	contexts: { ...hostile.detail.contexts, device: { ...hostile.detail.contexts.device, [PAYLOAD]: 'hostile-field' } },
};

// Over both limits (spec §4.3, TR1-TR8), except the contract's three examples: a 1 KiB header value,
// a 64 KiB string request body and a 64 KiB device model.
export const longFill = (id: string) => {
	if (id === 'E27') return 'h'.repeat(1024);
	if (id === 'E28') return 'd'.repeat(65536);
	if (id === 'E35') return 'm'.repeat(65536);
	return id + ':' + 'x'.repeat(3000);
};

const long = inventoryOccurrence(longFill);
export const LONG_OCCURRENCE: Row = long.occurrence;
export const LONG_DETAIL: Row = long.detail;

/** The `expand-text` buttons the selected LONG card shows before any click, per component (spec §12.2). */
export const LONG_EXPANDERS = {
	OccurrenceHeader: 8,
	StackFrames: 5,
	TagsTable: 1,
	ExceptionChain: 8,
	BreadcrumbTimeline: 5,
	RequestPanel: 6,
	UserPanel: 5,
	ContextCards: 6,
	total: 44,
} as const;

// ---------------------------------------------------------------------------------------------------
// `__proto__` keys (spec §3.4, L11). Built with JSON.parse: an object literal's `__proto__` sets the
// prototype instead of making a key. Each `__proto__` sits beside an ordinary sibling key.

const PROTO_V2_ID = '4d5e6f70-0000-4000-8000-000000000001';
const PROTO_PRE_V2_ID = '4d5e6f70-0000-4000-8000-000000000002';

function protoRow(id: string, v2: boolean): Row {
	const row: Row = JSON.parse(`{
		"id": "${id}", "issueId": "${ISSUE_ID}", "environment": "production", "platform": "go",
		"releaseVersion": null, "traceId": null, "spanId": null, "eventId": null, "projectId": "${PROJECT_ID}",
		"metadata": {"__proto__": {"polluted": true}, "request_id": "r-1"},
		"stacktrace": [{"__proto__": "x", "file": "main.go", "line": 1, "function": "main", "in_app": true}],
		"sdk": {"__proto__": {"a": 1}, "name": "sentinel.go", "version": "1.0.0"},
		"level": ${v2 ? '"error"' : 'null'}, "message": null, "serverName": null, "transaction": null, "dist": null
	}`);
	row.createdAt = new Date('2026-09-25T02:00:00.000Z');
	return row;
}

function protoDetail(): Row {
	const heavy: Row = JSON.parse(`{
		"tags": {"__proto__": "t", "env": "prod"},
		"contexts": {"__proto__": {"x": 1}, "device": {"__proto__": {"y": 2}, "model": "Pixel 7"}},
		"request": {"method": "GET", "headers": {"__proto__": "h", "Accept": "*/*"}},
		"breadcrumbs": [{"message": "m", "data": {"__proto__": 1, "url": "/a"}}],
		"user": {"__proto__": {"admin": true}, "id": "u1"},
		"exception": {"values": [{"type": "E", "mechanism": {"__proto__": 1, "type": "generic"}}, {"type": "F"}]}
	}`);
	return { ...protoRow(PROTO_V2_ID, true), ...heavy };
}

/** Listed rows (a v2 row first, so it is the default selection, then a pre-v2 row) and the detail row. */
export const PROTO_KEY_ROWS = {
	listed: () => [protoRow(PROTO_V2_ID, true), protoRow(PROTO_PRE_V2_ID, false)],
	detail: protoDetail,
};

/**
 * C6 / U4 (spec §4.2 rule 5): contexts keyed by `Object.prototype` names, plus an own `__proto__` key
 * (defined, not assigned, so it is a key and not the prototype).
 */
export function protoContexts(): Record<string, unknown> {
	const contexts: Record<string, unknown> = {
		device: { model: 'P7', constructor: 'Pixel' },
		constructor: { name: 'c' },
		toString: { v: 1 },
		hasOwnProperty: 'x',
	};
	const parsed = JSON.parse('{"__proto__": {"a": 1}}');
	Object.defineProperty(contexts, '__proto__', {
		value: Object.getOwnPropertyDescriptor(parsed, '__proto__')!.value,
		enumerable: true,
		configurable: true,
		writable: true,
	});
	return contexts;
}

// ---------------------------------------------------------------------------------------------------
// A JavaScript chain of four values, none symbolicated (X3): chain frames are never symbolicated (T12).

function jsChainValue(n: number): Row {
	return {
		type: `Error${n}`,
		value: `failure ${n}`,
		mechanism: { type: 'chained', handled: true },
		stacktrace: [
			{ function: `f${n}`, file: `https://cdn.example.com/assets/app.min.js`, line: 1, column: 100 + n, in_app: true },
			{ function: `g${n}`, file: `https://cdn.example.com/assets/vendor.min.js`, line: 2, column: 200 + n, in_app: false },
		],
	};
}

export const JS_CHAIN_DETAIL: Row = {
	id: '5e6f7081-0000-4000-8000-000000000001',
	platform: 'javascript',
	exception: { values: [jsChainValue(1), jsChainValue(2), jsChainValue(3), jsChainValue(4)] },
};

// ---------------------------------------------------------------------------------------------------
// F6: a v2 JavaScript row whose top-level stack mixes symbolicated and raw frames, the shape
// sentinel-sourcemaps writes. Crashing frame first.

export const V2_JS_OCCURRENCE: Row = {
	id: '6f708192-0000-4000-8000-000000000001',
	issueId: ISSUE_ID,
	environment: 'production',
	platform: 'javascript',
	releaseVersion: null,
	stacktrace: [
		{
			function: 'submitOrder',
			file: 'src/cart/checkout.ts',
			line: 42,
			column: 7,
			in_app: true,
			symbolicated: true,
			raw: { function: 'n', file: 'https://cdn.example.com/assets/app.3f9a1c.min.js', line: 1, column: 18234 },
		},
		{
			function: 'postJson',
			file: 'src/cart/api.ts',
			line: 17,
			column: 3,
			in_app: true,
			symbolicated: true,
			raw: { function: 't', file: 'https://cdn.example.com/assets/app.3f9a1c.min.js', line: 1, column: 9021 },
		},
		{ function: 'r', file: 'https://cdn.example.com/assets/vendor.min.js', line: 2, column: 1044, in_app: false },
	],
	metadata: {},
	traceId: null,
	spanId: null,
	eventId: null,
	projectId: PROJECT_ID,
	createdAt: '2026-09-25T01:30:00.000Z',
	level: 'error',
	message: "TypeError: Cannot read properties of undefined (reading 'total')",
	serverName: null,
	transaction: null,
	dist: null,
	sdk: { name: 'sentinel.javascript.browser', version: '1.2.0' },
};

export const V2_JS_DETAIL: Row = {
	...V2_JS_OCCURRENCE,
	...Object.fromEntries(HEAVY_KEYS.map((k) => [k, null])),
};

// ---------------------------------------------------------------------------------------------------
// F7 / SSR2 (d): frames a hand-written row can hold. Only the last two are objects.

export const MALFORMED_FRAMES: unknown[] = [
	null,
	7,
	'x',
	[],
	{ line: 3, in_app: true },
	{ file: null, function: { a: 1 }, line: 'x', raw: 'str', in_app: false },
];

export const V2_MALFORMED_OCCURRENCE: Row = {
	...V2_BARE_OCCURRENCE,
	id: '708192a3-0000-4000-8000-000000000001',
	stacktrace: MALFORMED_FRAMES,
};

// ---------------------------------------------------------------------------------------------------
// The page's `data`, in the shape of page.svelte.test.ts's makePageData, with the two keys this
// feature adds.

export function pageData(overrides: Row = {}): any {
	const occurrences = (overrides.occurrences ?? []) as Row[];
	return {
		issue: {
			id: ISSUE_ID,
			projectId: PROJECT_ID,
			environment: 'production',
			errorClass: 'CheckoutException',
			message: 'Issue-level message',
			status: 'unresolved',
			firstSeen: new Date('2026-09-20T00:00:00Z'),
			lastSeen: new Date('2026-09-25T02:00:00Z'),
			count: 3,
			issueType: 'system_error',
		},
		project: { id: PROJECT_ID, name: 'Daya' },
		occurrences,
		occurrenceTotal: occurrences.length,
		environmentDisplayName: 'Production',
		relations: [],
		trend: { hourly: [], daily: [] },
		retentionDays: 30,
		releases: [],
		canWrite: false,
		selectedOccurrenceId: occurrences[0]?.id ?? null,
		occurrenceDetail: null,
		...overrides,
	};
}

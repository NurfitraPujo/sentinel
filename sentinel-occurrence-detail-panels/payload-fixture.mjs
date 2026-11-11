// sentinel-occurrence-detail-panels spec §3.3 / §12.2 / §12.4: the measurement fixture.
//
// This is the source of FLUTTER_OCCURRENCE / FLUTTER_DETAIL. TASK-04 copies these two literals into
// apps/dashboard-web/src/lib/components/issues/occurrence-fixtures.ts verbatim (same keys, same values),
// and test M1 (spec §12.3) pins their serialised size to the numbers measure-payload.mjs prints, so the
// two copies cannot drift apart unnoticed.
//
// Plain ESM with no dependency, so `node sentinel-occurrence-detail-panels/measure-payload.mjs` runs from
// a bare checkout. The JSONB bodies are in the stored snake_case (spec T9-T17).

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
function flutterFrame(i) {
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
			module: 'package:example/' + file.slice(4).replace(/\.dart$/, ''),
			abs_path: 'package:example/' + file.slice(4),
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
			module: `package:example/core/app_${i}`,
		};
	}
	return {
		file: i % 2 === 0 ? 'dart:async/zone.dart' : 'package:flutter/src/widgets/framework.dart',
		line: 1000 + i * 17,
		function: i % 2 === 0 ? '_rootRunUnary' : 'StatefulElement.performRebuild',
		in_app: false,
	};
}

function chainFrames(n, prefix) {
	return Array.from({ length: n }, (_, i) => ({
		file: `lib/net/${prefix}_${i}.dart`,
		line: 10 + i,
		function: `${prefix}.step${i}`,
		in_app: i < 2,
	}));
}

const CATEGORIES = ['navigation', 'http', 'ui.click', 'console'];
const TYPES = { navigation: 'navigation', http: 'http', 'ui.click': 'user', console: 'debug' };

/** Crumb i of 100, one every 1.2 s, the last (i = 99) at the occurrence itself. */
function flutterCrumb(i) {
	const category = CATEGORIES[i % 4];
	const crumb = {
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
		crumb.message = `POST https://api.example.com/v2/orders/88412/confirm [${failed ? 502 : 200}]`;
		crumb.data = {
			method: 'POST',
			url: 'https://api.example.com/v2/orders/88412/confirm',
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

export const FLUTTER_OCCURRENCE = {
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
export const FLUTTER_DETAIL = {
	id: FLUTTER_OCCURRENCE.id,
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
			app_name: 'Example',
			app_version: '2.14.0',
			app_build: '2140',
			app_identifier: 'id.example.app',
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
		url: 'https://api.example.com/v2/orders/88412/confirm',
		query_string: 'retry=1&source=app',
		headers: {
			Accept: 'application/json',
			Authorization: '***REDACTED***',
			'Content-Type': 'application/json',
			'User-Agent': 'Example/2.14.0 (Android 14; Pixel 7)',
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
				value: 'Connection reset by peer (OS Error: errno = 104), address = api.example.com, port = 443',
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

export const LIGHT_KEYS = ['level', 'message', 'serverName', 'transaction', 'dist', 'sdk'];
export const HEAVY_KEYS = ['user', 'tags', 'contexts', 'request', 'breadcrumbs', 'exception'];
export const PRE_V2_KEYS = [
	'id', 'issueId', 'environment', 'platform', 'releaseVersion', 'stacktrace',
	'metadata', 'traceId', 'spanId', 'eventId', 'projectId', 'createdAt',
];

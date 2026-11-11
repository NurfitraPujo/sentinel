#!/usr/bin/env python3
"""Regenerates tests/fixtures/sentry/envelopes/* from the anonymized events (spec 10.2).

Run from the repository root:  python3 tests/fixtures/sentry/build_envelopes.py
The compressed variants are produced by non-Go tools on purpose (gzip, zlib), so the decoders are
proven against another implementation's output.
"""
import json, subprocess, zlib, pathlib

ROOT = pathlib.Path(__file__).parent
ENV = ROOT / "envelopes"
ENV.mkdir(exist_ok=True)


def compact(obj):
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def item(header, payload, length=True):
    h = dict(header)
    if length:
        h["length"] = len(payload)
    return b"\n" + compact(h) + b"\n" + payload


# flutter-9.6.0: the framing of sentry-dart's SentryEnvelope.envelopeStream (length on every item, no trailing newline).
event = json.loads((ROOT / "flutter-typeerror-unhandled.json").read_text())
ev_bytes = compact(event)
header = {
    "event_id": "11111111111141118111111111111111",
    "sdk": {"name": "sentry.dart.flutter", "version": "9.6.0"},
    "trace": {"trace_id": "0000000000000000000000000000c0de", "public_key": "sent_live_fixture"},
    "dsn": "https://sent_live_fixture@ingest.example.invalid/27690631804612",
    "sent_at": "2026-09-25T09:41:53.000Z",
}
txn = {
    "type": "transaction",
    "event_id": "33333333333343338333333333333333",
    "transaction": "/checkout",
    "start_timestamp": 1790329310.0,
    "timestamp": 1790329311.0,
    "platform": "other",
    "contexts": {"trace": {"trace_id": "0000000000000000000000000000c0de", "span_id": "00000000000000b1", "op": "navigation"}},
    "spans": [],
}
report = {
    "timestamp": "2026-09-25T09:41:53.000Z",
    "discarded_events": [{"reason": "sample_rate", "category": "transaction", "quantity": 1}],
}
flutter = (
    compact(header)
    + item({"content_type": "application/json", "type": "event"}, ev_bytes)
    + item({"content_type": "application/json", "type": "transaction"}, compact(txn))
    + item({"content_type": "application/json", "type": "client_report"}, compact(report))
)
(ENV / "flutter-9.6.0.envelope").write_bytes(flutter)

# nextjs-10: hand-built from Sentry's envelope docs; item headers WITHOUT length, one-line payloads, no trailing newline.
nx_event = {
    "event_id": "44444444444444448444444444444444",
    "platform": "javascript",
    "timestamp": 1790330400.123,
    "level": "error",
    "environment": "production",
    "release": "web@1.0.0",
    "exception": {"values": [{
        "type": "TypeError",
        "value": "Cannot read properties of undefined (reading 'id')",
        "mechanism": {"type": "generic", "handled": False},
        "stacktrace": {"frames": [
            {"filename": "app:///_next/server/app/page.js", "function": "renderPage", "lineno": 12, "colno": 5, "in_app": True},
            {"filename": "app:///_next/server/app/checkout/page.js", "function": "loadCart", "lineno": 88, "colno": 17, "in_app": True},
        ]},
    }]},
    "breadcrumbs": [
        {"timestamp": 1790330399.5, "type": "http", "category": "fetch", "level": "info", "data": {"url": "https://api.example.invalid/cart", "status_code": 200}},
        {"timestamp": 1790330399.9, "type": "default", "category": "console", "level": "log", "message": "cart loaded"},
    ],
    "tags": {"runtime": "node", "transaction": "GET /checkout"},
    "request": {"method": "GET", "url": "https://app.example.invalid/checkout",
                "headers": {"User-Agent": "Mozilla/5.0 (X11; Linux x86_64)", "Accept": "text/html"}},
    "contexts": {"trace": {"trace_id": "0000000000000000000000000000c0de", "span_id": "00000000000000c1"},
                 "runtime": {"name": "node", "version": "v22.0.0"}},
    "sdk": {"name": "sentry.javascript.nextjs", "version": "10.69.0"},
}
nx_header = {"event_id": "44444444444444448444444444444444", "sent_at": "2026-09-25T10:00:00.000Z",
             "sdk": {"name": "sentry.javascript.nextjs", "version": "10.69.0"}}
nextjs = (
    compact(nx_header)
    + item({"type": "event"}, compact(nx_event), length=False)
    + item({"type": "session"}, compact({"sid": "55555555555545558555555555555555", "init": True, "started": "2026-09-25T10:00:00.000Z",
                                          "timestamp": "2026-09-25T10:00:00.000Z", "status": "ok", "errors": 1,
                                          "attrs": {"release": "web@1.0.0", "environment": "production"}}), length=False)
)
(ENV / "nextjs-10.envelope").write_bytes(nextjs)

for name in ("flutter-9.6.0.envelope", "nextjs-10.envelope"):
    raw = (ENV / name).read_bytes()
    (ENV / (name + ".gz")).write_bytes(subprocess.run(["gzip", "-9nc"], input=raw, capture_output=True, check=True).stdout)
    if name.startswith("flutter"):
        (ENV / (name + ".zlib")).write_bytes(zlib.compress(raw, 9))
        c = zlib.compressobj(9, zlib.DEFLATED, -15)
        (ENV / (name + ".deflate")).write_bytes(c.compress(raw) + c.flush())

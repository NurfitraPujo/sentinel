# Sentry-compatible ingest fixtures

Fixtures for `sentinel-sentry-compat-ingest` (spec §10). Every file is safe to commit: the two real
Flutter events were anonymized before they were copied here, and
`tests/unit/sentry_compat_fixtures_test.go: TestSentryFixturesAreAnonymized` fails on any leaked
address, e-mail, foreign URL host, original identifier or real geo value.

| File | What it is |
|---|---|
| `flutter-typeerror-unhandled.json`, `flutter-dio-error.json` | Two real sentry_flutter 9.6.0 events (a `TypeError` from `PlatformDispatcher.onError`, and a `DioException`), edited only where §10.1 lists: event, device, trace, span and user ids, `user.geo.country_code`, two public hosts and the kernel build string. |
| `envelopes/flutter-9.6.0.envelope` | The typeerror event framed as sentry-dart's `SentryEnvelope.envelopeStream` frames it (every item has `length`, no trailing newline), plus a `transaction` and a `client_report` item. |
| `envelopes/flutter-9.6.0.envelope.{gz,zlib,deflate}` | The same bytes: `gzip -9n`, zlib, and raw deflate, made by non-Go tools so the decoders are proven against another implementation's output. |
| `envelopes/flutter-9.6.0-transaction.envelope` | Hand-built (sentinel-span-ingest): a sentry_flutter 9.6.0 Dio request `transaction` (root plus three child spans, one with a secret-shaped attribute) framed with `length`. Read by the span-ingest unit and integration tests; not in the anonymization scan because it holds no real identifier. |
| `envelopes/nextjs-10.envelope` (+ `.gz`) | Hand-built from Sentry's envelope docs as `@sentry/nextjs` 10.69.0 sends it: item headers without `length`, breadcrumbs as a plain array, tags as an object, then a `session` item. |
| `project-id-vectors.json` | The golden vector for the derived Sentry project id, read by `auth.SentryProjectID`'s test and by the dashboard's vitest (spec §3.4). |
| `flutter-typeerror-unhandled.payload.golden.json` | The mapper's output for the typeerror fixture (P4.1). |
| `anonymization-denylist.txt` | SHA-256 hashes (lower-cased) of the original identifiers. The originals are never stored in the repository. |
| `build_envelopes.py` | Regenerates everything under `envelopes/` from the two anonymized events. |

## Regenerating

```bash
python3 tests/fixtures/sentry/build_envelopes.py       # envelopes and their compressed variants
# the variants individually:
gzip -9nc tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope > tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope.gz
python3 -c 'import sys,zlib; sys.stdout.buffer.write(zlib.compress(sys.stdin.buffer.read(), 9))' \
  < tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope > tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope.zlib
python3 -c 'import sys,zlib; c=zlib.compressobj(9, zlib.DEFLATED, -15); sys.stdout.buffer.write(c.compress(sys.stdin.buffer.read())+c.flush())' \
  < tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope > tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope.deflate
# the golden payload (fails on purpose after rewriting; rerun without -update):
GOWORK=off go test ./tests/unit/ -run TestSentryCompatFlutterGolden -args -update
```

`TestSentryEnvelopeVariantsMatch` proves each compressed variant decompresses byte-identical to its
source, so a regenerated identity envelope with stale variants fails.

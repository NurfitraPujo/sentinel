# GeoIP test databases

MaxMind's own test databases, committed so no test downloads anything
(sentinel-event-enrichment spec §5.6).

- **Source:** <https://github.com/maxmind/MaxMind-DB>, `test-data/`, at commit
  `0eef25a46e20f4e96d27b951d0228efabe21323f` (2026-09-22).
- **Built from:** `source-data/GeoLite2-City-Test.json` and `source-data/GeoIP2-City-Test.json` at that commit.
- **Licence:** the MaxMind-DB repository is "licensed under the Apache License, Version 2.0 or the MIT
  License, at your option". Both texts are copied here from the root of that commit
  (`LICENSE-APACHE`, `LICENSE-MIT`).

| File | `DatabaseType` | `BuildEpoch` | Size | Used for |
|---|---|---|---|---|
| `GeoLite2-City-Test.mmdb` | `GeoLite2-City` | 2026-02-04 22:49:29 UTC | 21088 B | every lookup test |
| `GeoIP2-City-Test.mmdb` | `GeoIP2-City` | 2026-02-04 22:49:29 UTC | 22569 B | the reload test only (a different object and `DatabaseType`; same build epoch) |

Metadata read with `maxminddb-golang` v2.6.0 on 2026-09-24; each file's `Verify()` passes.

**Addresses the tests use** (present in `source-data/GeoLite2-City-Test.json` at the pinned commit):

| Address | Record |
|---|---|
| `81.2.69.142` (`81.2.69.142/31`) | city `London`, country `GB` / `United Kingdom`, subdivision `England` |
| `198.51.100.23` | not in the database (a documentation address; a lookup miss) |

Both databases are older than the ingestor's default `GEOIP_MAX_AGE` (720 h), so every test that
expects an answer injects the loader's clock (`geoip.Config.Now`) or sets `GEOIP_MAX_AGE`. A corrupt
database is produced inside the test (the valid file truncated); no broken binary is committed.

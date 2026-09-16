-- sentinel-sourcemaps §8 / §4.4 "Consequence stated, not hidden" (be.md task 13, decision 31) —
-- the PRE-DEPLOY count of the identity split.
--
-- What it counts. Every existing JavaScript-platform occurrence whose fingerprint input changes
-- under this feature gets a NEW fingerprint on its next occurrence — one identity split per such
-- issue, once, after which it is stable across builds. The input changes when any of the frames
-- `fingerprint.Compute` hashes (apps/processor-go/fingerprint/fingerprint.go: the first three
-- `in_app` frames, or — the P4-3 fallback — the first three frames when none is `in_app`; never
-- line or column, never a fourth frame):
--
--   * carries a scheme and host (`^https?://<host>`) — §4.4 step 1 strips both for the hash input;
--   * matches the BASENAME rule — in the last path segment, a `-` or `.` followed by a hash token
--     immediately before the extension run, or a bare-hash stem (`8f7a6b5c3d2e1f0a.js`); a hash
--     token is 6–64 chars of [A-Za-z0-9_] that contains a digit OR is entirely [0-9a-f]; the
--     extension run is the trailing `.<ext>` segments of 1–8 lowercase letters (`.chunk.js`,
--     `.min.js`) — mirrored from NormalizeHashInput / splitExtensionRun / isHashToken, edge cases
--     included (a `.abcdef` suffix is an extension first and a token never, as in Go);
--   * matches the DIRECTORY rule — a whole non-final segment of ≥ 16 chars of [A-Za-z0-9_-]
--     containing a digit (a Next.js build id).
--
-- Step 1 (b) — the `?…` / `#…` strip, whatever the scheme — is applied before the rules, as
-- Compute does, and is a split in its own right: before this feature Compute hashed `File`
-- verbatim, so a frame carrying `?v=1` or `#L1` changes its hash input even when no rule matches.
-- It is counted under `by_query_or_fragment` and folded into the totals.
--
-- sentinel-sourcemaps-turbopack-grouping §4.4 (decision 18, be.md task 9) EXTENDS the count with
-- two more predicates, folded into the same totals:
--
--   * the SYMBOLICATED-frame predicate (§4.2) — a frame the stage rewrote (`symbolicated: true`)
--     whose resolved `file` contains `/node_modules/` (or starts with `node_modules/`, a bare
--     relative source under an empty sourceRoot) or whose namespace ROOT — the first segment after
--     an optional `<scheme>:///`, or the leading segment of a bare path — is a bracketed name other
--     than `[project]` (`[turbopack]`, `[next]`, `[externals]`; a `[id]` or `[...slug]` later in the
--     path is an ordinary directory). Mirrored from symbolicate.inApp. Such a frame's `in_app`
--     becomes the processor's on the next occurrence instead of the SDK's, which changes WHICH
--     frames Compute selects — so this predicate is judged over EVERY frame of the occurrence, not
--     only the three currently selected. `by_symbolicated_vendor` is the predicate as §4.4 states
--     it; `by_in_app_rederived_changed` narrows it to the frames whose stored `in_app` differs from
--     the value the rule derives (a vendor frame the SDK already sent as `false` does not move).
--     The wider one is what the totals fold in — the count is conservative by construction.
--   * the UNSYMBOLICATED-frame predicate (§4.3) — a selected frame the stage did not rewrite
--     (no `symbolicated: true`) whose basename matches `_[0-9a-f]{8}(\._)?\.[a-z]+$`:
--     Turbopack's `_<8-hex>` server-chunk suffix, with or without the `._` merged-chunk marker,
--     which the normaliser now strips (`_052cc036._.js` → `_*._.js`). Judged, like the two rules
--     above, on the frames Compute hashes.
--
-- What it does NOT do. It cannot tell a pre-feature row from one the new processor already
-- stored — on a database the feature has run against, every symbolicated row's `file` is a source
-- path (`../../src/x.ts`) that matches nothing, and every raw row matches exactly as it did before,
-- so the number is meaningful ONLY when run BEFORE the processor tag that carries §4.4. That is
-- why its staging and production runs are the release condition of the held staging item
-- PLANNING files (decision 31), not a RUNBOOK sentence; a non-zero count is reported to the
-- operator, never worked around. The population is zero by the record (no browser producer
-- exists, contract Fact 2) and this file is for the day one does.
--
--   usage:  psql "$DATABASE_URL" -f scripts/sourcemaps-identity-split-count.sql
--
-- The platform set is SYMBOLICATE_PLATFORMS' default (`javascript,browser`); edit the IN list if
-- the deployment sets another.

WITH js AS (
  SELECT eo.id, eo.issue_id, eo.stacktrace
    FROM error_occurrences eo
   WHERE eo.platform IN ('javascript', 'browser')
     AND jsonb_typeof(eo.stacktrace) = 'array'
),
frames AS (
  SELECT js.id AS occurrence_id, js.issue_id, f.ord,
         COALESCE(f.frame->>'file', '') AS file,
         COALESCE((f.frame->>'in_app')::boolean, false) AS in_app,
         COALESCE((f.frame->>'symbolicated')::boolean, false) AS symbolicated
    FROM js
    CROSS JOIN LATERAL jsonb_array_elements(js.stacktrace) WITH ORDINALITY AS f(frame, ord)
),
-- Compute's selection: the first three in_app frames, else (P4-3) the first three frames.
selected AS (
  SELECT occurrence_id, issue_id, ord, file, symbolicated
    FROM (
      SELECT fr.*,
             bool_or(in_app) OVER (PARTITION BY occurrence_id) AS any_in_app,
             row_number() OVER (PARTITION BY occurrence_id, in_app ORDER BY ord) AS rn_flag,
             row_number() OVER (PARTITION BY occurrence_id ORDER BY ord) AS rn
        FROM frames fr
    ) x
   WHERE (any_in_app AND in_app AND rn_flag <= 3) OR (NOT any_in_app AND rn <= 3)
),
-- Step 1 of §4.4 on the selected frames, then the two rules on the result.
judged AS (
  SELECT s.occurrence_id, s.issue_id, s.ord, s.file,
         s.file ~* '^https?://[^/?#]'                                  AS has_scheme_host,
         s.file ~ '[?#]'                                               AS has_query_or_fragment,
         p.path,
         -- turbopack-grouping §4.3: an unrewritten frame whose basename ends in _<8-hex>[._].<ext>
         (NOT s.symbolicated AND bb.base ~ '_[0-9a-f]{8}(\._)?\.[a-z]+$') AS turbopack_suffix_rule,
         -- basename rule: token after the last [-.] of the stem (or the whole stem), before the ext run
         (b.token ~ '^[A-Za-z0-9_]{6,64}$'
            AND (b.token ~ '[0-9]' OR b.token ~ '^[0-9a-f]+$')
            AND b.stem <> '')                                           AS basename_rule,
         EXISTS (
           SELECT 1
             FROM unnest(string_to_array(p.path, '/')) WITH ORDINALITY AS seg(s, i)
            WHERE i < array_length(string_to_array(p.path, '/'), 1)
              AND seg.s ~ '^[A-Za-z0-9_-]{16,}$'
              AND seg.s ~ '[0-9]'
         )                                                             AS directory_rule
    FROM selected s
    CROSS JOIN LATERAL (
      SELECT regexp_replace(regexp_replace(s.file, '^[Hh][Tt][Tt][Pp][Ss]?://[^/?#]+', ''), '[?#].*$', '') AS path
    ) p
    CROSS JOIN LATERAL (
      SELECT regexp_replace(p.path, '^.*/', '') AS base
    ) bb
    CROSS JOIN LATERAL (
      -- the maximal trailing run of .<ext> (1–8 lowercase letters each); '' when there is none
      SELECT COALESCE(substring(bb.base FROM '((\.[a-z]{1,8})+)$'), '') AS ext
    ) e
    CROSS JOIN LATERAL (
      SELECT left(bb.base, length(bb.base) - length(e.ext)) AS stem
    ) st
    CROSS JOIN LATERAL (
      SELECT st.stem, regexp_replace(st.stem, '^.*[-.]', '') AS token
    ) b
),
-- turbopack-grouping §4.2 on EVERY symbolicated frame of the occurrence (it moves the selection,
-- so the currently selected three are not the population): mirrored from symbolicate.inApp.
rederived AS (
  SELECT fr.occurrence_id, fr.issue_id,
         bool_or(v.vendor)                    AS symbolicated_vendor,
         bool_or(v.vendor = fr.in_app)        AS in_app_changed   -- derived (NOT vendor) <> stored
    FROM frames fr
    CROSS JOIN LATERAL (
      SELECT fr.file LIKE '%/node_modules/%'
          OR fr.file LIKE 'node\_modules/%'
          OR COALESCE(
               substring(fr.file FROM '^(?:[a-z][a-z0-9+.-]*:///)?\[([a-z][a-z0-9-]*)\]/') <> 'project',
               false) AS vendor
    ) v
   WHERE fr.symbolicated
   GROUP BY fr.occurrence_id, fr.issue_id
),
per_occurrence AS (
  SELECT j.occurrence_id, j.issue_id,
         bool_or(j.has_scheme_host)       AS scheme_host,
         bool_or(j.has_query_or_fragment) AS query_or_fragment,
         bool_or(j.basename_rule)         AS basename,
         bool_or(j.directory_rule)        AS directory,
         bool_or(j.turbopack_suffix_rule) AS turbopack_suffix,
         COALESCE(bool_or(r.symbolicated_vendor), false) AS symbolicated_vendor,
         COALESCE(bool_or(r.in_app_changed), false)      AS in_app_changed
    FROM judged j
    LEFT JOIN rederived r ON r.occurrence_id = j.occurrence_id
   GROUP BY j.occurrence_id, j.issue_id
)
SELECT
  (SELECT count(*) FROM js)                                                        AS javascript_occurrences,
  count(*) FILTER (WHERE scheme_host OR query_or_fragment OR basename OR directory
                      OR symbolicated_vendor OR turbopack_suffix)                   AS occurrences_that_would_split,
  count(DISTINCT issue_id)
      FILTER (WHERE scheme_host OR query_or_fragment OR basename OR directory
                 OR symbolicated_vendor OR turbopack_suffix)                        AS issues_that_would_split,
  count(*) FILTER (WHERE scheme_host)                                               AS by_scheme_host,
  count(*) FILTER (WHERE query_or_fragment)                                         AS by_query_or_fragment,
  count(*) FILTER (WHERE basename)                                                  AS by_basename_rule,
  count(*) FILTER (WHERE directory)                                                 AS by_directory_rule,
  count(*) FILTER (WHERE symbolicated_vendor)                                       AS by_symbolicated_vendor,
  count(*) FILTER (WHERE in_app_changed)                                            AS by_in_app_rederived_changed,
  count(*) FILTER (WHERE turbopack_suffix)                                          AS by_turbopack_suffix_rule
  FROM per_occurrence;

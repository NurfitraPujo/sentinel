-- +goose NO TRANSACTION
-- sentinel-issue-tag-facets TASK-01 -- the per-issue tag rollup `issue_tag_values`, kept exact by
-- statement-level triggers on error_occurrences, plus the backfill state, the drift record and the
-- four error_occurrences indexes /search's tag filter needs. Spec: sentinel-issue-tag-facets/spec.md
-- §3 (rollup), §4 (backfill), §5 (retention), §6.4 (search indexes), §8 (this file); contract
-- decision 9 ratified the trigger design. The Drizzle mirror is apps/dashboard-web/src/lib/db/schema.ts
-- `issueTagValues`, `issueTagValuesBackfill`, `issueTagValuesDrift` and
-- `errorOccurrences.tagRollupApplied` (hand-maintained; this file is the source of truth).
--
-- The no-transaction directive is line 1 and appears nowhere else in this file, not even quoted:
-- goose scans every comment line for its annotation marker (the 1722200000 / 1725200000 headers
-- record why). Every statement and every DO / function body sits in its own StatementBegin /
-- StatementEnd pair: those bodies contain semicolons, and goose would otherwise split them at the
-- first one. Each pair is one autocommit Exec, which CREATE INDEX CONCURRENTLY requires.
--
-- THE FIFTH IN-TRANSACTION EXCEPTION. The triggers below write issue_tag_values inside every
-- transaction that inserts, deletes or updates an occurrence -- inside StoreEvent's transaction at its
-- step 2 (store.go's doc comment lists it as the fifth deliberate exception). Exact counts are the
-- contract's criterion; written after commit, a NAK between the occurrence commit and the rollup
-- write would lose the count or, on retry, find the occurrence deduplicated and never count it.
-- Triggers, not Go code, so that every pod version counts, old pods during the roll included: the
-- backfill's boundary is the commit of block TR, a database fact (spec §4.3).
--
-- LOCK ORDER, the rule every exactness argument rests on: EVERY WRITER OF issue_tag_values AND EVERY
-- DELETER OF OCCURRENCES LOCKS THE OWNING issues ROWS FIRST, in id order, and only then touches
-- occurrence rows and rollup rows (issues -> error_occurrences -> issue_tag_values). StoreEvent takes
-- the issues row at its step 1; the triggers re-assert it (FOR NO KEY UPDATE); the retention sweep
-- takes FOR UPDATE in id order before its DELETE; the backfill locks before it counts. A future
-- writer or deleter that breaks this order breaks exactness (spec §3.5, §4.4, §11.2).
--
-- COST FOR FUTURE MIGRATIONS: any UPDATE of error_occurrences pays ~70 us per updated row in the
-- update trigger, even when it changes no rollup column (measured, spec W9 / §8.3): a full-table
-- UPDATE of production's ~4.4 M rows spends ~5 minutes in it. Such a migration must BATCH its update;
-- NEVER DISABLE THE TRIGGERS -- a disabled trigger lets the rollup drift silently, and the backfill
-- refuses to run while one is missing or disabled (contract decision 9, spec §8.3, §11.2).
--
-- Lock profile (spec §8.1): block C (the column) and block TR (the triggers) take a conflicting lock
-- on error_occurrences, and block T1 (the FK) on issues; each is a bounded-wait DO block: every attempt
-- waits at most deadlock_timeout + 500 ms, gives the queue back on lock_not_available, sleeps 5-6 s,
-- and tries again, 20 attempts, so no other statement waits more than 1.5 s behind this file at a
-- time. A replay's blocks C and T1 take no lock at all: each returns at its guard (pg_attribute,
-- to_regclass) before reaching its ALTER. Block TR's CREATE OR REPLACE TRIGGER still takes SHARE ROW
-- EXCLUSIVE on error_occurrences on every replay (never ACCESS EXCLUSIVE, which DROP TRIGGER would),
-- which conflicts with ingest's ROW EXCLUSIVE — bounded to the same 1.5 s per attempt as every block
-- above. The five index builds run CONCURRENTLY with lock_timeout = 0; the four on
-- error_occurrences get statement_timeout = 60min (two heap scans each on a ~7-20 GB heap).
--
-- Idempotency: one flat directory serves every goose target, and every block is re-runnable: the
-- guarded column, IF NOT EXISTS tables, the singleton insert ON CONFLICT DO NOTHING (a replay never
-- resets the cursor), CREATE OR REPLACE FUNCTION / TRIGGER, and drop-if-invalid before each
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS with a validity check that RAISEs by name.
--
-- No backfill here (spec §4.1): `processor backfill-tag-facets`, run once per environment from the
-- suspended CronJob, counts the pre-existing rows.
--
-- DEPLOY ORDER (spec §8.4): this migration first, then the processor and the dashboard, then the
-- backfill Job. Every intermediate state works: old processor pods' inserts are counted by the
-- triggers; an old dashboard's single-statement retention DELETE stays exact through the delete trigger.
-- ROLLBACK ORDER: roll the dashboard and processor back first, then run Down.
-- +goose Up
-- block S
-- +goose StatementBegin
SET statement_timeout = '15min';
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- block C: the marker column (guarded, bounded wait)
-- +goose StatementBegin
DO $$
-- block C: bounded lock wait. Each attempt waits at most deadlock_timeout + 500 ms for its lock, so a
-- statement queued behind this DDL is stalled at most that long; attempts are 5-6 s apart; 20 attempts.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'error_occurrences'::regclass AND attname = 'tag_rollup_applied' AND NOT attisdropped) THEN RETURN; END IF;
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      ALTER TABLE error_occurrences ADD COLUMN tag_rollup_applied BOOLEAN;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE '1725400000 block C: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd

-- block T1: issue_tag_values (guarded, bounded wait)
-- +goose StatementBegin
DO $$
-- block T1: bounded lock wait. Each attempt waits at most deadlock_timeout + 500 ms for its lock, so a
-- statement queued behind this DDL is stalled at most that long; attempts are 5-6 s apart; 20 attempts.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  IF to_regclass('issue_tag_values') IS NOT NULL THEN RETURN; END IF;
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      CREATE TABLE IF NOT EXISTS issue_tag_values (
          issue_id   UUID        NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
          key        VARCHAR(32) NOT NULL CHECK (key <> ''),
          value      TEXT        NOT NULL CHECK (octet_length(value) <= 1024),
          count      BIGINT      NOT NULL CHECK (count >= 0),
          first_seen TIMESTAMPTZ NOT NULL,
          last_seen  TIMESTAMPTZ NOT NULL,
          PRIMARY KEY (issue_id, key, value)
      ) WITH (fillfactor = 70);
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE '1725400000 block T1: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd

-- block T2: the backfill state table
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS issue_tag_values_backfill (
    singleton    BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),
    cursor_id    UUID,
    done         BOOLEAN     NOT NULL DEFAULT false,
    rows_counted BIGINT      NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ
);
-- +goose StatementEnd

-- block T2: its singleton row (a replay never resets the cursor)
-- +goose StatementBegin
INSERT INTO issue_tag_values_backfill (singleton) VALUES (true) ON CONFLICT (singleton) DO NOTHING;
-- +goose StatementEnd

-- block T3: the drift record (W11)
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS issue_tag_values_drift (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    issue_id      UUID        NOT NULL,
    contributions BIGINT      NOT NULL CHECK (contributions > 0),
    source        TEXT        NOT NULL CHECK (source IN ('delete', 'update')),
    detected_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    reported_at   TIMESTAMPTZ,
    rebuilt_at    TIMESTAMPTZ,
    operator_only_at TIMESTAMPTZ   -- set by the retention route when it will not rebuild the issue (§5); cleared by nothing,
                                   -- superseded by rebuilt_at when the operator's issue_tag_values_rebuild runs
);
-- +goose StatementEnd

-- block F: the thirteen functions. The six on the hot path are PL/pgSQL with plan_cache_mode =
-- force_generic_plan, so their plans are built once per session (spec §3.5).

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_derived(p_contexts jsonb, p_user jsonb, p_release text, p_environment text)
RETURNS TABLE (ord int, key text, value text) LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT * FROM (VALUES
    (1, 'release',        p_release),
    (2, 'environment',    p_environment),
    (3, 'os.name',        CASE WHEN jsonb_typeof(p_contexts->'os'->'name')        = 'string' THEN p_contexts->'os'->>'name'        END),
    (4, 'client_os.name', CASE WHEN jsonb_typeof(p_contexts->'client_os'->'name') = 'string' THEN p_contexts->'client_os'->>'name' END),
    (5, 'device.model',   CASE WHEN jsonb_typeof(p_contexts->'device'->'model')   = 'string' THEN p_contexts->'device'->>'model'   END),
    (6, 'browser.name',   CASE WHEN jsonb_typeof(p_contexts->'browser'->'name')   = 'string' THEN p_contexts->'browser'->>'name'   END),
    (7, 'user.id',        CASE WHEN jsonb_typeof(p_user->'id')                    = 'string' THEN p_user->>'id'                    END)
  ) AS d(ord, key, value)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_pairs(p_tags jsonb, p_contexts jsonb, p_user jsonb, p_release text, p_environment text)
RETURNS TABLE (key text, value text) LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  WITH derived AS (SELECT d.key, d.value FROM issue_tag_derived(p_contexts, p_user, p_release, p_environment) d),
  candidates AS (
    SELECT d.key, d.value FROM derived d
    UNION ALL
    SELECT e.key, e.j #>> '{}'
      FROM jsonb_each(CASE WHEN jsonb_typeof(p_tags) = 'object' THEN p_tags END) AS e(key, j)
     WHERE jsonb_typeof(e.j) = 'string'
       AND e.key ~ '^[a-zA-Z0-9_.:-]{1,32}$'
       AND NOT EXISTS (SELECT 1 FROM derived d WHERE d.key = e.key)
  )
  SELECT c.key, c.value FROM candidates c
   WHERE c.value IS NOT NULL AND c.value <> '' AND octet_length(c.value) <= 1024
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_counted(p_applied boolean, p_id uuid, p_cursor uuid, p_done boolean)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT p_applied IS TRUE OR (p_applied IS NULL AND (p_done OR (p_cursor IS NOT NULL AND p_id <= p_cursor)))
$$;
-- +goose StatementEnd

-- Callers hold the issues row locks of every p_issue. Input rows are distinct (issue, key, value).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_values_add(
    p_issue uuid[], p_key text[], p_value text[], p_n bigint[], p_first timestamptz[], p_last timestamptz[])
RETURNS void LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
BEGIN
    WITH d AS (SELECT * FROM unnest(p_issue, p_key, p_value, p_n, p_first, p_last) AS d(issue_id, key, value, n, fs, ls)),
    upd AS (
      UPDATE issue_tag_values t
         SET count = t.count + d.n, first_seen = LEAST(t.first_seen, d.fs), last_seen = GREATEST(t.last_seen, d.ls)
        FROM d
       WHERE t.issue_id = d.issue_id AND t.key = d.key AND t.value = d.value AND t.value <> ''
      RETURNING t.issue_id, t.key, t.value),
    miss AS (SELECT d.* FROM d WHERE NOT EXISTS (
               SELECT 1 FROM upd u WHERE u.issue_id = d.issue_id AND u.key = d.key AND u.value = d.value)),
    state AS MATERIALIZED (
              SELECT k.issue_id, k.key, k.has_overflow,
                     CASE WHEN k.has_overflow THEN NULL
                          ELSE (SELECT count(*) FROM issue_tag_values t
                                 WHERE t.issue_id = k.issue_id AND t.key = k.key AND t.value <> '') END AS named
                FROM (SELECT m.issue_id, m.key,
                             EXISTS (SELECT 1 FROM issue_tag_values t
                                      WHERE t.issue_id = m.issue_id AND t.key = m.key AND t.value = '') AS has_overflow
                        FROM (SELECT DISTINCT issue_id, key FROM miss) m) k),
    ranked AS (SELECT miss.*, s.named, s.has_overflow,
                 row_number() OVER (PARTITION BY miss.issue_id, miss.key ORDER BY miss.value COLLATE "C") AS rk
               FROM miss JOIN state s USING (issue_id, key)),
    ins AS (INSERT INTO issue_tag_values (issue_id, key, value, count, first_seen, last_seen)
            SELECT issue_id, key, value, n, fs, ls FROM ranked WHERE NOT has_overflow AND named + rk <= 1000
            RETURNING 1)
    INSERT INTO issue_tag_values (issue_id, key, value, count, first_seen, last_seen)
    SELECT issue_id, key, '', sum(n), min(fs), max(ls) FROM ranked
     WHERE has_overflow OR named + rk > 1000
     GROUP BY issue_id, key
    ON CONFLICT (issue_id, key, value) DO UPDATE
       SET count      = issue_tag_values.count + EXCLUDED.count,
           first_seen = LEAST(issue_tag_values.first_seen, EXCLUDED.first_seen),
           last_seen  = GREATEST(issue_tag_values.last_seen, EXCLUDED.last_seen);
END $$;
-- +goose StatementEnd

-- Callers hold the issues row locks of every p_issue. Deletes the issues' rollup rows and re-adds the pairs
-- of their COUNTED occurrences. Exact, because nothing else can write these issues' rows meanwhile.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_values_rebuild_locked(p_issues uuid[]) RETURNS bigint LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
DECLARE bf record; a record;
BEGIN
  SELECT b.cursor_id, b.done INTO bf FROM issue_tag_values_backfill b;
  DELETE FROM issue_tag_values WHERE issue_id = ANY(p_issues);
  SELECT array_agg(g.issue_id) AS i, array_agg(g.key) AS k, array_agg(g.value) AS v,
         array_agg(g.cnt) AS c, array_agg(g.fs) AS f, array_agg(g.ls) AS l, sum(g.cnt) AS total INTO a
    FROM (SELECT o.issue_id, p.key, p.value, count(*)::bigint AS cnt,
                 min(coalesce(o.created_at, now())) AS fs, max(coalesce(o.created_at, now())) AS ls
            FROM error_occurrences o
            CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
           WHERE o.issue_id = ANY(p_issues) AND issue_tag_counted(o.tag_rollup_applied, o.id, bf.cursor_id, bf.done)
           GROUP BY 1, 2, 3) g;
  IF a.i IS NOT NULL THEN PERFORM issue_tag_values_add(a.i, a.k, a.v, a.c, a.f, a.l); END IF;
  RETURN coalesce(a.total, 0);
END $$;
-- +goose StatementEnd

-- Callers hold the issues row locks of every p_issue. Subtracts; deletes, by primary key, exactly the rows the
-- two UPDATEs brought to 0 (their RETURNING rows); for every issue
-- where a contribution found nothing (or too little) to subtract from, records one drift row and rebuilds
-- that issue exactly (W11). Returns the rebuilt issue ids (NULL when none drifted).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_values_subtract(
    p_issue uuid[], p_key text[], p_value text[], p_n bigint[], p_source text)
RETURNS uuid[] LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
DECLARE drifted uuid[]; drifted_n bigint[]; small uuid[]; deferred uuid[]; budget bigint := 10000; sz bigint;
        z_i uuid[]; z_k text[]; z_v text[];
BEGIN
  WITH d AS (SELECT * FROM unnest(p_issue, p_key, p_value, p_n) AS d(issue_id, key, value, n)),
  named AS (SELECT d.*, t.count AS cur FROM d JOIN issue_tag_values t
              ON t.issue_id = d.issue_id AND t.key = d.key AND t.value = d.value AND t.value <> ''),
  upd AS (UPDATE issue_tag_values t SET count = GREATEST(t.count - nm.n, 0)
            FROM named nm WHERE t.issue_id = nm.issue_id AND t.key = nm.key AND t.value = nm.value
          RETURNING t.issue_id, t.key, t.value, t.count),
  miss AS (SELECT d.issue_id, d.key, sum(d.n) AS n FROM d WHERE NOT EXISTS (
             SELECT 1 FROM named nm WHERE nm.issue_id = d.issue_id AND nm.key = d.key AND nm.value = d.value)
           GROUP BY 1, 2),
  ovf AS (SELECT m.*, t.count AS cur FROM miss m LEFT JOIN issue_tag_values t
            ON t.issue_id = m.issue_id AND t.key = m.key AND t.value = ''),
  upd2 AS (UPDATE issue_tag_values t SET count = GREATEST(t.count - o.n, 0)
             FROM ovf o WHERE o.cur IS NOT NULL AND t.issue_id = o.issue_id AND t.key = o.key AND t.value = ''
           RETURNING t.issue_id, t.key, t.value, t.count),
  zeroed AS (SELECT u.issue_id, u.key, u.value FROM upd u WHERE u.count = 0
             UNION ALL
             SELECT u.issue_id, u.key, u.value FROM upd2 u WHERE u.count = 0),
  dr AS (SELECT x.issue_id, sum(x.short)::bigint AS n
           FROM (SELECT nm.issue_id, GREATEST(nm.n - nm.cur, 0) AS short FROM named nm
                 UNION ALL
                 SELECT o.issue_id, CASE WHEN o.cur IS NULL THEN o.n ELSE GREATEST(o.n - o.cur, 0) END FROM ovf o) x
          GROUP BY x.issue_id HAVING sum(x.short) > 0)
  SELECT (SELECT array_agg(dr.issue_id ORDER BY dr.issue_id) FROM dr), (SELECT array_agg(dr.n ORDER BY dr.issue_id) FROM dr),
         z.i, z.k, z.v
    INTO drifted, drifted_n, z_i, z_k, z_v
    FROM (SELECT array_agg(zz.issue_id) AS i, array_agg(zz.key) AS k, array_agg(zz.value) AS v FROM zeroed zz) z;
  -- Delete exactly the rows the two UPDATEs brought to 0, by primary key (review finding
  -- tfs-rev-subtract-zero-delete-scan): never a scan of the touched issues' whole rollup.
  IF z_i IS NOT NULL THEN
    DELETE FROM issue_tag_values t USING unnest(z_i, z_k, z_v) AS z(issue_id, key, value)
     WHERE t.issue_id = z.issue_id AND t.key = z.key AND t.value = z.value AND t.count = 0;
  END IF;
  IF drifted IS NULL THEN RETURN NULL; END IF;
  -- Inline rebuild only while the drifted issues' occurrences fit a budget of 10,000 (~55 ms of rebuild,
  -- which every issue this statement has locked waits for); a larger issue is recorded with
  -- rebuilt_at NULL and rebuilt later in its own transaction (spec §3.4 W11, §5).
  FOR i IN 1 .. cardinality(drifted) LOOP
    SELECT count(*) INTO sz FROM (SELECT 1 FROM error_occurrences o
                                   WHERE o.issue_id = drifted[i] LIMIT budget + 1) s;
    IF sz <= budget THEN small := small || drifted[i]; budget := budget - sz;
    ELSE deferred := deferred || drifted[i]; END IF;
  END LOOP;
  INSERT INTO issue_tag_values_drift (issue_id, contributions, source, rebuilt_at)
  SELECT u.i, u.n, p_source, CASE WHEN u.i = ANY(small) THEN now() END
    FROM unnest(drifted, drifted_n) AS u(i, n);
  RAISE WARNING 'issue_tag_values drift (%): rebuilt % issue(s) %, deferred % issue(s) %',
    p_source, coalesce(cardinality(small), 0), small, coalesce(cardinality(deferred), 0), deferred;
  IF small IS NOT NULL THEN PERFORM issue_tag_values_rebuild_locked(small); END IF;
  RETURN small;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION error_occurrences_tag_rollup_mark() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.tag_rollup_applied := true;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION error_occurrences_tag_rollup_ins() RETURNS trigger LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
DECLARE a record;
BEGIN
  PERFORM 1 FROM issues WHERE id IN (SELECT issue_id FROM tr_new) ORDER BY id FOR NO KEY UPDATE;
  SELECT array_agg(g.issue_id) AS i, array_agg(g.key) AS k, array_agg(g.value) AS v,
         array_agg(g.n) AS n, array_agg(g.fs) AS f, array_agg(g.ls) AS l INTO a
    FROM (SELECT o.issue_id, p.key, p.value, count(*)::bigint AS n,
                 min(coalesce(o.created_at, now())) AS fs, max(coalesce(o.created_at, now())) AS ls
            FROM tr_new o
            CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
           GROUP BY 1, 2, 3) g;
  IF a.i IS NOT NULL THEN PERFORM issue_tag_values_add(a.i, a.k, a.v, a.n, a.f, a.l); END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION error_occurrences_tag_rollup_del() RETURNS trigger LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
DECLARE a record; bf record;
BEGIN
  PERFORM 1 FROM issues WHERE id IN (SELECT issue_id FROM tr_old) ORDER BY id FOR NO KEY UPDATE;
  SELECT b.cursor_id, b.done INTO bf FROM issue_tag_values_backfill b;
  SELECT array_agg(g.issue_id) AS i, array_agg(g.key) AS k, array_agg(g.value) AS v, array_agg(g.n) AS n INTO a
    FROM (SELECT o.issue_id, p.key, p.value, count(*)::bigint AS n
            FROM tr_old o
            CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
           WHERE issue_tag_counted(o.tag_rollup_applied, o.id, bf.cursor_id, bf.done)
             AND EXISTS (SELECT 1 FROM issues i WHERE i.id = o.issue_id)
           GROUP BY 1, 2, 3) g;
  IF a.i IS NOT NULL THEN PERFORM issue_tag_values_subtract(a.i, a.k, a.v, a.n, 'delete'); END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION error_occurrences_tag_rollup_upd() RETURNS trigger LANGUAGE plpgsql SET plan_cache_mode = force_generic_plan AS $$
DECLARE a record; b record; bf record; rebuilt uuid[];
BEGIN
  PERFORM 1 FROM issues WHERE id IN (SELECT issue_id FROM tr_old UNION SELECT issue_id FROM tr_new)
    ORDER BY id FOR NO KEY UPDATE;
  SELECT bk.cursor_id, bk.done INTO bf FROM issue_tag_values_backfill bk;
  SELECT array_agg(x.issue_id) FILTER (WHERE x.n < 0) AS si, array_agg(x.key) FILTER (WHERE x.n < 0) AS sk,
         array_agg(x.value) FILTER (WHERE x.n < 0) AS sv, array_agg(-x.n) FILTER (WHERE x.n < 0) AS sn,
         array_agg(x.issue_id) FILTER (WHERE x.n > 0) AS ai, array_agg(x.key) FILTER (WHERE x.n > 0) AS ak,
         array_agg(x.value) FILTER (WHERE x.n > 0) AS av, array_agg(x.n) FILTER (WHERE x.n > 0) AS an,
         array_agg(x.fs) FILTER (WHERE x.n > 0) AS af, array_agg(x.ls) FILTER (WHERE x.n > 0) AS al INTO a
    FROM (SELECT c.issue_id, c.key, c.value, sum(c.s)::bigint AS n, min(c.at) AS fs, max(c.at) AS ls
            FROM (SELECT o.issue_id, p.key, p.value, -1 AS s, coalesce(o.created_at, now()) AS at
                    FROM tr_old o JOIN tr_new nw ON nw.id = o.id
                    CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
                   WHERE issue_tag_counted(o.tag_rollup_applied, o.id, bf.cursor_id, bf.done)
                     AND (o.issue_id, o.tags, o.contexts, o.event_user, o.release_version, o.environment, o.tag_rollup_applied)
                         IS DISTINCT FROM
                         (nw.issue_id, nw.tags, nw.contexts, nw.event_user, nw.release_version, nw.environment, nw.tag_rollup_applied)
                  UNION ALL
                  SELECT nw.issue_id, p.key, p.value, 1, coalesce(nw.created_at, now())
                    FROM tr_new nw JOIN tr_old o ON o.id = nw.id
                    CROSS JOIN LATERAL issue_tag_pairs(nw.tags, nw.contexts, nw.event_user, nw.release_version, nw.environment) p
                   WHERE issue_tag_counted(nw.tag_rollup_applied, nw.id, bf.cursor_id, bf.done)
                     AND (o.issue_id, o.tags, o.contexts, o.event_user, o.release_version, o.environment, o.tag_rollup_applied)
                         IS DISTINCT FROM
                         (nw.issue_id, nw.tags, nw.contexts, nw.event_user, nw.release_version, nw.environment, nw.tag_rollup_applied)
                 ) c
           GROUP BY 1, 2, 3
          HAVING sum(c.s) <> 0) x;
  IF a.si IS NOT NULL THEN rebuilt := issue_tag_values_subtract(a.si, a.sk, a.sv, a.sn, 'update'); END IF;
  IF a.ai IS NOT NULL THEN
    -- An issue the subtraction rebuilt (W11) already counts its post-update rows: no add for it.
    SELECT array_agg(u.i) AS i, array_agg(u.k) AS k, array_agg(u.v) AS v, array_agg(u.n) AS n,
           array_agg(u.f) AS f, array_agg(u.l) AS l INTO b
      FROM unnest(a.ai, a.ak, a.av, a.an, a.af, a.al) AS u(i, k, v, n, f, l)
     WHERE NOT (u.i = ANY(coalesce(rebuilt, '{}')));
    IF b.i IS NOT NULL THEN PERFORM issue_tag_values_add(b.i, b.k, b.v, b.n, b.f, b.l); END IF;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION error_occurrences_tag_rollup_trunc() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM issue_tag_values;
  RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_values_backfill_batch(p_limit int)
RETURNS TABLE (counted int, cursor_id uuid, done boolean) LANGUAGE plpgsql AS $$
DECLARE st record; ids uuid[]; last_id uuid; locked uuid[]; a record; n int; missing text;
BEGIN
  SELECT b.* INTO st FROM issue_tag_values_backfill b FOR UPDATE;
  IF st.done THEN RETURN QUERY SELECT 0, st.cursor_id, true; RETURN; END IF;
  SELECT string_agg(w.name, ', ') INTO missing
    FROM unnest(ARRAY['trg_error_occurrences_tag_rollup_mark', 'trg_error_occurrences_tag_rollup_ins',
                      'trg_error_occurrences_tag_rollup_del', 'trg_error_occurrences_tag_rollup_upd',
                      'trg_error_occurrences_tag_rollup_trunc']) AS w(name)
   WHERE NOT EXISTS (SELECT 1 FROM pg_trigger t
                      WHERE t.tgrelid = 'error_occurrences'::regclass AND t.tgname = w.name AND t.tgenabled <> 'D');
  IF missing IS NOT NULL THEN
    RAISE EXCEPTION 'issue_tag_values backfill refused: trigger(s) missing or disabled: %', missing;
  END IF;
  SELECT array_agg(c.id ORDER BY c.id) INTO ids FROM (
    SELECT o.id FROM error_occurrences o
     WHERE o.tag_rollup_applied IS NULL
       AND o.id > coalesce(st.cursor_id, '00000000-0000-0000-0000-000000000000'::uuid)
     ORDER BY o.id LIMIT p_limit) c;
  IF ids IS NULL THEN
    UPDATE issue_tag_values_backfill SET done = true, finished_at = now(), updated_at = now();
    RETURN QUERY SELECT 0, st.cursor_id, true; RETURN;
  END IF;
  last_id := ids[array_length(ids, 1)];
  SELECT array_agg(x.id ORDER BY x.id) INTO locked FROM (
    SELECT i.id FROM issues i
     WHERE i.id IN (SELECT o.issue_id FROM error_occurrences o WHERE o.id = ANY(ids))
     ORDER BY i.id FOR NO KEY UPDATE) x;
  IF EXISTS (SELECT 1 FROM error_occurrences o
              WHERE o.id = ANY(ids) AND NOT (o.issue_id = ANY(coalesce(locked, '{}')))) THEN
    RAISE EXCEPTION 'issue_tag_values backfill: an occurrence changed issue during the batch; retry'
      USING ERRCODE = '40001';
  END IF;
  SELECT count(*) INTO n FROM error_occurrences o WHERE o.id = ANY(ids) AND o.tag_rollup_applied IS NULL;
  SELECT array_agg(g.issue_id) AS i, array_agg(g.key) AS k, array_agg(g.value) AS v,
         array_agg(g.cnt) AS c, array_agg(g.fs) AS f, array_agg(g.ls) AS l INTO a
    FROM (SELECT o.issue_id, p.key, p.value, count(*)::bigint AS cnt,
                 min(coalesce(o.created_at, now())) AS fs, max(coalesce(o.created_at, now())) AS ls
            FROM error_occurrences o
            CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
           WHERE o.id = ANY(ids) AND o.tag_rollup_applied IS NULL
           GROUP BY 1, 2, 3) g;
  IF a.i IS NOT NULL THEN PERFORM issue_tag_values_add(a.i, a.k, a.v, a.c, a.f, a.l); END IF;
  UPDATE issue_tag_values_backfill b
     SET cursor_id = last_id, rows_counted = b.rows_counted + n,
         started_at = coalesce(b.started_at, now()), updated_at = now();
  RETURN QUERY SELECT n, last_id, false;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION issue_tag_values_rebuild(p_issue uuid) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE total bigint;
BEGIN
  PERFORM 1 FROM issues WHERE id = p_issue FOR UPDATE;
  total := issue_tag_values_rebuild_locked(ARRAY[p_issue]);
  UPDATE issue_tag_values_drift SET rebuilt_at = now() WHERE issue_id = p_issue AND rebuilt_at IS NULL;
  RETURN total;
END $$;
-- +goose StatementEnd

-- block I1: drop-if-invalid, then the concurrent build, then the validity check (the 1725300000 pattern).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('idx_issue_tag_values_key_value') AND NOT indisvalid) THEN
        ALTER INDEX idx_issue_tag_values_key_value RENAME TO idx_issue_tag_values_key_value_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_tag_values_key_value_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_tag_values_key_value ON issue_tag_values (key, value, issue_id);
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index WHERE indexrelid = to_regclass('idx_issue_tag_values_key_value');
    IF valid IS NULL THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_issue_tag_values_key_value does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_issue_tag_values_key_value exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- The four error_occurrences builds scan its heap twice each: 60 min, not 15 (spec §8.1).
-- +goose StatementBegin
SET statement_timeout = '60min';
-- +goose StatementEnd

-- block I2 (search branch (b) for user.id): drop-if-invalid, then the concurrent build, then the validity check (the 1725300000 pattern).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_user_id') AND NOT indisvalid) THEN
        ALTER INDEX idx_error_occurrences_user_id RENAME TO idx_error_occurrences_user_id_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_user_id_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_user_id
    ON error_occurrences ((event_user->>'id'), issue_id)
    WHERE jsonb_typeof(event_user->'id') = 'string';
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_user_id');
    IF valid IS NULL THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_user_id does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_user_id exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- block I3 (search branch (b) for device.model): drop-if-invalid, then the concurrent build, then the validity check (the 1725300000 pattern).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_device_model') AND NOT indisvalid) THEN
        ALTER INDEX idx_error_occurrences_device_model RENAME TO idx_error_occurrences_device_model_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_device_model_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_device_model
    ON error_occurrences ((contexts->'device'->>'model'), issue_id)
    WHERE jsonb_typeof(contexts->'device'->'model') = 'string';
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_device_model');
    IF valid IS NULL THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_device_model does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_device_model exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- block I4 (a platform filter combined with a tag): drop-if-invalid, then the concurrent build, then the validity check (the 1725300000 pattern).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_issue_platform') AND NOT indisvalid) THEN
        ALTER INDEX idx_error_occurrences_issue_platform RENAME TO idx_error_occurrences_issue_platform_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_platform_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_issue_platform
    ON error_occurrences (issue_id, platform, created_at DESC, id DESC);
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_issue_platform');
    IF valid IS NULL THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_issue_platform does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_issue_platform exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- block I5 (search branch (b) for custom tags, issue-scoped): drop-if-invalid, then the concurrent build, then the validity check (the 1725300000 pattern).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_issue_tags') AND NOT indisvalid) THEN
        ALTER INDEX idx_error_occurrences_issue_tags RENAME TO idx_error_occurrences_issue_tags_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_tags_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_issue_tags
    ON error_occurrences USING gin ((jsonb_set('{}'::jsonb, ARRAY[issue_id::text], tags)) jsonb_path_ops)
    WHERE tags IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index WHERE indexrelid = to_regclass('idx_error_occurrences_issue_tags');
    IF valid IS NULL THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_issue_tags does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION 'add_issue_tag_values: idx_error_occurrences_issue_tags exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- +goose StatementBegin
SET statement_timeout = '15min';
-- +goose StatementEnd

-- block TR: the five triggers, in one DO block (one transaction), bounded wait.
-- +goose StatementBegin
DO $$
-- block TR: bounded lock wait, as block C.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      CREATE OR REPLACE TRIGGER trg_error_occurrences_tag_rollup_mark BEFORE INSERT ON error_occurrences
        FOR EACH ROW EXECUTE FUNCTION error_occurrences_tag_rollup_mark();
      CREATE OR REPLACE TRIGGER trg_error_occurrences_tag_rollup_ins AFTER INSERT ON error_occurrences
        REFERENCING NEW TABLE AS tr_new FOR EACH STATEMENT EXECUTE FUNCTION error_occurrences_tag_rollup_ins();
      CREATE OR REPLACE TRIGGER trg_error_occurrences_tag_rollup_del AFTER DELETE ON error_occurrences
        REFERENCING OLD TABLE AS tr_old FOR EACH STATEMENT EXECUTE FUNCTION error_occurrences_tag_rollup_del();
      CREATE OR REPLACE TRIGGER trg_error_occurrences_tag_rollup_upd AFTER UPDATE ON error_occurrences
        REFERENCING OLD TABLE AS tr_old NEW TABLE AS tr_new FOR EACH STATEMENT EXECUTE FUNCTION error_occurrences_tag_rollup_upd();
      CREATE OR REPLACE TRIGGER trg_error_occurrences_tag_rollup_trunc AFTER TRUNCATE ON error_occurrences
        FOR EACH STATEMENT EXECUTE FUNCTION error_occurrences_tag_rollup_trunc();
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE '1725400000 block TR: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd

-- block R
-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd
-- +goose StatementBegin
RESET statement_timeout;
-- +goose StatementEnd

-- +goose Down
-- Runs only after the processor and the dashboard are rolled back (header, ROLLBACK ORDER).
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
-- triggers: bounded lock wait, as Up's block C.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  IF to_regclass('error_occurrences') IS NULL THEN RETURN; END IF;
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      DROP TRIGGER IF EXISTS trg_error_occurrences_tag_rollup_mark ON error_occurrences;
      DROP TRIGGER IF EXISTS trg_error_occurrences_tag_rollup_ins ON error_occurrences;
      DROP TRIGGER IF EXISTS trg_error_occurrences_tag_rollup_del ON error_occurrences;
      DROP TRIGGER IF EXISTS trg_error_occurrences_tag_rollup_upd ON error_occurrences;
      DROP TRIGGER IF EXISTS trg_error_occurrences_tag_rollup_trunc ON error_occurrences;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE 'add_issue_tag_values down, triggers: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_values_rebuild(uuid);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_values_backfill_batch(int);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS error_occurrences_tag_rollup_trunc();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS error_occurrences_tag_rollup_upd();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS error_occurrences_tag_rollup_del();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS error_occurrences_tag_rollup_ins();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS error_occurrences_tag_rollup_mark();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_values_subtract(uuid[], text[], text[], bigint[], text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_values_rebuild_locked(uuid[]);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_values_add(uuid[], text[], text[], bigint[], timestamptz[], timestamptz[]);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_counted(boolean, uuid, uuid, boolean);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_pairs(jsonb, jsonb, jsonb, text, text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS issue_tag_derived(jsonb, jsonb, text, text);
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_tags;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_tags_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_platform;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_issue_platform_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_device_model;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_device_model_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_user_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_user_id_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_tag_values_key_value;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_tag_values_key_value_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS issue_tag_values_drift;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS issue_tag_values_backfill;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
-- issue_tag_values: bounded lock wait, as Up's block C. Its REFERENCES issues(id) FK means dropping the
-- table takes AccessExclusiveLock on issues too (measured: BEGIN; DROP TABLE issue_tag_values; pg_locks
-- shows issues AccessExclusiveLock) — the same queue-stall risk §8.1 bounds for Up's blocks C/T1/TR,
-- just as unbounded as the column drop below if left a plain statement.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'issue_tag_values' AND relkind = 'r') THEN RETURN; END IF;
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      DROP TABLE IF EXISTS issue_tag_values;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE 'add_issue_tag_values down, issue_tag_values: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
-- column: bounded lock wait, as Up's block C.
DECLARE attempt int := 0;
        per_try text := ((extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 500) || 'ms';
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'error_occurrences'::regclass AND attname = 'tag_rollup_applied' AND NOT attisdropped) THEN RETURN; END IF;
  LOOP
    attempt := attempt + 1;
    BEGIN
      PERFORM set_config('lock_timeout', per_try, true);
      ALTER TABLE error_occurrences DROP COLUMN IF EXISTS tag_rollup_applied;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF attempt >= 20 THEN RAISE; END IF;
      RAISE NOTICE 'add_issue_tag_values down, column: lock not granted within % (attempt %), retrying', per_try, attempt;
    END;
    PERFORM pg_sleep(5 + random());
  END LOOP;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd

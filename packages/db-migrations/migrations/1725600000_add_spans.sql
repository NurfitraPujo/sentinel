-- +goose Up
-- +goose StatementBegin
-- sentinel-span-ingest D1/D3 (contract prd/sentinel-span-ingest): spans for the trace waterfall, and the durable queue
-- of traces to pull from core's trace store.
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database (A1), so this file
-- is replayed. Every statement is IF NOT EXISTS / CREATE OR REPLACE, and partitions are created by a function that
-- skips the ones that exist.

-- D1: declaratively partitioned by day on start_ts so the 14-day retention is a DROP of whole partitions, never a
-- row-by-row DELETE on a table that takes millions of rows. The primary key must contain the partition key, which
-- is also the dedupe key: (org_id, trace_id, span_id, start_ts), written with ON CONFLICT DO NOTHING. org_id is in
-- the key so another organization can neither pre-insert a span with the same ids and start to block this one's row
-- nor make a second allow-listed organization pulling the same trace store nothing.
--
-- org_id has NO foreign key on purpose: a FK from a partitioned table would take a lock on organizations for every
-- insert burst and make DROP PARTITION check nothing useful. A deleted organization's spans age out with the window.
-- project_id is nullable: a span pulled from core's trace store belongs to an organization, never to a project.
CREATE TABLE IF NOT EXISTS spans (
    org_id         UUID        NOT NULL,
    project_id     UUID,
    environment    TEXT        NOT NULL DEFAULT '',
    source         TEXT        NOT NULL CHECK (source IN ('mobile', 'pulled')),
    service_name   TEXT        NOT NULL DEFAULT '',
    trace_id       TEXT        NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    span_id        TEXT        NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$'),
    parent_span_id TEXT        NOT NULL DEFAULT '',
    name           TEXT        NOT NULL DEFAULT '',
    op             TEXT        NOT NULL DEFAULT '',
    kind           TEXT        NOT NULL DEFAULT '',
    start_ts       TIMESTAMPTZ NOT NULL,
    duration_us    BIGINT      NOT NULL DEFAULT 0 CHECK (duration_us >= 0),
    status         TEXT        NOT NULL DEFAULT '',
    attributes     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    -- D8: a PULLED span is core's data and is governed by the organization's configured CORE project
    -- (SPAN_PULL_SCOPES): the viewer must reach that project and its environment grant on it must permit the
    -- span's environment, whichever project's error queued the trace. NULL for mobile spans, which carry project_id.
    scope_project_id UUID,
    PRIMARY KEY (org_id, trace_id, span_id, start_ts)
) PARTITION BY RANGE (start_ts);

-- The trace page reads by (org, trace); the PK already leads with trace_id, so this index is the org-scoped read path
-- and is created on the parent, i.e. once per partition.
CREATE INDEX IF NOT EXISTS idx_spans_trace_id ON spans (trace_id, org_id);

-- D1: the partition window. spans_ensure_partitions creates the day partitions from (today - back_days) through
-- (today + ahead_days), UTC, skipping any that exist; it returns how many it created. spans_drop_expired drops every
-- day partition whose upper bound is at or before (now - keep_days), returning how many. Both are called by the
-- hourly retention cron (apps/dashboard-web retention.ts) and by the processor at start-up; they live in SQL so the
-- two callers cannot disagree on partition naming or bounds. Partition names are spans_pYYYYMMDD and are parsed from
-- pg_class by the drop, never trusted from a caller.
CREATE OR REPLACE FUNCTION spans_ensure_partitions(back_days integer DEFAULT 1, ahead_days integer DEFAULT 2)
RETURNS integer LANGUAGE plpgsql AS $fn$
DECLARE
    today   date := (now() AT TIME ZONE 'UTC')::date;
    d       date;
    created integer := 0;
    pname   text;
BEGIN
    FOR d IN SELECT g::date FROM generate_series(today - back_days, today + ahead_days, interval '1 day') AS g LOOP
        pname := 'spans_p' || to_char(d, 'YYYYMMDD');
        IF to_regclass(pname) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF spans FOR VALUES FROM (%L) TO (%L)',
                pname,
                to_char(d, 'YYYY-MM-DD') || ' 00:00:00+00',
                to_char(d + 1, 'YYYY-MM-DD') || ' 00:00:00+00');
            created := created + 1;
        END IF;
    END LOOP;
    RETURN created;
END
$fn$;

CREATE OR REPLACE FUNCTION spans_drop_expired(keep_days integer DEFAULT 14)
RETURNS integer LANGUAGE plpgsql AS $fn$
DECLARE
    cutoff  date := ((now() AT TIME ZONE 'UTC') - make_interval(days => keep_days))::date;
    rec     record;
    dropped integer := 0;
BEGIN
    -- A partition covering day D holds spans with start_ts in [D, D+1). It is wholly older than the window when
    -- D + 1 <= cutoff, i.e. D < cutoff.
    FOR rec IN
        SELECT c.relname
          FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = 'spans'::regclass
           AND c.relname ~ '^spans_p[0-9]{8}$'
           AND to_date(substr(c.relname, 8), 'YYYYMMDD') < cutoff
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I', rec.relname);
        dropped := dropped + 1;
    END LOOP;
    RETURN dropped;
END
$fn$;

SELECT spans_ensure_partitions(1, 2);

-- D3: the durable pull queue, one row per (organization, trace). A row is the dedupe: a trace whose row exists is not
-- enqueued again, and rows are deleted after 14 days by the retention cron. stage 0 = first fetch pending (2 minutes
-- after the first error), 1 = second fetch pending (10 minutes after it), 2 = done. next_fetch_at doubles as the
-- claim lease: a worker pushes it forward while it works, so a crashed worker's row simply becomes due again.
CREATE TABLE IF NOT EXISTS span_pull_queue (
    org_id        UUID        NOT NULL,
    trace_id      TEXT        NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    stage         SMALLINT    NOT NULL DEFAULT 0 CHECK (stage BETWEEN 0 AND 2),
    attempts      INTEGER     NOT NULL DEFAULT 0,
    next_fetch_at TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error    TEXT        NOT NULL DEFAULT '',
    -- The project of the FIRST error that queued this trace; ON CONFLICT DO NOTHING keeps it stable. For debugging
    -- only: it scopes nothing (see spans.scope_project_id).
    trigger_project_id UUID,
    PRIMARY KEY (org_id, trace_id)
);

CREATE INDEX IF NOT EXISTS idx_span_pull_queue_due ON span_pull_queue (next_fetch_at) WHERE stage < 2;
CREATE INDEX IF NOT EXISTS idx_span_pull_queue_created_at ON span_pull_queue (created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS span_pull_queue;
DROP TABLE IF EXISTS spans CASCADE;
DROP FUNCTION IF EXISTS spans_drop_expired(integer);
DROP FUNCTION IF EXISTS spans_ensure_partitions(integer, integer);
-- +goose StatementEnd

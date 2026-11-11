-- sentinel-affected-users §4.6 — the affected users / tenants rollup.
--
-- Two side tables holding one row per DISTINCT (issue, user) and (issue, tenant) pair, and two
-- counters on `issues` that are, by definition (decision 12), the cardinality of those tables:
-- `affected_user_count = (SELECT count(*) FROM issue_affected_users WHERE issue_id = issues.id)`
-- at every point outside an open transaction. The processor bumps the counter only when its
-- upsert INSERTs (§4.3) and the retention sweep recomputes it in the same transaction as its
-- DELETE (§4.5), so the invariant holds at every commit boundary.
--
-- No backfill statement, and none is to be added (decision 4): the only historical source is
-- `error_search_index`, whose user_id/tenant_id columns hold the NORMALISED placeholders
-- (`<UUID>`, `<EMAIL>`), so a backfill would count one user per issue. Pre-existing issues start
-- at 0 and the DEFAULT below is what puts them there.
--
-- IF NOT EXISTS throughout: same re-runnability rationale as 1722000000_add_api_key_management.sql
-- — one flat migrations directory serves several goose ledgers against the SAME physical database
-- (A1, hazard sentinel_goose_target), so every statement here is replayed per target. Nothing in
-- this file needs a pg_constraint guard: the CHECKs and the composite PRIMARY KEYs are inline on
-- CREATE TABLE IF NOT EXISTS, so they exist exactly when the table does.

-- +goose Up
-- +goose StatementBegin
-- Same values and reasoning as 1724400000_add_project_environments.sql: ADD COLUMN takes an
-- ACCESS EXCLUSIVE lock on `issues`, the table every ingest writes to. 5s is short enough that a
-- database where the lock is NOT immediately available fails fast and visibly instead of stalling
-- ingest behind a lock wait; 5min on the statement is a ceiling, not a target.
SET lock_timeout = '5s';
SET statement_timeout = '5min';
-- +goose StatementEnd

-- +goose StatementBegin
-- PRIMARY KEY (issue_id, user_id) is the ON CONFLICT target of §4.3's upsert and the thing that
-- makes "distinct" a database fact; it also serves the cascade and the recompute's
-- `WHERE a.issue_id =`. CHECK (user_id <> '') is decision 5's tripwire: the processor maps an
-- empty id to "absent" BEFORE the statement, and the constraint makes a regression of that loud
-- instead of a phantom user named "". VARCHAR(255) matches error_search_index.user_id, the
-- column these values were always meant for.
CREATE TABLE IF NOT EXISTS issue_affected_users (
    issue_id   UUID         NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id    VARCHAR(255) NOT NULL CHECK (user_id <> ''),
    first_seen TIMESTAMPTZ  NOT NULL,
    last_seen  TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (issue_id, user_id)
);
-- For §4.5's sweep (`WHERE last_seen < cutoff`), which is otherwise a sequential scan of the side
-- table every hour.
CREATE INDEX IF NOT EXISTS idx_issue_affected_users_last_seen
    ON issue_affected_users (last_seen);

CREATE TABLE IF NOT EXISTS issue_affected_tenants (
    issue_id   UUID         NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    tenant_id  VARCHAR(255) NOT NULL CHECK (tenant_id <> ''),
    first_seen TIMESTAMPTZ  NOT NULL,
    last_seen  TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (issue_id, tenant_id)
);
CREATE INDEX IF NOT EXISTS idx_issue_affected_tenants_last_seen
    ON issue_affected_tenants (last_seen);
-- +goose StatementEnd

-- +goose StatementBegin
-- Constant defaults: a metadata-only ADD COLUMN on PostgreSQL >= 11, no table rewrite. The DEFAULT
-- stays (unlike the environments migration's) because 0 is the correct value for every row that
-- predates the count (decision 4) and for every future INSERT path that does not know about it.
ALTER TABLE issues ADD COLUMN IF NOT EXISTS affected_user_count   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN IF NOT EXISTS affected_tenant_count INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose StatementBegin
-- §4.1's `sort=affected_users` order under the existing project/environment predicate. The key
-- order is §4.1's ORDER BY in full, `id DESC` included (decision 22), so the sorted, paginated
-- read can be an index range scan. PROPOSED in the contract: the architect may drop it if M1's
-- plan shows it unused at the seed size — say so rather than deleting it quietly.
CREATE INDEX IF NOT EXISTS idx_issues_project_env_affected_users
    ON issues (project_id, environment, affected_user_count DESC, last_seen DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '5min';
DROP INDEX IF EXISTS idx_issues_project_env_affected_users;
ALTER TABLE issues DROP COLUMN IF EXISTS affected_tenant_count;
ALTER TABLE issues DROP COLUMN IF EXISTS affected_user_count;
DROP TABLE IF EXISTS issue_affected_tenants;
DROP TABLE IF EXISTS issue_affected_users;
-- +goose StatementEnd

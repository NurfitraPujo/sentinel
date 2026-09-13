-- sentinel-affected-users BUG-01 (review finding R1 on 1724500000_add_issue_affected_rollup.sql:74).
--
-- §4.1's `sort=affected_users` has TWO request shapes and §4.6 indexed one of them.
-- `idx_issues_project_env_affected_users (project_id, environment, affected_user_count DESC,
-- last_seen DESC, id DESC)` serves `?project=<id>&environment=<x>&sort=affected_users`: with an
-- equality on both leading keys the ORDER BY is the index order and the paginated read is one
-- range scan. §13 M1's NAMED request is `?project=<id>&status=all&sort=affected_users` — i.e.
-- environment=all, no predicate on `environment` at all — and a btree cannot produce the ORDER BY
-- from that index without an equality on its second key: the planner falls back to a scan of the
-- project's rows plus a top-N sort at ANY size (measured 8.3×–10.2× the `sort=last_seen` request
-- on the committed seed, against a 2.0× bound), for a structural reason, not a size reason.
--
-- The fix is the second index the finding names: the same key order without `environment`, so the
-- environment=all form is a range scan on `project_id` in exactly §4.1's ORDER BY — `id DESC` last,
-- unchanged (decision 22). `idx_issues_project_env_affected_users` STAYS: with an environment
-- equality its key order is the tighter range (the environment is pinned by the index, not
-- filtered row by row), and dropping it would be a §4.6 change this item does not sanction.
-- Neither counter's semantics moves (decision 12).
--
-- Its own migration rather than an edit to 1724500000: that file is on a shared branch and applied
-- on every database that has run it, and goose never replays an applied version, so an edit there
-- would reach no existing database. IF NOT EXISTS, same reason as every migration since
-- 1722000000_add_api_key_management.sql (A1, hazard sentinel_goose_target).

-- +goose Up
-- +goose StatementBegin
-- Same values and reasoning as 1724500000: CREATE INDEX takes a SHARE lock on `issues`, the table
-- every ingest writes to; fail fast and visibly if it is not available rather than stall ingest.
SET lock_timeout = '5s';
SET statement_timeout = '5min';
CREATE INDEX IF NOT EXISTS idx_issues_project_affected_users
    ON issues (project_id, affected_user_count DESC, last_seen DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '5min';
DROP INDEX IF EXISTS idx_issues_project_affected_users;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin
-- D22: no self-relation guard existed anywhere (not the endpoint, not createIssueRelation, no DB
-- CHECK). A direct POST could create 'A duplicate_of A'. Add a DB-level CHECK constraint as the
-- last line of defense, matching the pattern used for other issue_relations constraints in
-- 1721900000_add_issue_lifecycle_and_relations.sql. The application-level guard lives in
-- apps/dashboard-web/src/routes/api/issues/[issueId]/relations/+server.ts.
ALTER TABLE issue_relations
  ADD CONSTRAINT issue_relations_no_self_relation CHECK (source_issue_id <> target_issue_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE issue_relations
  DROP CONSTRAINT IF EXISTS issue_relations_no_self_relation;
-- +goose StatementEnd

package dbmigrations

// sentinel-relay-key-trust TASK-01 (contract §4.1): 1725500000 widens project_api_keys_scope_check with 'relay'.
// Like the oncall tests, it needs a DISPOSABLE Postgres (MIGRATIONS_TEST_DB_URL, see that file's header) and
// self-skips without one; MIGRATIONS_TEST_REQUIRED=1 turns "no database" into a failure.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	relayBeforeVersion int64 = 1725400000
	relayScopeVersion  int64 = 1725500000
)

func TestRelayScopeMigration(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "relay")
	defer cleanup()

	migrateTo(t, db, relayBeforeVersion)
	orgID := seedRelayOrg(t, db)
	seedKey(t, db, orgID, "pre-ingest", "ingest")
	assertKeyRejected(t, db, orgID, "relay-before", "relay")

	migrateTo(t, db, relayScopeVersion)

	// Up touched no existing row, and a relay key can now be stored, next to every earlier scope.
	assertScopeOf(t, db, "pre-ingest", "ingest")
	projectID := seedRelayProject(t, db, orgID)
	seedProjectKey(t, db, orgID, projectID, "the-relay", "relay")
	// A relay key must belong to a project: an org-wide one is rejected, an org-wide key of any other scope is not.
	assertKeyRejected(t, db, orgID, "orgwide-relay", "relay")
	for _, scope := range []string{"ingest", "read", "admin", "agent", "report"} {
		seedKey(t, db, orgID, "post-"+scope, scope)
	}
	assertKeyRejected(t, db, orgID, "bogus", "superuser")

	// Replay (one flat directory serves several goose ledgers against the same database): the file's SQL
	// runs again over an already-widened constraint without error.
	replayRelayScopeUp(t, db)
	assertScopeOf(t, db, "the-relay", "relay")
	assertKeyRejected(t, db, orgID, "orgwide-relay-replay", "relay")

	// Down: a relay row would violate the restored set, so it is demoted to ingest, and the constraint is
	// back to the 1724100000 set.
	if err := goose.DownToContext(context.Background(), db, migrationsDir(), relayBeforeVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", relayBeforeVersion, err)
	}
	assertScopeOf(t, db, "the-relay", "ingest")
	assertKeyRejected(t, db, orgID, "relay-after-down", "relay")
	if err := keyInsertProject(db, orgID, projectID, "relay-after-down-project", "relay"); err == nil {
		t.Fatal("a project relay key must be rejected by the restored scope check after down")
	}
	assertNoConstraint(t, db, "project_api_keys_relay_project_check")

	migrateTo(t, db, relayScopeVersion)
	seedProjectKey(t, db, orgID, projectID, "relay-again", "relay")
	assertKeyRejected(t, db, orgID, "orgwide-relay-again", "relay")
}

func seedRelayOrg(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO organizations (name, slug) VALUES ('relay-org', 'relay-org') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	return id
}

func seedRelayProject(t *testing.T, db *sql.DB, orgID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO projects (name, organization_id, api_key, api_key_hash) VALUES ('relay-project', $1, 'relay-pk', 'relay-pk-hash') RETURNING id`, orgID).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func keyInsertProject(db *sql.DB, orgID, projectID, name, scope string) error {
	_, err := db.Exec(
		`INSERT INTO project_api_keys (organization_id, project_id, name, key_prefix, key_hash, scope, created_by)
		 VALUES ($1, $2, $3, 'sent_', $3, $4, 'test')`, orgID, projectID, name, scope)
	return err
}

func seedProjectKey(t *testing.T, db *sql.DB, orgID, projectID, name, scope string) {
	t.Helper()
	if err := keyInsertProject(db, orgID, projectID, name, scope); err != nil {
		t.Fatalf("a project %q key must be storable here: %v", scope, err)
	}
}

func assertNoConstraint(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	if n != 0 {
		t.Fatalf("constraint %s must be gone after down", name)
	}
}

func keyInsert(db *sql.DB, orgID, name, scope string) error {
	_, err := db.Exec(
		`INSERT INTO project_api_keys (organization_id, name, key_prefix, key_hash, scope, created_by)
		 VALUES ($1, $2, 'sent_', $2, $3, 'test')`, orgID, name, scope)
	return err
}

func seedKey(t *testing.T, db *sql.DB, orgID, name, scope string) {
	t.Helper()
	if err := keyInsert(db, orgID, name, scope); err != nil {
		t.Fatalf("a %q key must be storable here: %v", scope, err)
	}
}

func assertKeyRejected(t *testing.T, db *sql.DB, orgID, name, scope string) {
	t.Helper()
	if err := keyInsert(db, orgID, name, scope); err == nil {
		t.Fatalf("a %q key must be rejected by project_api_keys_scope_check here", scope)
	}
}

func assertScopeOf(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT scope FROM project_api_keys WHERE name = $1`, name).Scan(&got); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("key %s has scope %q, want %q", name, got, want)
	}
}

// replayRelayScopeUp re-executes the Up block of 1725500000 verbatim.
func replayRelayScopeUp(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;
  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent', 'report', 'relay')
  );
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_relay_project_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_relay_project_check CHECK (
      scope <> 'relay' OR project_id IS NOT NULL
    );
  END IF;
END $$`); err != nil {
		t.Fatalf("replay of the up block failed: %v", err)
	}
}

package dbmigrations

// sentinel-cli-browser-login TASK-01: 1725900000 adds personal agents (agents.owner_user_id + kind 'personal') and the
// cli_device_authorizations table. Needs a DISPOSABLE Postgres (MIGRATIONS_TEST_DB_URL, see migrations_oncall_weekday_test.go's
// header) and self-skips without one.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

const cliDeviceLoginVersion int64 = 1725900000

func TestCLIDeviceLoginMigration(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "clidev")
	defer cleanup()
	migrateTo(t, db, cliDeviceLoginVersion-1)

	const org = "22222222-2222-2222-2222-222222222222"
	const user = "user-cbl-1"
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, 'cbl', 'cbl')`, org)
	mustExec(`INSERT INTO "user" (id, name, email) VALUES ($1, 'Ada', 'ada@example.test')`, user)
	// An org agent that exists BEFORE the migration must keep validating under the new constraints.
	mustExec(`INSERT INTO agents (org_id, name, kind, created_by) VALUES ($1, 'ci', 'ai', $2)`, org, user)

	migrateTo(t, db, cliDeviceLoginVersion)

	personal := `INSERT INTO agents (org_id, name, kind, created_by, owner_user_id) VALUES ($1, 'Ada (CLI)', $2, $3, $4)`
	if _, err := db.Exec(personal, org, "personal", user, nil); err == nil {
		t.Fatal("a personal agent without an owner must violate agents_personal_owner_check")
	}
	if _, err := db.Exec(personal, org, "ai", user, user); err == nil {
		t.Fatal("an org agent WITH an owner must violate agents_personal_owner_check")
	}
	mustExec(personal, org, "personal", user, user)
	if _, err := db.Exec(personal, org, "personal", user, user); err == nil {
		t.Fatal("a second personal agent for the same (org, user) must violate uq_agents_personal_owner")
	}

	device := `INSERT INTO cli_device_authorizations (device_code_hash, user_code, device_name, expires_at, status)
	           VALUES ($1, $2, 'laptop', now() + interval '10 minutes', $3)`
	mustExec(device, "h1", "BCDFGHJK", "pending")
	if _, err := db.Exec(device, "h2", "BCDFGHJK", "pending"); err == nil {
		t.Fatal("two PENDING rows must not share a user_code")
	}
	mustExec(device, "h3", "BCDFGHJK", "delivered") // a used code may repeat
	if _, err := db.Exec(device, "h4", "ABCD1234", "pending"); err == nil {
		t.Fatal("a user_code outside the consonant alphabet must violate the CHECK")
	}
	if _, err := db.Exec(device, "h5", "BCDFGHJL", "expired"); err == nil {
		t.Fatal("an unknown status must violate the CHECK")
	}

	// Replay: the Up runs again over the existing column, constraints, index and table, keeping their rows.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version_id = $1`, cliDeviceLoginVersion); err != nil {
		t.Fatalf("forget the version: %v", err)
	}
	migrateTo(t, db, cliDeviceLoginVersion)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM cli_device_authorizations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("device rows after a replay = %d (%v), want 2", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM agents WHERE kind = 'personal'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("personal agents after a replay = %d (%v), want 1", n, err)
	}

	// Down removes the table, the personal agent and the column, and restores the old CHECK; up again recreates them.
	if err := goose.DownToContext(context.Background(), db, migrationsDir(), cliDeviceLoginVersion-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('cli_device_authorizations')::text`).Scan(&reg); err != nil || reg.Valid {
		t.Fatalf("cli_device_authorizations after down: %v, %v; want dropped", reg, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'agents' AND column_name = 'owner_user_id'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("owner_user_id after down = %d (%v), want 0", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM agents`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("agents after down = %d (%v), want only the org agent", n, err)
	}
	if _, err := db.Exec(`INSERT INTO agents (org_id, name, kind, created_by) VALUES ($1, 'x', 'personal', $2)`, org, user); err == nil {
		t.Fatal("after down, kind 'personal' must violate the restored CHECK")
	}
	migrateTo(t, db, cliDeviceLoginVersion)
}

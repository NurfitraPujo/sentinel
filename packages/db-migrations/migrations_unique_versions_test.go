package dbmigrations

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMigrationVersionsAreUnique pins the one property of migrations/ that git cannot: goose keys
// its ledger on the leading integer of the filename, not on the filename, so two files at one
// version are a runtime error ("goose: duplicate version") and NOT a merge conflict. That is how
// 1724300000_add_project_environments.sql landed beside 1724300000_add_repo_credential_project_scope.sql
// — both merged cleanly, and nothing on a pull request executes a migration (the only CI step that
// does is gated `github.event_name != 'pull_request'`), so the collision was found by reading.
// This test needs no database and runs in the `go-migrations` job, which is where a version
// collision has to be caught to block the merge that introduces it.
func TestMigrationVersionsAreUnique(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	seen := map[int64]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".sql" {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Errorf("%s: goose needs <version>_<name>.sql", name)
			continue
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Errorf("%s: version prefix %q is not an integer: %v", name, prefix, err)
			continue
		}
		if other, dup := seen[version]; dup {
			t.Errorf("goose version %d is claimed by both %s and %s; renumber the one that is not on main", version, other, name)
			continue
		}
		seen[version] = name
	}
}

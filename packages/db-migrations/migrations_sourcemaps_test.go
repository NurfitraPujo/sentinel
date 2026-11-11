package dbmigrations

// sentinel-sourcemaps §4.5 (TASK-01, criterion 1; contract §11's schema-drift row is the
// dashboard half). Replays the feature's one migration against a real Postgres in the style
// migrations_releases_snooze_test.go established for this package:
//
//	migrations/1725100000_add_sourcemaps.sql   the `sourcemaps` table with its inline CHECKs,
//	                                            composite UNIQUE and FK, the UNIQUE storage_key
//	                                            index and the (project_id, release_version)
//	                                            index -- DDL only, no lock guard (§4.5)
//
// up -> down -> up, then the whole directory again under two more goose ledgers on the SAME
// database -- the situation hazard sentinel_goose_target names: one flat directory, three
// ledgers, one physical database, so every statement runs again on a schema that already has
// every object it creates.
//
// What is asserted is the SHAPE the contract paints (the eight columns with their types, NOT
// NULLs and defaults; the PK; `release_version <> ''`, `minified_path <> ''`, `size_bytes > 0`;
// UNIQUE (project_id, release_version, minified_path); the FK ON DELETE CASCADE from projects and
// NO FK to releases; the two indexes with their exact definitions) and the behaviours only a
// live database can prove:
//
//   - the three CHECKs and the two UNIQUEs reject what they must, and a row that satisfies them
//     is accepted with `id` and `uploaded_at` filled by their defaults;
//   - a map for a version `releases` has NO row for is accepted (no FK to releases -- CI uploads
//     at build time, before the first occurrence);
//   - deleting a project cascades its sourcemaps; a `releases` row for the same version does
//     not have to exist for that either;
//   - Down removes the two indexes, the table and the ledger row, and the second Up recreates
//     everything from scratch;
//   - the file's text: no `lock_timeout` guard (the header says why -- a new table takes no lock
//     on `issues` or `error_occurrences`), IF NOT EXISTS on the table and both indexes, and no
//     ALTER TABLE (every constraint is inline, so no pg_constraint guard is needed).
//
// Same database contract as migrations_oncall_weekday_test.go: MIGRATIONS_TEST_DB_URL at a
// DISPOSABLE Postgres whose role may CREATE DATABASE, self-skipping without one,
// MIGRATIONS_TEST_REQUIRED=1 to make the skip a failure. Never the shared dev database.
//
//	MIGRATIONS_TEST_DB_URL='postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable' \
//	  GOWORK=off go test ./... -run 'Sourcemaps' -count=1 -v

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	// 1725000000 is the releases backfill -- the last version before this feature's file.
	sourcemapsBaseVersion int64 = 1725000000
	sourcemapsVersion     int64 = 1725100000

	sourcemapsFile = "1725100000_add_sourcemaps.sql"
)

// TestSourcemapsReplay drives the file up -> down -> up over a database seeded with a project and
// asserts the schema and the constraints in each state, then replays the directory under two
// more ledgers and asserts the table and the rows written in between are untouched.
func TestSourcemapsReplay(t *testing.T) {
	db, cleanup := freshMigrationDatabase(t, "sourcemaps")
	defer cleanup()

	ctx := context.Background()
	migrateTo(t, db, sourcemapsBaseVersion)
	// Every ledger is at the base version before the feature lands -- production's shape when
	// the migrate Job runs this release, and the only order the directory permits with issue
	// rows present (1724400000_add_project_environments.sql refuses to run on a database whose
	// `issues` has rows). Each ledger then applies exactly this feature's file.
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, sourcemapsBaseVersion)
	}
	assertSourcemapsShape(t, db, false)

	projectP, _ := seedProjectIssue(t, db, "sourcemaps-p", "issue-p")
	projectQ, _ := seedProjectIssue(t, db, "sourcemaps-q", "issue-q")

	// First up.
	migrateTo(t, db, sourcemapsVersion)
	assertSourcemapsShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", sourcemapsVersion, true)

	// A row that satisfies every constraint is accepted, with `id` and `uploaded_at` defaulted --
	// and `releases` has NO row for v1.0.0: a map is uploaded before the first occurrence of its
	// version exists (§4.5: no FK to releases).
	assertRowCount(t, db, "releases", 0)
	rowID := insertSourcemap(t, db, projectP, "v1.0.0", "/assets/index-3f2a1c.js", "sourcemaps/p/v1/index.map", 4193821)
	if rowID == "" {
		t.Fatal("the accepted row returned no id -- DEFAULT gen_random_uuid() did not fill it")
	}
	var uploadedAtIsNow bool
	if err := db.QueryRow(
		`SELECT uploaded_at BETWEEN now() - interval '1 minute' AND now() FROM sourcemaps WHERE id = $1`, rowID,
	).Scan(&uploadedAtIsNow); err != nil {
		t.Fatalf("read uploaded_at: %v", err)
	}
	if !uploadedAtIsNow {
		t.Error("uploaded_at was not defaulted to NOW()")
	}
	// The same (release, path) in ANOTHER project is a different row: the UNIQUE is per project,
	// which is the tenancy §11's route test asserts through the lookup.
	insertSourcemap(t, db, projectQ, "v1.0.0", "/assets/index-3f2a1c.js", "sourcemaps/q/v1/index.map", 4193821)
	// A second path of the same release, same bytes, is a second object (decision 17 folds the
	// path into the key), so the storage_key UNIQUE does not refuse it.
	insertSourcemap(t, db, projectP, "v1.0.0", "/assets/vendor-9a0d4e.js", "sourcemaps/p/v1/vendor.map", 4193821)
	assertRowCount(t, db, "sourcemaps", 3)

	// The constraints reject what they must, each by name.
	assertSourcemapsConstraintsReject(t, db, projectP)

	// Down: the two indexes, the table and the ledger row go; the seeded projects stay.
	if err := goose.DownToContext(ctx, db, migrationsDir(), sourcemapsBaseVersion); err != nil {
		t.Fatalf("goose down to %d failed: %v", sourcemapsBaseVersion, err)
	}
	assertSourcemapsShape(t, db, false)
	assertLedgerHas(t, db, "schema_migrations", sourcemapsVersion, false)
	assertRowCount(t, db, "projects", 2)

	// Second up: everything from scratch, on the same ledger.
	migrateTo(t, db, sourcemapsVersion)
	assertSourcemapsShape(t, db, true)
	assertLedgerHas(t, db, "schema_migrations", sourcemapsVersion, true)
	assertRowCount(t, db, "sourcemaps", 0)
	insertSourcemap(t, db, projectP, "v1.0.0", "/assets/index-3f2a1c.js", "sourcemaps/p/v1/index.map", 4193821)
	insertSourcemap(t, db, projectQ, "v1.0.0", "/assets/index-3f2a1c.js", "sourcemaps/q/v1/index.map", 4193821)

	// The second and third ledgers replay the file on a database that already has every object it
	// creates: the IF NOT EXISTS forms must make each statement a no-op, and the two rows written
	// in between must survive (a replay that dropped and recreated the table would lose them).
	for _, ledger := range []string{"dashboard_migrations", "processor_migrations"} {
		replayUnderLedger(t, db, ledger, sourcemapsVersion)
		assertLedgerHas(t, db, ledger, sourcemapsVersion, true)
		assertSourcemapsShape(t, db, true)
		assertRowCount(t, db, "sourcemaps", 2)
	}

	// ON DELETE CASCADE from projects: P's row goes with P; Q's stays.
	if _, err := db.Exec(`DELETE FROM projects WHERE id = $1`, projectP); err != nil {
		t.Fatalf("delete project P: %v", err)
	}
	assertRowCount(t, db, "sourcemaps", 1)
	var remaining string
	if err := db.QueryRow(`SELECT project_id FROM sourcemaps`).Scan(&remaining); err != nil {
		t.Fatalf("read the surviving row: %v", err)
	}
	if remaining != projectQ {
		t.Errorf("after deleting project P the surviving sourcemaps row belongs to %s, want Q (%s)", remaining, projectQ)
	}
}

// TestSourcemapsFileIsShapedAsDecided pins the file's text: no lock guard and a header that says
// why (§4.5 *Lock profile*), IF NOT EXISTS on the table and both indexes, no ALTER TABLE (every
// constraint inline), and the exact column bounds the contract states.
func TestSourcemapsFileIsShapedAsDecided(t *testing.T) {
	if got := firstGooseAnnotation(t, sourcemapsFile); got != "Up" {
		t.Errorf("%s: first goose annotation is %q, want \"Up\" -- this is a plain transactional DDL file", sourcemapsFile, got)
	}

	var (
		hasLockGuard, hasAlterTable                         bool
		hasTableIfNotExists, hasKeyIndex, hasProjectRelease bool
		hasReleaseCheck, hasPathCheck, hasSizeCheck         bool
		hasUnique, hasCascade, hasSha256, hasReleasesFK     bool
	)
	for _, line := range sqlLines(t, sourcemapsFile) {
		u := strings.ToUpper(strings.Join(strings.Fields(line), " "))
		hasLockGuard = hasLockGuard || strings.Contains(u, "LOCK_TIMEOUT")
		hasAlterTable = hasAlterTable || strings.Contains(u, "ALTER TABLE")
		hasTableIfNotExists = hasTableIfNotExists || strings.Contains(u, "CREATE TABLE IF NOT EXISTS SOURCEMAPS")
		hasKeyIndex = hasKeyIndex || strings.Contains(u, "CREATE UNIQUE INDEX IF NOT EXISTS IDX_SOURCEMAPS_STORAGE_KEY ON SOURCEMAPS (STORAGE_KEY)")
		hasProjectRelease = hasProjectRelease || strings.Contains(u, "CREATE INDEX IF NOT EXISTS IDX_SOURCEMAPS_PROJECT_RELEASE ON SOURCEMAPS (PROJECT_ID, RELEASE_VERSION)")
		hasReleaseCheck = hasReleaseCheck || strings.Contains(u, "RELEASE_VERSION VARCHAR(100) NOT NULL CHECK (RELEASE_VERSION <> '')")
		hasPathCheck = hasPathCheck || strings.Contains(u, "MINIFIED_PATH VARCHAR(512) NOT NULL CHECK (MINIFIED_PATH <> '')")
		hasSizeCheck = hasSizeCheck || strings.Contains(u, "SIZE_BYTES BIGINT NOT NULL CHECK (SIZE_BYTES > 0)")
		hasUnique = hasUnique || strings.Contains(u, "UNIQUE (PROJECT_ID, RELEASE_VERSION, MINIFIED_PATH)")
		hasCascade = hasCascade || strings.Contains(u, "REFERENCES PROJECTS(ID) ON DELETE CASCADE")
		hasSha256 = hasSha256 || strings.Contains(u, "SHA256 CHAR(64) NOT NULL")
		hasReleasesFK = hasReleasesFK || strings.Contains(u, "REFERENCES RELEASES")
	}
	if hasLockGuard {
		t.Errorf("%s: carries a lock_timeout guard; §4.5 says a new table takes no lock on issues or error_occurrences and the file says so in its header instead of copying one", sourcemapsFile)
	}
	if hasAlterTable {
		t.Errorf("%s: carries an ALTER TABLE; every constraint is inline on CREATE TABLE IF NOT EXISTS so no pg_constraint guard is needed (§4.5)", sourcemapsFile)
	}
	if hasReleasesFK {
		t.Errorf("%s: references releases; §4.5 says NO FK to releases -- a map is uploaded before the first occurrence of its version exists", sourcemapsFile)
	}
	if !hasTableIfNotExists || !hasKeyIndex || !hasProjectRelease {
		t.Errorf("%s: CREATE TABLE IF NOT EXISTS=%v, UNIQUE storage_key index=%v, (project_id, release_version) index=%v; all three must be present verbatim (§4.5)",
			sourcemapsFile, hasTableIfNotExists, hasKeyIndex, hasProjectRelease)
	}
	if !hasReleaseCheck || !hasPathCheck || !hasSizeCheck || !hasUnique || !hasCascade || !hasSha256 {
		t.Errorf("%s: release_version CHECK=%v, minified_path CHECK=%v, size_bytes CHECK=%v, composite UNIQUE=%v, FK ON DELETE CASCADE=%v, sha256 CHAR(64)=%v; all six must be present verbatim (§4.5)",
			sourcemapsFile, hasReleaseCheck, hasPathCheck, hasSizeCheck, hasUnique, hasCascade, hasSha256)
	}
	// The header must say why there is no guard, in the file, rather than leave the absence to be
	// read as an oversight.
	var saysNoLockGuard bool
	for _, line := range fileLines(t, sourcemapsFile) {
		u := strings.ToUpper(line)
		if strings.HasPrefix(strings.TrimSpace(u), "--") && strings.Contains(u, "NO LOCK GUARD") {
			saysNoLockGuard = true
			break
		}
	}
	if !saysNoLockGuard {
		t.Errorf("%s: the header does not say \"no lock guard\" and why (§4.5 *Lock profile*)", sourcemapsFile)
	}
}

// assertSourcemapsShape checks every object §4.5 creates is present (want=true) or absent
// (want=false): the table with its eight columns, the PK, the three CHECKs, the composite
// UNIQUE, the FK and the two indexes with their exact definitions.
func assertSourcemapsShape(t *testing.T, db *sql.DB, want bool) {
	t.Helper()

	assertTableExists(t, db, "sourcemaps", want)
	for _, index := range []string{"idx_sourcemaps_storage_key", "idx_sourcemaps_project_release"} {
		assertIndexExists(t, db, index, want)
	}
	if !want {
		return
	}

	assertColumnShape(t, db, "sourcemaps", "id", "uuid", "NO", "gen_random_uuid()")
	assertColumnShape(t, db, "sourcemaps", "project_id", "uuid", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "release_version", "character varying", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "minified_path", "character varying", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "storage_key", "character varying", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "size_bytes", "bigint", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "sha256", "character", "NO", "")
	assertColumnShape(t, db, "sourcemaps", "uploaded_at", "timestamp with time zone", "NO", "now()")
	for column, length := range map[string]int{"release_version": 100, "minified_path": 512, "storage_key": 1024, "sha256": 64} {
		var got int
		if err := db.QueryRow(
			`SELECT character_maximum_length FROM information_schema.columns WHERE table_name = 'sourcemaps' AND column_name = $1`, column,
		).Scan(&got); err != nil {
			t.Fatalf("introspect sourcemaps.%s length: %v", column, err)
		}
		if got != length {
			t.Errorf("sourcemaps.%s has length %d, want %d", column, got, length)
		}
	}

	assertConstraint(t, db, "sourcemaps", "sourcemaps_pkey", "PRIMARY KEY (id)")
	assertConstraint(t, db, "sourcemaps", "sourcemaps_release_version_check", "CHECK (((release_version)::text <> ''::text))")
	assertConstraint(t, db, "sourcemaps", "sourcemaps_minified_path_check", "CHECK (((minified_path)::text <> ''::text))")
	assertConstraint(t, db, "sourcemaps", "sourcemaps_size_bytes_check", "CHECK ((size_bytes > 0))")
	assertConstraint(t, db, "sourcemaps", "sourcemaps_project_id_release_version_minified_path_key",
		"UNIQUE (project_id, release_version, minified_path)")
	assertConstraint(t, db, "sourcemaps", "sourcemaps_project_id_fkey",
		"FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE")
	// NO FK to releases: exactly one foreign key on the table, and it points at projects.
	var fkCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_constraint WHERE conrelid = 'sourcemaps'::regclass AND contype = 'f'`,
	).Scan(&fkCount); err != nil {
		t.Fatalf("count sourcemaps foreign keys: %v", err)
	}
	if fkCount != 1 {
		t.Errorf("sourcemaps has %d foreign keys, want exactly 1 (projects; NO FK to releases -- §4.5)", fkCount)
	}

	assertIndexDefinition(t, db, "idx_sourcemaps_storage_key", "ON public.sourcemaps USING btree (storage_key)")
	var keyIndexIsUnique bool
	if err := db.QueryRow(
		`SELECT indisunique FROM pg_index WHERE indexrelid = 'idx_sourcemaps_storage_key'::regclass`,
	).Scan(&keyIndexIsUnique); err != nil {
		t.Fatalf("introspect idx_sourcemaps_storage_key: %v", err)
	}
	if !keyIndexIsUnique {
		t.Error("idx_sourcemaps_storage_key is not UNIQUE; each row must own exactly one object (§4.5)")
	}
	assertIndexDefinition(t, db, "idx_sourcemaps_project_release", "ON public.sourcemaps USING btree (project_id, release_version)")
}

// insertSourcemap writes one row with a fixed 64-hex sha256 and returns its defaulted id.
func insertSourcemap(t *testing.T, db *sql.DB, projectID, version, path, key string, size int64) string {
	t.Helper()

	var id string
	if err := db.QueryRow(
		`INSERT INTO sourcemaps (project_id, release_version, minified_path, storage_key, size_bytes, sha256)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		projectID, version, path, key, size, strings.Repeat("9f86d081", 8),
	).Scan(&id); err != nil {
		t.Fatalf("insert sourcemap (%s, %s, %s): %v", projectID, version, path, err)
	}
	return id
}

// assertSourcemapsConstraintsReject drives each constraint with the one row that violates only
// it, and names the constraint the error must carry.
func assertSourcemapsConstraintsReject(t *testing.T, db *sql.DB, projectID string) {
	t.Helper()

	sha := strings.Repeat("9f86d081", 8)
	row := func(version, path, key string, size int64) string {
		return `INSERT INTO sourcemaps (project_id, release_version, minified_path, storage_key, size_bytes, sha256) VALUES ('` +
			projectID + `', '` + version + `', '` + path + `', '` + key + `', ` + strconv.FormatInt(size, 10) + `, '` + sha + `')`
	}
	// release_version <> '' and minified_path <> '' (the two tripwires), size_bytes > 0.
	assertRejectedBy(t, db, row("", "/assets/a.js", "sourcemaps/x/a.map", 1), "sourcemaps_release_version_check")
	assertRejectedBy(t, db, row("v1.0.0", "", "sourcemaps/x/b.map", 1), "sourcemaps_minified_path_check")
	assertRejectedBy(t, db, row("v1.0.0", "/assets/c.js", "sourcemaps/x/c.map", 0), "sourcemaps_size_bytes_check")
	// The composite UNIQUE: the (P, v1.0.0, index) row already exists.
	assertRejectedBy(t, db, row("v1.0.0", "/assets/index-3f2a1c.js", "sourcemaps/x/dup.map", 1),
		"sourcemaps_project_id_release_version_minified_path_key")
	// storage_key UNIQUE: a fresh path pointing at an object another row already owns.
	assertRejectedBy(t, db, row("v1.0.0", "/assets/other.js", "sourcemaps/p/v1/index.map", 1), "idx_sourcemaps_storage_key")
	// The FK: a project that does not exist.
	assertRejectedBy(t, db,
		`INSERT INTO sourcemaps (project_id, release_version, minified_path, storage_key, size_bytes, sha256)
		 VALUES ('00000000-0000-0000-0000-000000000000', 'v1.0.0', '/assets/z.js', 'sourcemaps/x/z.map', 1, '`+sha+`')`,
		"sourcemaps_project_id_fkey")
	// Nothing above was written.
	assertRowCount(t, db, "sourcemaps", 3)
}

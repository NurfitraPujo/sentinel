package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sentinel-issue-tag-facets spec §3.3-§3.4 and §10.2 (TASK-01): the rollup the triggers keep is exact
// under StoreEvent, duplicates, rollbacks, old pods, the cap, updates, truncation and issue deletes.

// tfEvent is one StoreEvent delivery.
type tfEvent struct {
	Environment, Fingerprint, Release, EventID string
	Tags, Contexts, User                       string
}

// tfStore delivers ev through the real store.StoreEvent and returns the issue id and `stored`.
func tfStore(t *testing.T, env *tagFacetsEnv, projectID string, ev tfEvent) (string, bool) {
	t.Helper()
	id, stored, err := tfStoreErr(env, projectID, ev)
	if err != nil {
		t.Fatalf("StoreEvent: %v", err)
	}
	return id, stored
}

// tfStoreErr is tfStore for goroutines, which must not call t.Fatalf.
func tfStoreErr(env *tagFacetsEnv, projectID string, ev tfEvent) (string, bool, error) {
	now := time.Now().UTC()
	issue := &store.Issue{
		ID: uuid.NewString(), ProjectID: projectID, Environment: ev.Environment, Fingerprint: ev.Fingerprint,
		Message: "tag facets test", ErrorClass: "TagFacetsError", Status: "unresolved", FirstSeen: now, LastSeen: now,
	}
	occ := &store.ErrorOccurrence{
		ID: uuid.NewString(), Environment: ev.Environment, Platform: "go", ReleaseVersion: ev.Release,
		Stacktrace: json.RawMessage(`[]`), Metadata: json.RawMessage(`{}`), CreatedAt: now, EventID: ev.EventID,
		Level: "error", ErrorClass: "TagFacetsError",
	}
	if ev.Tags != "" {
		occ.Tags = json.RawMessage(ev.Tags)
	}
	if ev.Contexts != "" {
		occ.Contexts = json.RawMessage(ev.Contexts)
	}
	if ev.User != "" {
		occ.User = json.RawMessage(ev.User)
	}
	_, stored, err := env.Store.StoreEvent(context.Background(), issue, occ, ev.Release)
	return issue.ID, stored, err
}

func TestTagFacets_StoreEventCountsExactly(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)

	// 60 events over 3 issues (A production, B staging, C production with another fingerprint); releases
	// vary (incl. "" = no release), contexts carry os / client_os / device / browser, some carry a user,
	// and each carries 0-3 tags. The hand counts below are computed from the same list, in Go.
	type delivered struct {
		issue string
		ev    tfEvent
	}
	var evs []delivered
	for i := 0; i < 60; i++ {
		var ev tfEvent
		var which string
		switch i % 3 {
		case 0:
			which, ev = "A", tfEvent{Environment: "production", Fingerprint: "fp-A", Release: "1.0"}
		case 1:
			which, ev = "B", tfEvent{Environment: "staging", Fingerprint: "fp-B", Release: []string{"1.0", "2.0", ""}[i%3+i%2]}
		default:
			which, ev = "C", tfEvent{Environment: "production", Fingerprint: "fp-C", Release: ""}
		}
		j := i / 3 // the event's index within its issue
		ctxs := map[string]any{}
		model := "Pixel 7"
		if (which == "A" && j < 12) || (which != "A" && i%5 == 0) {
			model = "SM-A115F"
		}
		if which == "A" || i%7 != 0 {
			ctxs["device"] = map[string]any{"model": model}
		}
		if i%2 == 0 {
			ctxs["os"] = map[string]any{"name": "Android"}
		} else {
			ctxs["client_os"] = map[string]any{"name": "Linux"}
		}
		if i%4 == 0 {
			ctxs["browser"] = map[string]any{"name": "Chrome"}
		}
		b, _ := json.Marshal(ctxs)
		ev.Contexts = string(b)
		if i%3 == 0 && i%9 != 0 {
			ev.User = tfUser(fmt.Sprintf("user-%d", i%4))
		}
		nTags := i % 4 // 0-3 tags; A always carries at least its tenant
		if which == "A" {
			nTags = 1 + i%3
		}
		tenant := "acme"
		if which == "A" && j >= 7 {
			tenant = "globex"
		}
		tags := map[string]string{}
		for k := 0; k < nTags; k++ {
			tags[[]string{"tenant", "region", "flag"}[k]] = []string{tenant, "eu", "on"}[k]
		}
		if len(tags) > 0 {
			tb, _ := json.Marshal(tags)
			ev.Tags = string(tb)
		}
		ev.EventID = fmt.Sprintf("evt-%d", i)
		evs = append(evs, delivered{which, ev})
	}

	ids := map[string]string{}
	for _, d := range evs {
		id, stored := tfStore(t, env, projectID, d.ev)
		if !stored {
			t.Fatalf("event %s not stored", d.ev.EventID)
		}
		ids[d.issue] = id
	}
	tfCheck(t, env.Pool, ids["A"], ids["B"], ids["C"])

	// Hand counts on issue A, from the list.
	var rel10, sm, acme int64
	for _, d := range evs {
		if d.issue != "A" {
			continue
		}
		if d.ev.Release == "1.0" {
			rel10++
		}
		if strings.Contains(d.ev.Contexts, `"SM-A115F"`) {
			sm++
		}
		if strings.Contains(d.ev.Tags, `"tenant":"acme"`) {
			acme++
		}
	}
	if rel10 != 20 || sm != 12 || acme != 7 {
		t.Fatalf("fixture drifted: A has release=1.0 %d (want 20), SM-A115F %d (want 12), tenant=acme %d (want 7)", rel10, sm, acme)
	}
	if got := tfCount(t, env.Pool, ids["A"], "release", "1.0"); got != rel10 {
		t.Fatalf("A release=1.0: %d, want %d", got, rel10)
	}
	if got := tfCount(t, env.Pool, ids["A"], "device.model", "SM-A115F"); got != sm {
		t.Fatalf("A device.model=SM-A115F: %d, want %d", got, sm)
	}
	if got := tfCount(t, env.Pool, ids["A"], "tenant", "acme"); got != acme {
		t.Fatalf("A tenant=acme: %d, want %d", got, acme)
	}
	// Issues are separate: C never saw a release, so it has no release row at all.
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'release'`, ids["C"]); n != 0 {
		t.Fatalf("C has %d release row(s); an empty release is never counted", n)
	}
	// One multi-row statement spanning all three issues (a seed script's or the backfill's shape): the
	// statement trigger groups its transition table per issue, so nothing leaks across issues.
	tfInsertOccs(t, env.Pool, []tfOcc{
		{IssueID: ids["A"], ProjectID: projectID, Release: "1.0", Environment: "production"},
		{IssueID: ids["B"], ProjectID: projectID, Release: "1.0", Environment: "staging"},
		{IssueID: ids["C"], ProjectID: projectID, Release: "1.0", Environment: "production"},
	})
	if got := tfCount(t, env.Pool, ids["A"], "release", "1.0"); got != rel10+1 {
		t.Fatalf("A release=1.0 after the multi-issue statement: %d, want %d", got, rel10+1)
	}
	if got := tfCount(t, env.Pool, ids["C"], "release", "1.0"); got != 1 {
		t.Fatalf("C release=1.0 after the multi-issue statement: %d, want 1", got)
	}
	tfCheck(t, env.Pool, ids["A"], ids["B"], ids["C"])
}

func TestTagFacets_DuplicateCountsOnce(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	ev := tfEvent{Environment: "production", Fingerprint: "fp-dup", Release: "3.1", EventID: "evt-dup",
		Tags: `{"tenant":"acme"}`, Contexts: `{"os":{"name":"Android"}}`, User: tfUser("u-dup")}
	issueID, stored := tfStore(t, env, projectID, ev)
	if !stored {
		t.Fatalf("first delivery not stored")
	}
	first := tfSnapshot(t, env.Pool)

	if _, stored := tfStore(t, env, projectID, ev); stored {
		t.Fatalf("second delivery stored; it is a duplicate")
	}
	if got := tfSnapshot(t, env.Pool); got != first {
		t.Fatalf("rollup changed after a duplicate:\n%s\nvs\n%s", got, first)
	}
	// The same (project_id, event_id) resolved to a DIFFERENT issue of the project (R1).
	other := ev
	other.Fingerprint = "fp-dup-other"
	if _, stored := tfStore(t, env, projectID, other); stored {
		t.Fatalf("third delivery stored; its (project_id, event_id) already exists")
	}
	if got := tfSnapshot(t, env.Pool); got != first {
		t.Fatalf("rollup changed after a cross-issue duplicate:\n%s\nvs\n%s", got, first)
	}
	if got := tfCount(t, env.Pool, issueID, "release", "3.1"); got != 1 {
		t.Fatalf("release=3.1: %d, want 1", got)
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacets_RolledBackInsertCountsNothing(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, Release: "1.0"}})
	before := tfSnapshot(t, env.Pool)

	tx, err := env.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	tfInsertOccs(t, tx, []tfOcc{{IssueID: issueID, ProjectID: projectID, Release: "9.9", Tags: `{"k":"v"}`}})
	if got := tfCount(t, tx, issueID, "release", "9.9"); got != 1 {
		t.Fatalf("inside the transaction release=9.9 is %d, want 1 (the trigger runs in the insert's transaction)", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := tfSnapshot(t, env.Pool); got != before {
		t.Fatalf("a rolled-back insert changed the rollup:\n%s\nvs\n%s", got, before)
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacets_CapAndOverflow(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	named := func() int64 {
		return tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'user.id' AND value <> ''`, issueID)
	}
	overflow := func() int64 { return tfCount(t, env.Pool, issueID, "user.id", "") }
	hasOverflow := func() bool {
		return tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'user.id' AND value = ''`, issueID) == 1
	}

	// 1001 distinct user.id in ONE statement: the 1000 byte-smallest get named rows, the last overflows.
	// u0000..u0599 are aged (the retention twin deletes them below).
	var occs []tfOcc
	for i := 0; i <= 1000; i++ {
		o := tfOcc{IssueID: issueID, ProjectID: projectID, User: tfUser(fmt.Sprintf("u%04d", i))}
		if i < 600 {
			o.CreatedAt = aged
		}
		occs = append(occs, o)
	}
	tfInsertOccs(t, env.Pool, occs)
	if named() != 1000 || overflow() != 1 || tfCount(t, env.Pool, issueID, "user.id", "u1000") != 0 ||
		tfCount(t, env.Pool, issueID, "user.id", "u0999") != 1 {
		t.Fatalf("after 1001 values: named %d (want 1000), overflow %d (want 1), u1000 must be in the overflow", named(), overflow())
	}
	tfCheck(t, env.Pool, issueID)

	// An existing named value goes to its row; a new value goes to the overflow row.
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, User: tfUser("u0700")}})
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, User: tfUser("u2000")}})
	if tfCount(t, env.Pool, issueID, "user.id", "u0700") != 2 || overflow() != 2 || named() != 1000 {
		t.Fatalf("u0700 %d (want 2), overflow %d (want 2), named %d", tfCount(t, env.Pool, issueID, "user.id", "u0700"), overflow(), named())
	}
	tfCheck(t, env.Pool, issueID)

	// The retention twin deletes the 600 aged named values: named 400, the overflow row stays.
	c := tfAcquire(t, env.Pool)
	if deleted, _ := tfSweepTwin(t, c, cutoff, 200); deleted != 600 {
		t.Fatalf("sweep twin deleted %d, want 600", deleted)
	}
	if named() != 400 || overflow() != 2 {
		t.Fatalf("after the sweep: named %d (want 400), overflow %d (want 2)", named(), overflow())
	}
	tfCheck(t, env.Pool, issueID)

	// I-cap: while the overflow row exists, a brand-new value still goes to it.
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, User: tfUser("u3000")}})
	if named() != 400 || overflow() != 3 {
		t.Fatalf("a new value while overflowed: named %d (want 400), overflow %d (want 3)", named(), overflow())
	}
	tfCheck(t, env.Pool, issueID)

	// Delete every occurrence counted in the overflow row: it goes, and the next new value is named.
	if _, err := env.Pool.Exec(ctx, `DELETE FROM error_occurrences WHERE issue_id = $1 AND event_user->>'id' = ANY($2)`,
		issueID, []string{"u1000", "u2000", "u3000"}); err != nil {
		t.Fatalf("delete overflow occurrences: %v", err)
	}
	if hasOverflow() {
		t.Fatalf("the overflow row survived its last occurrence")
	}
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, User: tfUser("u4000")}})
	if named() != 401 || tfCount(t, env.Pool, issueID, "user.id", "u4000") != 1 || hasOverflow() {
		t.Fatalf("after the overflow went: named %d (want 401), u4000 %d (want 1)", named(), tfCount(t, env.Pool, issueID, "user.id", "u4000"))
	}
	tfCheck(t, env.Pool, issueID)

	// The overflow value '' cannot collide with a caller's value: a real tag value "(other)" is named.
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: issueID, ProjectID: projectID, Tags: `{"k":"(other)"}`}})
	if tfCount(t, env.Pool, issueID, "k", "(other)") != 1 || tfCount(t, env.Pool, issueID, "k", "") != 0 {
		t.Fatalf("a real value \"(other)\" must be an ordinary named value")
	}
	tfCheck(t, env.Pool, issueID)
}

// f49721f's occurrence INSERT, verbatim (store.go:1064-1073 at that revision): it names no
// tag_rollup_applied, as an old processor pod's does during the roll (W6).
const tfOldProcessorInsert = `
		INSERT INTO error_occurrences (id, issue_id, environment, platform, release_version,
			stacktrace, metadata, trace_id, span_id, created_at, event_id, project_id,
			level, message, error_class, logger, server_name, transaction, dist, source,
			trace_flags, received_at, sdk, event_user, tags, contexts, request, breadcrumbs, exception)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, NULLIF($11,''), $12,
			$13, NULLIF($14,''), $15, NULLIF($16,''), NULLIF($17,''), NULLIF($18,''), NULLIF($19,''), NULLIF($20,''),
			$21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (project_id, event_id) WHERE project_id IS NOT NULL AND event_id IS NOT NULL DO NOTHING
	`

func tfOldInsertArgs(occID, issueID, projectID, release, eventID string) []any {
	return []any{occID, issueID, "production", "go", release, `[]`, `{}`, "", "", time.Now().UTC(), eventID, projectID,
		"error", "", "OldPodError", "", "", "", "", "",
		nil, nil, nil, `{"id":"old-user"}`, `{"tenant":"old"}`, `{"os":{"name":"Android"}}`, nil, nil, nil}
}

func TestTagFacets_OldProcessorInsertIsCounted(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")
	occID := uuid.NewString()
	tag, err := env.Pool.Exec(ctx, tfOldProcessorInsert, tfOldInsertArgs(occID, issueID, projectID, "0.9", "evt-old")...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("old processor insert: %v (%d rows)", err, tag.RowsAffected())
	}
	var applied *bool
	if err := env.Pool.QueryRow(ctx, `SELECT tag_rollup_applied FROM error_occurrences WHERE id = $1`, occID).Scan(&applied); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if applied == nil || !*applied {
		t.Fatalf("an old pod's row must be marked TRUE by the BEFORE trigger, got %v", applied)
	}
	for _, kv := range [][2]string{{"release", "0.9"}, {"user.id", "old-user"}, {"tenant", "old"}, {"os.name", "Android"}} {
		if got := tfCount(t, env.Pool, issueID, kv[0], kv[1]); got != 1 {
			t.Fatalf("%s=%s: %d, want 1", kv[0], kv[1], got)
		}
	}
	tfCheck(t, env.Pool, issueID)
	tfAfterBackfillStillOnce(t, env, issueID)
}

func TestTagFacets_RedeliveryAfterCommitCountsOnce(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	ev := tfEvent{Environment: "production", Fingerprint: "fp-redeliver", Release: "4.0", EventID: "evt-redeliver",
		Contexts: `{"browser":{"name":"Firefox"}}`}
	issueID, stored := tfStore(t, env, projectID, ev)
	if !stored {
		t.Fatalf("first delivery not stored")
	}
	for i := 0; i < 3; i++ { // the NATS redeliveries of a message whose first delivery committed (W4)
		if _, stored := tfStore(t, env, projectID, ev); stored {
			t.Fatalf("redelivery %d stored", i+1)
		}
	}
	if got := tfCount(t, env.Pool, issueID, "browser.name", "Firefox"); got != 1 {
		t.Fatalf("browser.name=Firefox: %d, want 1", got)
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacets_UpdateMovesContributions(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	a := tfSeedIssue(t, env.Pool, projectID, "production")
	b := tfSeedIssue(t, env.Pool, projectID, "production")
	var occs []tfOcc
	for i := 0; i < 60; i++ {
		occs = append(occs, tfOcc{IssueID: a, ProjectID: projectID, Release: "1.0", Tags: fmt.Sprintf(`{"t":"v%d"}`, i%3)})
	}
	ids := tfInsertOccs(t, env.Pool, occs)
	tfCheck(t, env.Pool, a, b)

	// Move 30 rows to issue B and change their tags in one statement (W9).
	if _, err := env.Pool.Exec(ctx,
		`UPDATE error_occurrences SET issue_id = $1, tags = '{"t":"moved"}' WHERE id = ANY($2::uuid[])`, b, ids[:30]); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := tfCount(t, env.Pool, a, "release", "1.0"); got != 30 {
		t.Fatalf("A release=1.0 after the move: %d, want 30", got)
	}
	if got := tfCount(t, env.Pool, b, "t", "moved"); got != 30 {
		t.Fatalf("B t=moved: %d, want 30", got)
	}
	tfCheck(t, env.Pool, a, b)

	// An update touching no rollup column changes nothing.
	before := tfSnapshot(t, env.Pool)
	if _, err := env.Pool.Exec(ctx, `UPDATE error_occurrences SET project_id = project_id`); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if got := tfSnapshot(t, env.Pool); got != before {
		t.Fatalf("a no-op update changed the rollup")
	}
	tfCheck(t, env.Pool, a, b)
}

func TestTagFacets_TruncateClears(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	a := tfSeedIssue(t, env.Pool, projectID, "production")
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: a, ProjectID: projectID, Release: "1.0"}, {IssueID: a, ProjectID: projectID, Release: "2.0"}})
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values`); n == 0 {
		t.Fatalf("precondition: the rollup is empty before the truncate")
	}
	if _, err := env.Pool.Exec(context.Background(), `TRUNCATE error_occurrences CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values`); n != 0 {
		t.Fatalf("%d rollup row(s) survived TRUNCATE (W10)", n)
	}
	tfCheck(t, env.Pool, a)
}

func TestTagFacets_IssueDeleteCascades(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	a := tfSeedIssue(t, env.Pool, projectID, "production")
	b := tfSeedIssue(t, env.Pool, projectID, "production")
	tfInsertOccs(t, env.Pool, []tfOcc{{IssueID: a, ProjectID: projectID, Release: "1.0"}, {IssueID: b, ProjectID: projectID, Release: "1.0"}})
	if _, err := env.Pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, a); err != nil {
		t.Fatalf("delete issue: %v", err)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1`, a); n != 0 {
		t.Fatalf("%d rollup row(s) of a deleted issue survived (W8)", n)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values_drift`); n != 0 {
		t.Fatalf("an issue delete recorded %d drift row(s)", n)
	}
	if got := tfCount(t, env.Pool, b, "release", "1.0"); got != 1 {
		t.Fatalf("B release=1.0: %d, want 1", got)
	}
	tfCheck(t, env.Pool, a, b)
}

// tfPlanNotices opens a dedicated connection whose NOTICEs are captured, for the auto_explain proofs.
func tfPlanNotices(t *testing.T, env *tagFacetsEnv) (*pgx.Conn, *[]string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, s := range []string{`LOAD 'auto_explain'`, `SET auto_explain.log_min_duration = 0`, `SET auto_explain.log_nested_statements = on`,
		`SET auto_explain.log_analyze = on`, `SET auto_explain.log_level = notice`, `SET auto_explain.log_format = json`} {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return conn, &notices
}

// tfPlanFor returns the parsed JSON plan of the first captured notice whose query text starts with prefix.
func tfPlanFor(t *testing.T, notices []string, prefix string) map[string]any {
	t.Helper()
	for _, n := range notices {
		i := strings.Index(n, "{")
		if i < 0 {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(n[i:]), &doc) != nil {
			continue
		}
		if q, _ := doc["Query Text"].(string); strings.HasPrefix(strings.TrimSpace(q), prefix) {
			return doc
		}
	}
	t.Fatalf("no auto_explain plan for a statement starting %q among %d notice(s)", prefix, len(notices))
	return nil
}

// tfWalkPlan calls fn on every plan node.
func tfWalkPlan(node map[string]any, fn func(map[string]any)) {
	fn(node)
	if kids, ok := node["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				tfWalkPlan(m, fn)
			}
		}
	}
}

func TestTagFacets_AdmissionStateOncePerKey(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	notOver := tfSeedIssue(t, env.Pool, projectID, "production")
	over := tfSeedIssue(t, env.Pool, projectID, "production")
	// notOver: 900 named user.id rows. over: 1000 named + the overflow row.
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO issue_tag_values (issue_id, key, value, count, first_seen, last_seen)
		SELECT $1::uuid, 'user.id', 'n' || lpad(g::text, 4, '0'), 1, now(), now() FROM generate_series(1, 900) g
		UNION ALL
		SELECT $2::uuid, 'user.id', 'o' || lpad(g::text, 4, '0'), 1, now(), now() FROM generate_series(1, 1000) g
		UNION ALL
		SELECT $2::uuid, 'user.id', '', 5, now(), now()`, notOver, over); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}
	var issues, keys, values []string
	var ns []int64
	var ts []time.Time
	for i := 0; i < 50; i++ {
		for _, is := range []string{notOver, over} {
			issues, keys, values = append(issues, is), append(keys, "user.id"), append(values, fmt.Sprintf("new-%02d", i))
			ns, ts = append(ns, 1), append(ts, time.Now().UTC())
		}
	}
	conn, notices := tfPlanNotices(t, env)
	if _, err := conn.Exec(ctx, `SELECT issue_tag_values_add($1::uuid[], $2::text[], $3::text[], $4::bigint[], $5::timestamptz[], $6::timestamptz[])`,
		issues, keys, values, ns, ts, ts); err != nil {
		t.Fatalf("issue_tag_values_add: %v", err)
	}
	plan := tfPlanFor(t, *notices, "WITH d AS (SELECT * FROM unnest(p_issue, p_key, p_value, p_n, p_first, p_last)")
	root, _ := plan["Plan"].(map[string]any)
	var stateLoops, countLoops float64 = -1, -1
	var countNodes int
	tfWalkPlan(root, func(n map[string]any) {
		if n["Subplan Name"] == "CTE state" {
			stateLoops, _ = n["Actual Loops"].(float64)
		}
		// The named-row count: an Aggregate subplan over issue_tag_values inside the state CTE.
		if n["Node Type"] == "Aggregate" && strings.HasPrefix(fmt.Sprint(n["Subplan Name"]), "SubPlan") {
			countNodes++
			countLoops, _ = n["Actual Loops"].(float64)
		}
	})
	if stateLoops != 1 {
		t.Fatalf("the state CTE must be materialised once (CTE state loops %v)", stateLoops)
	}
	if countNodes != 1 {
		t.Fatalf("expected exactly one named-row count SubPlan, found %d", countNodes)
	}
	// Two distinct (issue, key) miss groups; the overflowed one must never run the count.
	if countLoops != 1 {
		t.Fatalf("the named-row count ran %v time(s); want exactly 1 (once for the non-overflowed key, never for the overflowed one)", countLoops)
	}
	if got := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'user.id' AND value <> ''`, notOver); got != 950 {
		t.Fatalf("not-overflowed key: %d named, want 950", got)
	}
	if got := tfCount(t, env.Pool, over, "user.id", ""); got != 55 {
		t.Fatalf("overflowed key: overflow count %d, want 5 + 50", got)
	}
	if got := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'user.id' AND value <> ''`, over); got != 1000 {
		t.Fatalf("overflowed key: %d named, want 1000 (no admission while overflowed)", got)
	}
}

func TestTagFacets_SubtractDeletesOnlyZeroedRows(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	issueID := tfSeedIssue(t, env.Pool, projectID, "production")

	// First: 3 occurrences of release r1 / user u1, so both are named rows that reach 0 when they go.
	var first []tfOcc
	for i := 0; i < 3; i++ {
		first = append(first, tfOcc{IssueID: issueID, ProjectID: projectID, Release: "r1", User: tfUser("u1")})
	}
	threeIDs := tfInsertOccs(t, env.Pool, first)
	// Then 2,000 others over releases r2..r5 and 1,199 other user ids: 999 more named + the overflow row.
	var others []tfOcc
	for i := 0; i < 2000; i++ {
		others = append(others, tfOcc{IssueID: issueID, ProjectID: projectID, Release: fmt.Sprintf("r%d", 2+i%4),
			User: tfUser(fmt.Sprintf("x%04d", i%1199))})
	}
	tfInsertOccs(t, env.Pool, others)
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1`, issueID); n != 1007 {
		t.Fatalf("fixture: the issue's rollup has %d rows, want 1,007", n)
	}
	tfCheck(t, env.Pool, issueID)

	// 100 unrelated issues with 1,000 user.id rollup rows each, so the table is production-sized and the
	// generic plans take the primary key (review finding tfs-rev-subtract-test-red-on-correct-code).
	var unrelated []string
	for i := 0; i < 100; i++ {
		unrelated = append(unrelated, tfSeedIssue(t, env.Pool, projectID, "production"))
	}
	if _, err := env.Pool.Exec(ctx, `
		INSERT INTO issue_tag_values (issue_id, key, value, count, first_seen, last_seen)
		SELECT i, 'user.id', 'v' || g, 1, now(), now() FROM unnest($1::uuid[]) i, generate_series(1, 1000) g`, unrelated); err != nil {
		t.Fatalf("seed unrelated rollup: %v", err)
	}
	if _, err := env.Pool.Exec(ctx, `ANALYZE issue_tag_values`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rt := tfScalar(t, env.Pool, `SELECT reltuples::bigint FROM pg_class WHERE oid = 'issue_tag_values'::regclass`); rt < 100000 {
		t.Fatalf("precondition: reltuples %d < 100,000", rt)
	}

	tx, err := env.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	counters := func() (read, deleted int64) {
		if err := tx.QueryRow(ctx, `
			SELECT pg_stat_get_xact_tuples_fetched('issue_tag_values'::regclass) + pg_stat_get_xact_tuples_returned('issue_tag_values'::regclass),
			       pg_stat_get_xact_tuples_deleted('issue_tag_values'::regclass)`).Scan(&read, &deleted); err != nil {
			t.Fatalf("counters: %v", err)
		}
		return
	}
	read0, del0 := counters()
	if _, err := tx.Exec(ctx, `DELETE FROM error_occurrences WHERE id = ANY($1::uuid[])`, threeIDs); err != nil {
		t.Fatalf("delete the 3: %v", err)
	}
	read1, del1 := counters()
	if read1-read0 >= 100 {
		t.Fatalf("deleting 3 occurrences read %d rollup tuples; the zero-row delete must not scan the issue's rollup", read1-read0)
	}
	if del1-del0 != 2 {
		t.Fatalf("deleted %d rollup rows, want exactly 2 (r1 and u1)", del1-del0)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if tfCount(t, env.Pool, issueID, "release", "r1") != 0 || tfCount(t, env.Pool, issueID, "user.id", "u1") != 0 {
		t.Fatalf("the r1 / u1 rows survived at 0")
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key IN ('release', 'user.id') AND value IN ('r1', 'u1')`, issueID); n != 0 {
		t.Fatalf("%d r1/u1 row(s) left", n)
	}
	tfCheck(t, env.Pool, issueID)
}

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------------------------------

// fixture is one test's private tenant: its own organization, project, and ingest API key. Every
// assertion in this package must be scoped to these IDs, because the stack under test writes into a
// database shared with every other test in the run (see the package comment in main_test.go).
type fixture struct {
	t *testing.T

	OrgID   string
	OrgName string
	OrgSlug string

	ProjectID   string
	ProjectName string

	// APIKey is the plaintext secret to present in X-API-Key. Only its SHA-256 is stored.
	APIKey   string
	APIKeyID string
}

// fixtureSeq keeps names unique within a process without a random source, so a failing test's
// artifacts are named predictably enough to grep for in the container logs.
var fixtureSeq atomic.Int64

// uniqueSuffix builds a short, DNS-and-identifier-safe suffix that is unique per fixture per process.
// The PID is included because two `go test` invocations can share one database.
func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), fixtureSeq.Add(1))
}

// sanitizeName reduces a Go test name to something usable as a project name. Subtests arrive as
// "TestFoo/case_name"; the slash and any punctuation would otherwise leak into an identifier.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// newSecret returns a plausible API-key secret. It is a real random value, not a fixed string, so a
// test can never accidentally authenticate against another test's key.
func newSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating an API key secret: %v", err)
	}
	return "sk_e2e_" + hex.EncodeToString(buf)
}

// newFixture seeds an organization, a project, and an active project-scoped ingest key, and registers
// cleanup. The project name doubles as the `project_key` on the wire — it is a NAME, never a
// credential (DECISIONS.md D11); the secret only ever travels in the X-API-Key header.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireStack(t)

	suffix := uniqueSuffix()
	base := sanitizeName(t.Name())
	// projects.name is bounded, and validation rejects a project_key over 64 chars, so budget for the
	// suffix rather than letting a long subtest name silently truncate into a collision.
	if max := 40 - len(suffix); len(base) > max && max > 0 {
		base = base[:max]
	}

	f := &fixture{
		t:           t,
		OrgName:     "e2e-org-" + base + "-" + suffix,
		OrgSlug:     strings.ToLower("e2e-" + base + "-" + suffix),
		ProjectName: "e2e-" + base + "-" + suffix,
		APIKey:      newSecret(t),
	}

	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id::text`,
		f.OrgName, f.OrgSlug,
	).Scan(&f.OrgID); err != nil {
		t.Fatalf("seeding organization %q: %v", f.OrgName, err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4)
		 RETURNING id::text`,
		f.ProjectName, f.APIKey, f.APIKey, f.OrgID,
	).Scan(&f.ProjectID); err != nil {
		t.Fatalf("seeding project %q: %v", f.ProjectName, err)
	}

	f.APIKeyID = f.addKey(keySpec{Name: "primary", Secret: f.APIKey, ProjectScoped: true})

	t.Cleanup(f.cleanup)
	return f
}

// keySpec describes an additional API key to seed. The zero value is an active, org-wide ingest key
// with a high rate limit.
type keySpec struct {
	Name string
	// Secret is the plaintext key. Empty means "generate one" — read it back from the returned id via
	// the caller's own variable.
	Secret string
	// ProjectScoped ties the key to this fixture's project. False leaves project_id NULL, which is
	// what makes a key organization-wide: the caller must then name the target project, by
	// X-Project-Key header or by body project_key.
	ProjectScoped bool
	Status        string
	Scope         string
	RateLimitRPM  int
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
}

// addKey seeds another API key against this fixture's organization and returns its id.
func (f *fixture) addKey(spec keySpec) string {
	f.t.Helper()

	if spec.Status == "" {
		spec.Status = "active"
	}
	if spec.Scope == "" {
		spec.Scope = "ingest"
	}
	if spec.RateLimitRPM == 0 {
		// High enough that a functional test never trips the limiter by accident. The rate-limit tests
		// set this deliberately low instead.
		spec.RateLimitRPM = 100000
	}
	if spec.Name == "" {
		spec.Name = "key-" + uniqueSuffix()
	}

	var projectID any
	if spec.ProjectScoped {
		projectID = f.ProjectID
	}

	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO project_api_keys
		   (project_id, organization_id, key_hash, key_prefix, name, scope, status,
		    rate_limit_rpm, expires_at, revoked_at, created_by)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4, $5, $6, $7, $8, $9, $10, 'e2e-harness')
		 RETURNING id::text`,
		projectID, f.OrgID, spec.Secret, keyPrefix(spec.Secret), spec.Name, spec.Scope, spec.Status,
		spec.RateLimitRPM, spec.ExpiresAt, spec.RevokedAt,
	).Scan(&id)
	if err != nil {
		f.t.Fatalf("seeding API key %q: %v", spec.Name, err)
	}
	return id
}

// keyPrefix mirrors what the dashboard stores for display: a short, non-secret leading fragment.
// project_api_keys.key_prefix is varchar(16), so this must not exceed that.
func keyPrefix(secret string) string {
	if len(secret) > 12 {
		return secret[:12]
	}
	return secret
}

// cleanup removes every row this fixture caused, in foreign-key order. It deletes by this fixture's
// own org/project ids only — never by name pattern, and never unscoped.
func (f *fixture) cleanup() {
	ctx := context.Background()

	// Occurrence-level rows hang off issues, which hang off the project.
	exec(ctx, `DELETE FROM error_search_index WHERE occurrence_id IN (
	             SELECT eo.id FROM error_occurrences eo
	             JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = $1)`, f.ProjectID)
	exec(ctx, `DELETE FROM error_occurrences WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, f.ProjectID)
	exec(ctx, `DELETE FROM issue_activity WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, f.ProjectID)
	exec(ctx, `DELETE FROM issue_relations WHERE source_issue_id IN (SELECT id FROM issues WHERE project_id = $1)
	             OR target_issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, f.ProjectID)
	exec(ctx, `DELETE FROM issues WHERE project_id = $1`, f.ProjectID)

	exec(ctx, `DELETE FROM alert_configs WHERE project_id = $1`, f.ProjectID)
	exec(ctx, `DELETE FROM project_members WHERE project_id = $1`, f.ProjectID)
	exec(ctx, `DELETE FROM project_api_keys WHERE organization_id = $1`, f.OrgID)
	exec(ctx, `DELETE FROM projects WHERE organization_id = $1`, f.OrgID)

	exec(ctx, `DELETE FROM organization_invitations WHERE organization_id = $1`, f.OrgID)
	exec(ctx, `DELETE FROM organization_members WHERE organization_id = $1`, f.OrgID)
	exec(ctx, `DELETE FROM organizations WHERE id = $1`, f.OrgID)
}

// exec runs a cleanup statement, tolerating failure. Cleanup runs after the assertions have already
// decided the verdict; a delete that fails must not turn a passing test red, but it must be visible.
func exec(ctx context.Context, sql string, args ...any) {
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		fmt.Fprintf(os.Stderr, "e2e cleanup: %v\n  sql: %s\n", err, strings.Join(strings.Fields(sql), " "))
	}
}

// ---------------------------------------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------------------------------------

// event is a canonical, valid ErrorEvent body. Tests start here and change the one field under test,
// so an unrelated schema change breaks one builder rather than thirty literals.
type event map[string]any

// newEvent returns a valid event for this fixture. It deliberately sets project_key to the project's
// name (the wire field is a name, not a secret) and includes one in_app frame so fingerprinting has
// something to work with.
func (f *fixture) newEvent() event {
	return event{
		"project_key": f.ProjectName,
		"platform":    "go",
		"environment": "e2e",
		"message":     "e2e canonical error",
		"error_class": "E2ECanonicalError",
		"trace_id":    "trace-" + uniqueSuffix(),
		"span_id":     "span-" + uniqueSuffix(),
		"stacktrace": []map[string]any{
			{"file": "main.go", "line": 42, "function": "main", "in_app": true},
		},
		"metadata":        map[string]any{},
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
		"trace_flags":     0,
		"release_version": "1.0.0",
	}
}

// with returns a copy of the event with the given fields replaced. Copying keeps a table-driven test
// from mutating the shared builder output between cases.
func (e event) with(overrides map[string]any) event {
	out := make(event, len(e)+len(overrides))
	for k, v := range e {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ingestResult is what the harness reports back from an /ingest call.
type ingestResult struct {
	Status int
	Body   string
	Header http.Header
}

// batchResponse mirrors the /ingest/batch response shape in apps/ingestor-go/main.go.
type batchResponse struct {
	Ingested int `json:"ingested"`
	Failed   int `json:"failed"`
	Errors   []struct {
		Index   int    `json:"index"`
		Message string `json:"message"`
	} `json:"errors"`
	// EventIDs is additive (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f): one entry per SUCCESSFULLY
	// ingested item, keyed by its index in the request, carrying the EFFECTIVE event_id (the client's
	// own value when usable, or a freshly minted UUIDv4 otherwise). Failed items never appear here —
	// see main.go's handleBatchIngest, which only appends after result.Ingested++.
	EventIDs []struct {
		Index   int    `json:"index"`
		EventID string `json:"event_id"`
	} `json:"event_ids"`
}

// ingestAcceptedBody is the documented shape of a single-event 202 response
// (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f): {"status":"accepted","event_id":"..."}. event_id is the
// EFFECTIVE id — the client's own value when usable, or a freshly minted UUIDv4 otherwise — never the
// raw client-supplied value verbatim when it was replaced.
type ingestAcceptedBody struct {
	Status  string `json:"status"`
	EventID string `json:"event_id"`
}

// decodeAccepted parses a single-ingest 202 body, failing the test if it is not the documented
// {status,event_id} shape.
func (r ingestResult) decodeAccepted(t *testing.T) ingestAcceptedBody {
	t.Helper()
	var out ingestAcceptedBody
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		t.Fatalf("202 body was not the documented {status,event_id} shape: %v\n  body: %s", err, r.Body)
	}
	return out
}

// ingestOpts customizes a single request. The zero value presents the fixture's own key and no
// X-Project-Key header.
type ingestOpts struct {
	// APIKey overrides the fixture's key. Empty means the fixture's key; use NoAPIKey for "send none".
	APIKey string
	// NoAPIKey omits the X-API-Key header entirely, to exercise the unauthenticated path.
	NoAPIKey bool
	// ProjectKeyHeader sets X-Project-Key, which is how an organization-wide key names its target.
	ProjectKeyHeader string
	// Headers sets arbitrary additional request headers, applied last so a test can override any of
	// the above. U35 uses it to present an inbound W3C `traceparent`.
	Headers map[string]string
}

// ingest posts one event to the real /ingest endpoint over HTTP.
func (f *fixture) ingest(body any, opts ...ingestOpts) ingestResult {
	f.t.Helper()
	return f.post("/ingest", body, opts...)
}

// ingestBatch posts a slice of events to /ingest/batch and returns the raw result. Use decodeBatch to
// read the {ingested, failed, errors[]} body.
func (f *fixture) ingestBatch(events []event, opts ...ingestOpts) ingestResult {
	f.t.Helper()
	return f.post("/ingest/batch", events, opts...)
}

func (f *fixture) post(path string, body any, opts ...ingestOpts) ingestResult {
	f.t.Helper()

	var o ingestOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	payload, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshalling %s body: %v", path, err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.IngestorURL+path, bytes.NewReader(payload))
	if err != nil {
		f.t.Fatalf("building %s request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if !o.NoAPIKey {
		key := o.APIKey
		if key == "" {
			key = f.APIKey
		}
		req.Header.Set("X-API-Key", key)
	}
	if o.ProjectKeyHeader != "" {
		req.Header.Set("X-Project-Key", o.ProjectKeyHeader)
	}
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		f.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	return ingestResult{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
}

// decodeBatch parses a /ingest/batch response, failing the test if the body is not the documented
// shape. A 2xx carrying an unparseable body is itself the bug this checks for (S15).
func (f *fixture) decodeBatch(res ingestResult) batchResponse {
	f.t.Helper()
	var out batchResponse
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		f.t.Fatalf("batch response was not the documented {ingested,failed,errors[]} shape: %v\n  body: %s", err, res.Body)
	}
	return out
}

// ---------------------------------------------------------------------------------------------------
// Waiting for the asynchronous hop
// ---------------------------------------------------------------------------------------------------

// asyncTimeout bounds how long the ingestor → NATS → processor → Postgres hop may take. Generous
// enough for a cold consumer on a loaded CI runner, and still far below the suite's own timeout.
const asyncTimeout = 45 * time.Second

// waitFor polls cond until it returns true or the timeout expires, then fails with what was actually
// observed. Never replace this with a sleep: a fixed sleep is either flaky or slow, and this suite is
// only useful if it is trusted enough to stay enabled.
//
// cond returns (satisfied, detail) — detail is reported on failure so the message says what the state
// was when time ran out rather than only what was wanted.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var detail string
	for {
		ok, d := cond()
		detail = d
		if ok {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s\n  last observed: %s", timeout, what, detail)
}

// waitForOccurrences waits until the project has exactly want occurrences, then holds briefly to
// catch an overshoot. Asserting "at least N" would pass while a redelivery bug silently doubled
// every event, which is precisely the failure U28 and S16 are about.
func (f *fixture) waitForOccurrences(want int) {
	f.t.Helper()

	waitFor(f.t, asyncTimeout, fmt.Sprintf("%d occurrences in project %s", want, f.ProjectName), func() (bool, string) {
		got := f.occurrenceCount()
		return got == want, fmt.Sprintf("%d occurrences", got)
	})

	// A duplicate arrives after the target count is reached, not before, so it is invisible to the
	// poll above. Give redelivery a moment to prove it is not happening.
	time.Sleep(1 * time.Second)
	if got := f.occurrenceCount(); got != want {
		f.t.Fatalf("occurrence count moved after reaching %d: now %d — duplicate delivery", want, got)
	}
}

// waitForIssues waits until the project has exactly want issue rows.
func (f *fixture) waitForIssues(want int) {
	f.t.Helper()
	waitFor(f.t, asyncTimeout, fmt.Sprintf("%d issues in project %s", want, f.ProjectName), func() (bool, string) {
		got := f.issueCount()
		return got == want, fmt.Sprintf("%d issues", got)
	})
}

// ---------------------------------------------------------------------------------------------------
// Database reads (always scoped to the fixture)
// ---------------------------------------------------------------------------------------------------

func (f *fixture) occurrenceCount() int {
	f.t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM error_occurrences eo
		 JOIN issues i ON i.id = eo.issue_id
		 WHERE i.project_id = $1`, f.ProjectID).Scan(&n)
	if err != nil {
		f.t.Fatalf("counting occurrences: %v", err)
	}
	return n
}

func (f *fixture) issueCount() int {
	f.t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE project_id = $1`, f.ProjectID).Scan(&n); err != nil {
		f.t.Fatalf("counting issues: %v", err)
	}
	return n
}

// issueRow is the subset of `issues` the matrix asserts on.
//
// These fields are the REAL columns of `public.issues`, taken from `\d issues` on a migrated database —
// not from apps/dashboard-web/src/lib/db/schema.ts. The two have drifted before, and drift in that
// direction is invisible until a query runs: the Drizzle definition is what the dashboard believes,
// the migrations are what exists. There is no `title`, no `first_release` and no `last_release` column;
// the human-readable text is `message`, and release tracking is `resolved_in_version` plus
// error_occurrences.release_version.
//
// RegressionStatus and RegressionCount are NOT NULL with defaults ('none', 0), so they are values
// rather than pointers — a nil check on them would be dead code.
type issueRow struct {
	ID                string
	Fingerprint       string
	Message           string
	Status            string
	Count             int64
	ErrorClass        string
	RegressionStatus  string
	RegressionCount   int
	LastRegressedAt   *time.Time
	ResolvedInVersion *string
	ResolvedAt        *time.Time
}

// issues returns every issue in this project, oldest first.
func (f *fixture) issues() []issueRow {
	f.t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, fingerprint, message, status, count, error_class,
		        regression_status, regression_count, last_regressed_at,
		        resolved_in_version, resolved_at
		   FROM issues WHERE project_id = $1 ORDER BY first_seen`, f.ProjectID)
	if err != nil {
		f.t.Fatalf("querying issues: %v", err)
	}
	defer rows.Close()

	var out []issueRow
	for rows.Next() {
		var r issueRow
		if err := rows.Scan(&r.ID, &r.Fingerprint, &r.Message, &r.Status, &r.Count, &r.ErrorClass,
			&r.RegressionStatus, &r.RegressionCount, &r.LastRegressedAt,
			&r.ResolvedInVersion, &r.ResolvedAt); err != nil {
			f.t.Fatalf("scanning issue: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterating issues: %v", err)
	}
	return out
}

// onlyIssue returns the project's single issue, failing if there is not exactly one. Most rows in the
// matrix are about one issue, and "exactly one" is usually half the assertion.
func (f *fixture) onlyIssue() issueRow {
	f.t.Helper()
	all := f.issues()
	if len(all) != 1 {
		f.t.Fatalf("expected exactly 1 issue in project %s, found %d", f.ProjectName, len(all))
	}
	return all[0]
}

// occurrenceRow is the subset of `error_occurrences` the matrix asserts on.
//
// Again these are the real columns: `error_occurrences` has NO `message` and NO `timestamp`. The
// message belongs to the issue (one message per fingerprint, not per occurrence) and the time column is
// `created_at`. IssueMessage is joined in because U8 asserts on it.
type occurrenceRow struct {
	ID             string
	IssueID        string
	IssueMessage   string
	Platform       string
	Environment    string
	ReleaseVersion *string
	TraceID        *string
	SpanID         *string
	Metadata       []byte
	Stacktrace     []byte
	CreatedAt      time.Time
	// EventID is the idempotency key (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-b). NULLABLE — most
	// pre-W0/legacy rows and any event that never carried a usable id have this as nil, never "": D-b's
	// NULLIF mapping and CHECK constraint both exist specifically to make "" unreachable in this column.
	EventID *string
}

func (f *fixture) occurrences() []occurrenceRow {
	f.t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT eo.id::text, eo.issue_id::text, i.message, eo.platform, eo.environment,
		        eo.release_version, eo.trace_id, eo.span_id, eo.metadata, eo.stacktrace, eo.created_at,
		        eo.event_id
		   FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1
		  ORDER BY eo.created_at`, f.ProjectID)
	if err != nil {
		f.t.Fatalf("querying occurrences: %v", err)
	}
	defer rows.Close()

	var out []occurrenceRow
	for rows.Next() {
		var r occurrenceRow
		if err := rows.Scan(&r.ID, &r.IssueID, &r.IssueMessage, &r.Platform, &r.Environment,
			&r.ReleaseVersion, &r.TraceID, &r.SpanID, &r.Metadata, &r.Stacktrace, &r.CreatedAt,
			&r.EventID); err != nil {
			f.t.Fatalf("scanning occurrence: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterating occurrences: %v", err)
	}
	return out
}

// activityRow is one `issue_activity` row. The discriminator column is `event_type`, not `action`, and
// its CHECK constraint permits exactly: status_changed, assigned, unassigned, regressed, ai_analysis,
// linked. Note `status_changed` — the dashboard once wrote `status_change` and every such insert was
// rejected by the constraint.
//
// OldValue and NewValue are jsonb and nullable. `old_value` is currently always NULL on rows the
// processor writes; that is a known, recorded gap (DECISIONS.md D7), not something to assert as correct.
type activityRow struct {
	EventType string
	ActorType string
	ActorID   string
	OldValue  *string
	NewValue  *string
}

// activity returns this project's issue_activity rows of the given event type, oldest first.
func (f *fixture) activity(eventType string) []activityRow {
	f.t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT ia.event_type, ia.actor_type, ia.actor_id, ia.old_value::text, ia.new_value::text
		   FROM issue_activity ia
		   JOIN issues i ON i.id = ia.issue_id
		  WHERE i.project_id = $1 AND ia.event_type = $2
		  ORDER BY ia.created_at`, f.ProjectID, eventType)
	if err != nil {
		f.t.Fatalf("querying issue_activity: %v", err)
	}
	defer rows.Close()

	var out []activityRow
	for rows.Next() {
		var r activityRow
		if err := rows.Scan(&r.EventType, &r.ActorType, &r.ActorID, &r.OldValue, &r.NewValue); err != nil {
			f.t.Fatalf("scanning issue_activity: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterating issue_activity: %v", err)
	}
	return out
}

// resolve marks an issue resolved AT a specific release, which is what the dashboard does and what
// regression detection actually compares against.
//
// Setting `status='resolved'` alone is not enough and is an easy way to write a test that proves
// nothing: the processor compares an incoming event's release against `resolved_in_version`
// (apps/processor-go/store/store.go), and a NULL there makes every later occurrence look like a
// regression — which would collapse U11 (newer release regresses) and U12 (older release does not)
// into the same passing case.
func (f *fixture) resolve(issueID, resolvedInVersion string) {
	f.t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE issues
		    SET status = 'resolved', resolved_at = now(), resolved_in_version = $1,
		        resolved_by_type = 'user', resolved_by = 'e2e-harness'
		  WHERE id = $2 AND project_id = $3`, resolvedInVersion, issueID, f.ProjectID)
	if err != nil {
		f.t.Fatalf("resolving issue %s at %q: %v", issueID, resolvedInVersion, err)
	}
	if tag.RowsAffected() != 1 {
		f.t.Fatalf("resolving issue %s affected %d rows, want 1", issueID, tag.RowsAffected())
	}
}

// setIssueStatus sets only the status. `issues.check_status` permits exactly 'unresolved', 'resolved'
// and 'ignored'. For the resolved case prefer resolve(), which also records the release.
func (f *fixture) setIssueStatus(issueID, status string) {
	f.t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE issues SET status = $1 WHERE id = $2 AND project_id = $3`, status, issueID, f.ProjectID)
	if err != nil {
		f.t.Fatalf("setting issue %s status to %q: %v", issueID, status, err)
	}
	if tag.RowsAffected() != 1 {
		f.t.Fatalf("setting issue %s status to %q affected %d rows, want 1", issueID, status, tag.RowsAffected())
	}
}

// ---------------------------------------------------------------------------------------------------
// Fingerprinting (mirrors the processor)
// ---------------------------------------------------------------------------------------------------

// fingerprintOf reproduces the processor's grouping key so a test can look an issue up by it. It
// mirrors apps/processor-go's fingerprinter: error class, plus up to the first three in_app frames.
//
// When no frame is in_app the class alone is NOT the whole key — S11 was exactly the bug where it
// was, collapsing every distinct crash in a dependency into one issue. Tests that care about the
// no-in_app case should assert on the issue COUNT rather than on a fingerprint computed here.
func fingerprintOf(errorClass string, frames []map[string]any) string {
	const maxAppFrames = 3
	var appFrames []string
	for _, frame := range frames {
		if inApp, ok := frame["in_app"].(bool); ok && inApp {
			appFrames = append(appFrames, fmt.Sprintf("%v:%v", frame["file"], frame["function"]))
			if len(appFrames) >= maxAppFrames {
				break
			}
		}
	}
	input := errorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}
	sum := sha256Hex(input)
	return sum[:16]
}

// ---------------------------------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------------------------------

// dashboardUser is a seeded Auth.js user with a live database session, so the harness can call
// authenticated dashboard routes over real HTTP.
type dashboardUser struct {
	ID           string
	Email        string
	SessionToken string
}

// newDashboardUser seeds a user, an organization membership at the given role, and an Auth.js session
// row. Session strategy is "database" because the Drizzle adapter is configured (see
// src/lib/server/auth-config.ts) — which is why inserting a `session` row and presenting its token as
// a cookie is a genuine sign-in and not a shortcut around the auth code. The routes still run their
// own `locals.auth()` and RBAC checks against it.
func (f *fixture) newDashboardUser(role string) *dashboardUser {
	f.t.Helper()

	suffix := uniqueSuffix()
	u := &dashboardUser{
		Email:        fmt.Sprintf("e2e-%s-%s@example.test", strings.ToLower(role), suffix),
		SessionToken: "e2e-session-" + suffix + "-" + sha256Hex(suffix)[:16],
	}

	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (id, name, email, email_verified)
		 VALUES (gen_random_uuid()::text, $1, $2, now()) RETURNING id`,
		"e2e "+role, u.Email,
	).Scan(&u.ID); err != nil {
		f.t.Fatalf("seeding dashboard user: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO session (session_token, user_id, expires) VALUES ($1, $2, now() + interval '1 hour')`,
		u.SessionToken, u.ID,
	); err != nil {
		f.t.Fatalf("seeding session: %v", err)
	}

	if role != "" {
		if _, err := pool.Exec(ctx,
			`INSERT INTO organization_members (organization_id, user_id, role) VALUES ($1, $2, $3)`,
			f.OrgID, u.ID, role,
		); err != nil {
			f.t.Fatalf("seeding organization_members role %q: %v", role, err)
		}
	}

	f.t.Cleanup(func() {
		cctx := context.Background()
		exec(cctx, `DELETE FROM session WHERE user_id = $1`, u.ID)
		exec(cctx, `DELETE FROM organization_members WHERE user_id = $1`, u.ID)
		exec(cctx, `DELETE FROM project_members WHERE user_id = $1`, u.ID)
		// Rows that reference this user must go before the "user" row itself.
		// manual_issue_reports.reporter_id is a RESTRICT FK to "user"(id): deleting the
		// user first raises a foreign-key violation (which exec() only logs), stranding the
		// report rows. issue_subscriptions/issue_comments key on the polymorphic
		// (type, id) pair with no DB-level FK, so they leave silent orphans instead.
		// notifications cascades, but only once the user delete actually succeeds.
		exec(cctx, `DELETE FROM manual_issue_reports WHERE reporter_id = $1`, u.ID)
		exec(cctx, `DELETE FROM issue_subscriptions WHERE subscriber_type = 'user' AND subscriber_id = $1`, u.ID)
		exec(cctx, `DELETE FROM issue_comments WHERE author_type = 'user' AND author_id = $1`, u.ID)
		exec(cctx, `DELETE FROM notifications WHERE user_id = $1`, u.ID)
		exec(cctx, `DELETE FROM "user" WHERE id = $1`, u.ID)
	})

	return u
}

// dashboardResult is what the harness reports back from a dashboard HTTP call.
type dashboardResult struct {
	Status int
	Body   string
}

// JSON decodes the response body into v, failing the test if it is not JSON. A dashboard route that
// 500s returns an HTML error page, so the failure message includes the raw body — that is usually
// the whole diagnosis (see B8).
func (r dashboardResult) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.Body), v); err != nil {
		body := r.Body
		if len(body) > 800 {
			body = body[:800] + "…"
		}
		t.Fatalf("dashboard response (status %d) was not JSON: %v\n  body: %s", r.Status, err, body)
	}
}

// dashboardRequest calls a dashboard route. Pass nil for user to call it unauthenticated.
func dashboardRequest(t *testing.T, method, path string, user *dashboardUser, body any) dashboardResult {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, cfg.DashboardURL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != nil {
		// Auth.js names the cookie `authjs.session-token` over plain HTTP and
		// `__Secure-authjs.session-token` behind TLS. The compose dashboard is HTTP.
		req.AddCookie(&http.Cookie{Name: "authjs.session-token", Value: user.SessionToken})
	}

	// Redirects are not followed: an unauthenticated call to a protected route answers with a redirect
	// to the sign-in page, and that status IS the assertion. Following it would report whatever the
	// sign-in page returns and quietly turn every authorization check into a pass.
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return dashboardResult{Status: resp.StatusCode, Body: string(raw)}
}

// ---------------------------------------------------------------------------------------------------
// Processor health
// ---------------------------------------------------------------------------------------------------

// processorHealth is the processor's /health body, which carries the DLQ counters wired in D10.
type processorHealth struct {
	Status             string `json:"status"`
	DLQDepth           int64  `json:"dlq_depth"`
	DLQPublishFailures int64  `json:"dlq_publish_failures"`
}

func readProcessorHealth(t *testing.T) processorHealth {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(cfg.ProcessorHealth + "/health")
	if err != nil {
		t.Fatalf("GET processor /health: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out processorHealth
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("processor /health was not JSON: %v\n  body: %s", err, raw)
	}
	return out
}

// ---------------------------------------------------------------------------------------------------
// Small shared utilities
// ---------------------------------------------------------------------------------------------------

// sha256Hex is the hex-encoded SHA-256 of s. Used for fingerprint reproduction and for deriving
// unique-but-deterministic session tokens.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// queryRow is a thin helper for the many one-value assertions in this package.
func queryRow(t *testing.T, dest any, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dest); err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("no rows for: %s", strings.Join(strings.Fields(sql), " "))
		}
		t.Fatalf("query %s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

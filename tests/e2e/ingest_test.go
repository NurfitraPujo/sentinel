// Use-case rows U1–U5: the 001-ingest matrix in docs/plans/E2E_RECOVERY_PLAN.md, driven against the
// real /ingest and /ingest/batch HTTP surfaces of the deployed ingestor.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// issueSummary is the subset of `issues` this file asserts on, read directly rather than through
// harness_test.go's issues()/onlyIssue(). Those select title, first_release, and last_release, none
// of which exist on the live issues table (verified against the deployed sentinel-postgres: `\d
// issues` lists no such columns, and no goose migration under packages/db-migrations/migrations ever
// adds them) — a pre-existing harness/schema-drift bug, not something U1/U6/U7 are about. Since
// harness_test.go must not be edited, this file selects only columns that actually exist.
type issueSummary struct {
	ID         string
	ErrorClass string
	Message    string
	Count      int
}

// ingestIssueSummaries reads every issue row for the fixture's project, scoped like every other query
// in this package.
func ingestIssueSummaries(t *testing.T, f *fixture) []issueSummary {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, error_class, message, count FROM issues WHERE project_id = $1 ORDER BY first_seen`, f.ProjectID)
	if err != nil {
		t.Fatalf("querying issues (summary): %v", err)
	}
	defer rows.Close()

	var out []issueSummary
	for rows.Next() {
		var s issueSummary
		if err := rows.Scan(&s.ID, &s.ErrorClass, &s.Message, &s.Count); err != nil {
			t.Fatalf("scanning issue summary: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating issue summaries: %v", err)
	}
	return out
}

// ingestOnlyIssueSummary is ingestIssueSummaries, asserting there is exactly one row.
func ingestOnlyIssueSummary(t *testing.T, f *fixture) issueSummary {
	t.Helper()
	all := ingestIssueSummaries(t, f)
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 issue in project %s, found %d", f.ProjectName, len(all))
	}
	return all[0]
}

// occurrenceSummary is the subset of `error_occurrences` this file asserts on, read directly rather
// than through harness_test.go's occurrences(). That query selects eo.message and orders by
// eo.timestamp, and NEITHER column exists on the live error_occurrences table (verified against the
// deployed sentinel-postgres: `\d error_occurrences` has no message or timestamp column — the message
// text is denormalized onto issues.message instead, and the ordering column is created_at). This is
// the same pre-existing harness/schema-drift bug documented on issueSummary above.
type occurrenceSummary struct {
	ID             string
	IssueID        string
	Environment    string
	Platform       string
	ReleaseVersion *string
	TraceID        *string
}

// ingestOccurrenceSummaries reads every error_occurrences row for the fixture's project.
func ingestOccurrenceSummaries(t *testing.T, f *fixture) []occurrenceSummary {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT eo.id::text, eo.issue_id::text, eo.environment, eo.platform, eo.release_version, eo.trace_id
		   FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1
		  ORDER BY eo.created_at`, f.ProjectID)
	if err != nil {
		t.Fatalf("querying occurrences (summary): %v", err)
	}
	defer rows.Close()

	var out []occurrenceSummary
	for rows.Next() {
		var o occurrenceSummary
		if err := rows.Scan(&o.ID, &o.IssueID, &o.Environment, &o.Platform, &o.ReleaseVersion, &o.TraceID); err != nil {
			t.Fatalf("scanning occurrence summary: %v", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating occurrence summaries: %v", err)
	}
	return out
}

// ingestWithout returns a copy of e with key removed entirely, so a field is genuinely absent from
// the wire payload rather than merely present-but-empty. U5 is specifically about an omitted field.
func ingestWithout(e event, key string) event {
	out := make(event, len(e))
	for k, v := range e {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}

// ingestBodyNames reports whether an ingest response body mentions the given field name — the loose
// but honest way to check "the error is field-level and about this field" without over-fitting to one
// exact error-string format (protovalidate's message text and field-path prefix can both vary).
func ingestBodyNames(body, field string) bool {
	return strings.Contains(body, field)
}

// ingestBatchErrIndices formats the indices present in a batch response's Errors, for failure
// messages.
func ingestBatchErrIndices(errs []struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}) []int {
	out := make([]int, len(errs))
	for i, e := range errs {
		out[i] = e.Index
	}
	return out
}

// TestU1_SingleEventHTTPCapture proves U1: an SDK-less HTTP POST of one canonical event to /ingest
// returns 202 and results in exactly one issues row and exactly one error_occurrences row.
func TestU1_SingleEventHTTPCapture(t *testing.T) {
	f := newFixture(t)

	res := f.ingest(f.newEvent())
	if res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest: got status %d, want 202\n  body: %s", res.Status, res.Body)
	}

	f.waitForIssues(1)
	f.waitForOccurrences(1)

	issue := ingestOnlyIssueSummary(t, f)
	if issue.ErrorClass != "E2ECanonicalError" {
		t.Fatalf("issue.ErrorClass = %q, want %q", issue.ErrorClass, "E2ECanonicalError")
	}
	if issue.Message != "e2e canonical error" {
		t.Fatalf("issue.Message = %q, want %q", issue.Message, "e2e canonical error")
	}

	occs := ingestOccurrenceSummaries(t, f)
	if len(occs) != 1 {
		t.Fatalf("expected exactly 1 occurrence, found %d", len(occs))
	}
	occ := occs[0]
	if occ.IssueID != issue.ID {
		t.Fatalf("occurrence.IssueID = %q, want it to point at the only issue %q", occ.IssueID, issue.ID)
	}
	if occ.Platform != "go" {
		t.Fatalf("occurrence.Platform = %q, want %q", occ.Platform, "go")
	}
	if occ.Environment != "e2e" {
		t.Fatalf("occurrence.Environment = %q, want %q", occ.Environment, "e2e")
	}
}

// TestU2_BatchOfTenLandsAllOccurrences proves U2: a batch of 10 valid events via /ingest/batch
// produces a response reporting {ingested: 10, failed: 0}, and all 10 land as error_occurrences rows.
func TestU2_BatchOfTenLandsAllOccurrences(t *testing.T) {
	f := newFixture(t)

	const n = 10
	events := make([]event, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, f.newEvent().with(event{
			"message": fmt.Sprintf("e2e batch message %d", i),
		}))
	}

	res := f.ingestBatch(events)
	if res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest/batch: got status %d, want 202\n  body: %s", res.Status, res.Body)
	}

	body := f.decodeBatch(res)
	if body.Ingested != n {
		t.Fatalf("batch response Ingested = %d, want %d\n  body: %s", body.Ingested, n, res.Body)
	}
	if body.Failed != 0 {
		t.Fatalf("batch response Failed = %d, want 0\n  body: %s", body.Failed, res.Body)
	}

	f.waitForOccurrences(n)
}

// TestU3_PartialBatchFailureNamesFailedIndices proves U3: a batch of 3 valid + 2 invalid events
// yields a 2xx whose body names exactly the 2 failed indices (not merely a failure count), and
// exactly 3 occurrences land in Postgres. A 2xx with an empty/absent error list would be the S15 lie
// this row exists to catch — asserting len(Errors) == 2 with the right indices is the point.
func TestU3_PartialBatchFailureNamesFailedIndices(t *testing.T) {
	f := newFixture(t)

	valid1 := f.newEvent().with(event{"message": "batch valid 1"})
	invalidPlatform := f.newEvent().with(event{"platform": ""}) // index 1: fails required+format validation
	valid2 := f.newEvent().with(event{"message": "batch valid 2"})
	invalidErrorClass := f.newEvent().with(event{"error_class": ""}) // index 3: fails required validation
	valid3 := f.newEvent().with(event{"message": "batch valid 3"})

	batch := []event{valid1, invalidPlatform, valid2, invalidErrorClass, valid3}

	res := f.ingestBatch(batch)
	// 3 of 5 succeeded, so the endpoint's own "at least one made it through" rule makes this 202.
	if res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest/batch: got status %d, want 202 (3 of 5 items are valid)\n  body: %s", res.Status, res.Body)
	}

	body := f.decodeBatch(res)
	if body.Ingested != 3 {
		t.Fatalf("batch response Ingested = %d, want 3\n  body: %s", body.Ingested, res.Body)
	}
	if body.Failed != 2 {
		t.Fatalf("batch response Failed = %d, want 2\n  body: %s", body.Failed, res.Body)
	}
	if len(body.Errors) != 2 {
		t.Fatalf("batch response Errors has %d entries, want exactly 2 naming the 2 failed indices — an empty/absent "+
			"error list on a 2xx is the exact S15 defect this test guards against\n  body: %s", len(body.Errors), res.Body)
	}

	byIndex := map[int]string{}
	for _, e := range body.Errors {
		byIndex[e.Index] = e.Message
	}
	if msg, ok := byIndex[1]; !ok {
		t.Fatalf("expected an error naming index 1 (empty platform), got indices %v\n  body: %s", ingestBatchErrIndices(body.Errors), res.Body)
	} else if !ingestBodyNames(msg, "platform") {
		t.Fatalf("error for index 1 does not name the platform field: %q", msg)
	}
	if msg, ok := byIndex[3]; !ok {
		t.Fatalf("expected an error naming index 3 (empty error_class), got indices %v\n  body: %s", ingestBatchErrIndices(body.Errors), res.Body)
	} else if !ingestBodyNames(msg, "error_class") {
		t.Fatalf("error for index 3 does not name the error_class field: %q", msg)
	}

	f.waitForOccurrences(3)
}

// TestU4_MessageLengthSweep proves U4, the S3 regression test. S3's bug was a `string.len = 10000`
// rule, which means EXACTLY 10000 bytes — that rejected every message that was not precisely that
// length, i.e. almost all real traffic. The authoritative bound, per
// packages/proto/sentinel/v1/error_event.proto's error_event.message CEL rule, is
// `this.message.size() <= 10000`: an inclusive upper bound, not an exact-match one. So 10000 itself
// must be accepted (202) and only 10001 must be rejected (400).
func TestU4_MessageLengthSweep(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		length     int
		wantStatus int
	}{
		{length: 0, wantStatus: http.StatusAccepted},
		{length: 1, wantStatus: http.StatusAccepted},
		{length: 9999, wantStatus: http.StatusAccepted},
		{length: 10000, wantStatus: http.StatusAccepted},
		{length: 10001, wantStatus: http.StatusBadRequest},
	}

	wantOccurrences := 0
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("len=%d", c.length), func(t *testing.T) {
			msg := strings.Repeat("m", c.length)
			res := f.ingest(f.newEvent().with(event{"message": msg}))
			if res.Status != c.wantStatus {
				t.Fatalf("message length %d: got status %d, want %d\n  body: %s", c.length, res.Status, c.wantStatus, res.Body)
			}
			if c.wantStatus == http.StatusAccepted {
				wantOccurrences++
			} else if !ingestBodyNames(res.Body, "message") {
				t.Fatalf("message length %d: 400 body does not name the message field: %q", c.length, res.Body)
			}
		})
	}

	f.waitForOccurrences(wantOccurrences)
}

// TestU5_MissingPlatformIsRejected proves U5: an event with `platform` entirely absent from the wire
// payload is rejected 400, and the response body names `platform` as the offending field.
func TestU5_MissingPlatformIsRejected(t *testing.T) {
	f := newFixture(t)

	res := f.ingest(ingestWithout(f.newEvent(), "platform"))
	if res.Status != http.StatusBadRequest {
		t.Fatalf("POST /ingest with no platform field: got status %d, want 400\n  body: %s", res.Status, res.Body)
	}
	if !ingestBodyNames(res.Body, "platform") {
		t.Fatalf("400 body does not name the platform field: %q", res.Body)
	}

	// A rejected event must never reach Postgres.
	if got := f.occurrenceCount(); got != 0 {
		t.Fatalf("rejected event still produced %d occurrence(s)", got)
	}
}

package store

import (
	"strings"
	"testing"
	"time"
)

// R13 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §10): the processor's issue upsert (and,
// transitively, alert dispatch -- Dispatch always follows a StoreEvent that reported
// stored==true) must never bump last_seen/count on -- and Dispatcher.Dispatch must never fire
// for -- a manual (`issue_type = 'user_report'`) issue row, even in the improbable event of a
// (project_id, fingerprint) collision. Previously that guarantee was purely structural (the
// processor never writes issue_type at all); this asserts the explicit predicate is present.
func TestIssuesUpsertConflictPredicate_ScopesToSystemErrorOnly(t *testing.T) {
	if !strings.Contains(issuesUpsertConflictPredicate, "issue_type = 'system_error'") {
		t.Fatalf(
			"issuesUpsertConflictPredicate = %q; want it to explicitly scope the ON CONFLICT DO UPDATE to issue_type = 'system_error', not rely on issue_type never being anything else",
			issuesUpsertConflictPredicate,
		)
	}
}

func TestIsRegressionVersion(t *testing.T) {
	tests := []struct {
		name              string
		releaseVersion    string
		resolvedInVersion string
		expected          bool
	}{
		{
			name:              "Newer minor release triggers regression",
			releaseVersion:    "v1.2.0",
			resolvedInVersion: "v1.1.0",
			expected:          true,
		},
		{
			name:              "Equal release version triggers regression",
			releaseVersion:    "v1.1.0",
			resolvedInVersion: "v1.1.0",
			expected:          true,
		},
		{
			name:              "Older patch release does not trigger regression",
			releaseVersion:    "v1.0.9",
			resolvedInVersion: "v1.1.0",
			expected:          false,
		},
		{
			name:              "Empty resolved version triggers regression",
			releaseVersion:    "v1.0.0",
			resolvedInVersion: "",
			expected:          true,
		},
		{
			name:              "Version without v prefix handled properly",
			releaseVersion:    "2.0.0",
			resolvedInVersion: "1.9.9",
			expected:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isRegressionVersion(tt.releaseVersion, tt.resolvedInVersion)
			if result != tt.expected {
				t.Errorf("isRegressionVersion(%q, %q) = %v; want %v",
					tt.releaseVersion, tt.resolvedInVersion, result, tt.expected)
			}
		})
	}
}

// TestDecideRegression is sentinel-releases-snooze §4.3's decision table, one case per row (be.md
// task 3(d); §11 property (4)): the SHA pair of flows I4/I5 ordered by first_seen where today's
// compare reads 0 == 0, R == X (decision 13), the first_seen tie (decision 3), and `next` with a
// NULL resolved_at (decision 27) included. Implementer guidance beneath the store boundary, not a
// boundary of its own — the same rows are driven through StoreEvent against a real Postgres in
// tests/integration/event_idempotency_test.go.
func TestDecideRegression(t *testing.T) {
	monday := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	wednesday := monday.Add(48 * time.Hour)
	previousFriday := monday.Add(-72 * time.Hour)
	resolvedAt := monday.Add(12 * time.Hour) // Monday 21:00Z: after a1b2c3d's first sighting

	present := func(at time.Time) releaseRow { return releaseRow{present: true, firstSeen: at} }
	absent := releaseRow{}
	ptr := func(at time.Time) *time.Time { return &at }

	tests := []struct {
		name       string
		release    string // R, the incoming occurrence's version
		resolved   string // X, the stored resolved_in_version ('' for NULL)
		resolvedAt *time.Time
		rowR, rowX releaseRow
		want       bool
	}{
		// --- unchanged today ---
		{name: "X empty/NULL: any R regresses", release: "9f8e7d6", resolved: "", resolvedAt: ptr(resolvedAt), rowR: present(wednesday), rowX: absent, want: true},
		{name: "R empty: regresses whatever X is", release: "", resolved: "a1b2c3d", resolvedAt: ptr(resolvedAt), rowR: absent, rowX: present(monday), want: true},
		{name: "R empty against next: regresses", release: "", resolved: "next", resolvedAt: ptr(resolvedAt), rowR: absent, rowX: absent, want: true},

		// --- X = next (decisions 6, 27, 32) ---
		{name: "next, rowR absent: a release first seen now is newer than the resolution", release: "9f8e7d6", resolved: "next", resolvedAt: ptr(resolvedAt), rowR: absent, rowX: absent, want: true},
		{name: "next, rowR.first_seen > resolved_at: regression", release: "9f8e7d6", resolved: "next", resolvedAt: ptr(resolvedAt), rowR: present(wednesday), rowX: absent, want: true},
		{name: "next, rowR.first_seen < resolved_at: R existed when the resolution was made", release: "a1b2c3d", resolved: "next", resolvedAt: ptr(resolvedAt), rowR: present(monday), rowX: absent, want: false},
		{name: "next, rowR.first_seen == resolved_at: the <= edge is not a regression", release: "a1b2c3d", resolved: "next", resolvedAt: ptr(resolvedAt), rowR: present(resolvedAt), rowX: absent, want: false},
		{name: "next, resolved_at NULL (pre-feature batch resolve): nothing to compare against", release: "a1b2c3d", resolved: "next", resolvedAt: nil, rowR: present(monday), rowX: absent, want: true},

		// --- X a version, rowX absent: the fallback, unchanged (decision 3; flow I6) ---
		{name: "fallback: 1.2.3 against a typed 9.9.9 is older", release: "1.2.3", resolved: "9.9.9", resolvedAt: ptr(resolvedAt), rowR: present(wednesday), rowX: absent, want: false},
		{name: "fallback: 10.0.0 against a typed 9.9.9 is newer", release: "10.0.0", resolved: "9.9.9", resolvedAt: ptr(resolvedAt), rowR: present(wednesday), rowX: absent, want: true},
		{name: "fallback: equal is a regression, as today", release: "9.9.9", resolved: "9.9.9", resolvedAt: ptr(resolvedAt), rowR: absent, rowX: absent, want: true},

		// --- X a version, rowX present (decisions 1, 3, 13, 14; flows I4, I5, I7) ---
		{name: "rowX present, rowR absent: a first sighting is newest (v1.2.1 after v1.3.0)", release: "v1.2.1", resolved: "v1.3.0", resolvedAt: ptr(resolvedAt), rowR: absent, rowX: present(monday), want: true},
		{name: "R == X: the fix did not land", release: "a1b2c3d", resolved: "a1b2c3d", resolvedAt: ptr(resolvedAt), rowR: present(monday), rowX: present(monday), want: true},
		{name: "SHA pair I4: 9f8e7d6 (Wednesday) after a1b2c3d (Monday) — today's compare says 0 == 0", release: "9f8e7d6", resolved: "a1b2c3d", resolvedAt: ptr(resolvedAt), rowR: present(wednesday), rowX: present(monday), want: true},
		{name: "SHA pair I5: 0000aaa (previous Friday) before a1b2c3d (Monday) — today's compare would regress it", release: "0000aaa", resolved: "a1b2c3d", resolvedAt: ptr(resolvedAt), rowR: present(previousFriday), rowX: present(monday), want: false},
		{name: "tie, R != X: today's compare decides — v1.2.0 over v1.1.0", release: "v1.2.0", resolved: "v1.1.0", resolvedAt: ptr(resolvedAt), rowR: present(monday), rowX: present(monday), want: true},
		{name: "tie, R != X: today's compare decides — v1.0.9 under v1.1.0", release: "v1.0.9", resolved: "v1.1.0", resolvedAt: ptr(resolvedAt), rowR: present(monday), rowX: present(monday), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideRegression(tt.release, tt.resolved, tt.resolvedAt, tt.rowR, tt.rowX)
			if got != tt.want {
				t.Errorf("decideRegression(R=%q, X=%q, resolvedAt=%v, rowR=%+v, rowX=%+v) = %v; want %v",
					tt.release, tt.resolved, tt.resolvedAt, tt.rowR, tt.rowX, got, tt.want)
			}
		})
	}
}

// TestDecideRegression_IssueWithoutOccurrences pins that the decision needs no occurrence rows:
// decideRegression takes only the issue's resolution columns, the two `releases` rows and the
// incoming release. Retention (sentinel-retention-resolved-ignored-kept) keeps a resolved issue's
// row after every occurrence has aged out, so each case below is "a resolved issue whose
// occurrences are gone receives one event".
func TestDecideRegression_IssueWithoutOccurrences(t *testing.T) {
	monday := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	wednesday := monday.Add(48 * time.Hour)
	friday := monday.Add(-72 * time.Hour)
	resolvedAt := monday.Add(12 * time.Hour)
	present := func(at time.Time) releaseRow { return releaseRow{present: true, firstSeen: at} }

	tests := []struct {
		name       string
		release    string
		resolved   string
		resolvedAt *time.Time
		rowR, rowX releaseRow
		want       bool
	}{
		{"resolved with no version: any later event regresses", "2.0.0", "", &resolvedAt, present(wednesday), releaseRow{}, true},
		{"resolved in 1.0.0, event in a later release", "2.0.0", "1.0.0", &resolvedAt, present(wednesday), present(monday), true},
		{"resolved in 1.0.0, event in the same release: the fix did not land", "1.0.0", "1.0.0", &resolvedAt, present(monday), present(monday), true},
		{"resolved in 2.0.0, event in an earlier release: not a regression", "1.0.0", "2.0.0", &resolvedAt, present(friday), present(monday), false},
		{"resolved in next, event in a release first seen after the resolution", "2.0.0", "next", &resolvedAt, present(wednesday), releaseRow{}, true},
		{"resolved in next, event in a release that predates the resolution", "1.0.0", "next", &resolvedAt, present(monday), releaseRow{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideRegression(tt.release, tt.resolved, tt.resolvedAt, tt.rowR, tt.rowX); got != tt.want {
				t.Errorf("decideRegression(R=%q, X=%q) = %v; want %v", tt.release, tt.resolved, got, tt.want)
			}
		})
	}
}

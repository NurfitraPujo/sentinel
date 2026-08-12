package store

import (
	"strings"
	"testing"
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

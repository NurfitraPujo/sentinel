package unit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// sentinel-project-environments §4.7 — environment-scoped alert configs.
//
// §11 puts this assertion here rather than behind NATS + Postgres deliberately: whether an
// occurrence in an unsubscribed environment counts toward a config's threshold is a pure decision
// over (cfg.Environments, issue.environment), and driving it end-to-end would make the most
// important assertion in this feature the slowest one.
// ---------------------------------------------------------------------------

const envScopeProjectID = "env-scope-project"

// envScopedConfig builds a project-scoped email config with the given scope. environments == nil is
// the stored NULL — every environment, which is what every pre-feature row carries.
func envScopedConfig(threshold int, environments []string) *alerts.AlertConfig {
	return &alerts.AlertConfig{
		ID:                 "env-cfg",
		ProjectID:          envScopeProjectID,
		OrganizationID:     "env-scope-org",
		Channel:            "email",
		ChannelConfig:      map[string]interface{}{"to": "oncall@example.test"},
		FrequencyThreshold: threshold,
		FrequencyWindow:    time.Minute,
		Enabled:            true,
		Environments:       environments,
	}
}

// captureAlerts is captureDispatch's sibling: resolution tests only need to know WHICH config sent,
// these need the Alert itself (its Environment reaches the notifier's subject).
func captureAlerts(t *testing.T) (*alerts.Dispatcher, func() []*alerts.Alert) {
	t.Helper()
	d := alerts.NewDispatcherForTest(nil)

	var mu sync.Mutex
	var sent []*alerts.Alert
	d.SetSenderForTest(func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, alert)
	})

	return d, func() []*alerts.Alert {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*alerts.Alert, len(sent))
		copy(out, sent)
		return out
	}
}

// TestAlertsDispatcher_EnvironmentScope is the table §4.7's acceptance asks for: one row per
// (cfg.Environments, occurrence environment) pair, with threshold 1 so a covered occurrence always
// fires on its first delivery and an uncovered one is unambiguously silence rather than latency.
func TestAlertsDispatcher_EnvironmentScope(t *testing.T) {
	tests := []struct {
		name         string
		environments []string
		environment  string
		wantSent     bool
	}{
		{
			// Decision 6: a NULL scope covers every environment exactly as it does today. Every
			// config that existed before this feature is this row, so getting it wrong silences
			// every alert anyone is currently relying on.
			name:         "nil scope covers production",
			environments: nil,
			environment:  "production",
			wantSent:     true,
		},
		{
			name:         "nil scope covers an environment nobody has ever heard of",
			environments: nil,
			environment:  "pre-prod",
			wantSent:     true,
		},
		{
			name:         "single-entry scope covers its own environment",
			environments: []string{"production"},
			environment:  "production",
			wantSent:     true,
		},
		{
			name:         "single-entry scope does not cover another environment",
			environments: []string{"production"},
			environment:  "staging",
			wantSent:     false,
		},
		{
			name:         "multi-entry scope covers any of its entries",
			environments: []string{"production", "eu-west-1"},
			environment:  "eu-west-1",
			wantSent:     true,
		},
		{
			name:         "multi-entry scope covers nothing outside them",
			environments: []string{"production", "eu-west-1"},
			environment:  "staging-2",
			wantSent:     false,
		},
		{
			// The names are matched exactly and are never validated against the registry (decision
			// 8): a config may be scoped to a name nobody has declared, and a name that never
			// matches simply never fires. Case is part of the name.
			name:         "match is exact, not case-insensitive",
			environments: []string{"production"},
			environment:  "Production",
			wantSent:     false,
		},
		{
			// §4.7 answers `[]` with a 400 so this cannot arrive from the API, but a hand-written
			// row can still hold it, and "covers nothing" is the only reading consistent with the
			// 400's stated reason.
			name:         "empty-but-non-nil scope covers nothing",
			environments: []string{},
			environment:  "production",
			wantSent:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, sent := captureAlerts(t)
			d.SetConfigsForTest(map[string]*alerts.AlertConfig{
				envScopeProjectID: envScopedConfig(1, tt.environments),
			})

			d.Dispatch(context.Background(), "issue-1", envScopeProjectID, tt.environment, "TestError", "boom")

			if tt.wantSent {
				require.Len(t, sent(), 1, "expected an alert for an occurrence this config covers")
				assert.Equal(t, tt.environment, sent()[0].Environment, "the Alert must carry the environment it fired for")
			} else {
				assert.Empty(t, sent(), "an occurrence outside the config's scope must send nothing")
			}
		})
	}
}

// TestAlertsDispatcher_OutOfScopeOccurrenceDoesNotCount is the half the table above cannot see, and
// it is the one that matters: an out-of-scope occurrence must not INCREMENT
// projectID+":"+issueID+":"+cfg.ID toward the threshold.
//
// A dispatcher that counted every occurrence and only suppressed the SEND would pass every row
// above and still be wrong — a production-scoped config would reach its threshold at a rate set by
// staging traffic and then page on the first production occurrence that arrived, which is a
// worse failure than not scoping at all because it looks like it is working.
//
// Threshold 2, so the assertion is about the counter and not about the send: two staging
// occurrences followed by ONE production occurrence must be silent (the counter stands at 1), and
// only the second production occurrence fires.
func TestAlertsDispatcher_OutOfScopeOccurrenceDoesNotCount(t *testing.T) {
	d, sent := captureAlerts(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{
		envScopeProjectID: envScopedConfig(2, []string{"production"}),
	})

	d.Dispatch(context.Background(), "issue-1", envScopeProjectID, "staging", "TestError", "boom")
	d.Dispatch(context.Background(), "issue-1", envScopeProjectID, "staging", "TestError", "boom")
	require.Empty(t, sent(), "staging occurrences must not reach a production-scoped config at all")

	d.Dispatch(context.Background(), "issue-1", envScopeProjectID, "production", "TestError", "boom")
	require.Empty(t, sent(), "the two staging occurrences must not have counted toward the threshold")

	d.Dispatch(context.Background(), "issue-1", envScopeProjectID, "production", "TestError", "boom")
	require.Len(t, sent(), 1, "the second production occurrence reaches threshold 2 and fires")
	assert.Equal(t, "production", sent()[0].Environment)
}

// TestAlertsDispatcher_EnvironmentScopeAppliesPerConfig proves the decision is made per config
// rather than per dispatch: one occurrence, two configs resolving for the same project, and only the
// one whose scope covers it fires. Dispatch returning early on the first uncovered config would pass
// every other test in this file.
func TestAlertsDispatcher_EnvironmentScopeAppliesPerConfig(t *testing.T) {
	d, sent := captureAlerts(t)

	scoped := envScopedConfig(1, []string{"production"})
	scoped.ID = "scoped-cfg"
	scoped.ChannelConfig = map[string]interface{}{"to": "prod-oncall@example.test"}

	unscoped := envScopedConfig(1, nil)
	unscoped.ID = "unscoped-cfg"
	unscoped.ChannelConfig = map[string]interface{}{"to": "everything@example.test"}

	d.SetProjectConfigsForTest(map[string][]*alerts.AlertConfig{
		envScopeProjectID: {scoped, unscoped},
	})

	d.Dispatch(context.Background(), "issue-1", envScopeProjectID, "staging", "TestError", "boom")

	require.Len(t, sent(), 1, "the NULL-scoped config still covers staging even though the other does not")
	assert.Equal(t, "staging", sent()[0].Environment)
}

// ---------------------------------------------------------------------------
// alerts.AlertSubject (§4.7 — notify.go names the environment)
// ---------------------------------------------------------------------------

// TestAlertSubject is table-driven over the environment because that is the field this feature adds
// and the one an operator reads first. Since decision 7 made environment part of issue identity, the
// same error in staging and in production is two issues and two alerts, and a subject carrying only
// the issue id makes the page that matters indistinguishable at a glance from the one that does not.
func TestAlertSubject(t *testing.T) {
	tests := []struct {
		name        string
		alert       *alerts.Alert
		wantSubject string
	}{
		{
			name:        "production",
			alert:       &alerts.Alert{IssueID: "issue-1", Environment: "production"},
			wantSubject: "Sentinel alert: [production] issue issue-1",
		},
		{
			name:        "a widened name (decision 10) is printed verbatim",
			alert:       &alerts.Alert{IssueID: "issue-2", Environment: "eu-west-1"},
			wantSubject: "Sentinel alert: [eu-west-1] issue issue-2",
		},
		{
			name:        "staging-2",
			alert:       &alerts.Alert{IssueID: "issue-3", Environment: "staging-2"},
			wantSubject: "Sentinel alert: [staging-2] issue issue-3",
		},
		{
			// DispatchOperational's case: a DLQ backlog belongs to no environment and no project.
			// The environment is omitted rather than printed as an empty bracket.
			name:        "no environment falls back to the pre-feature subject",
			alert:       &alerts.Alert{IssueID: "dlq-backlog"},
			wantSubject: "Sentinel alert: issue dlq-backlog",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantSubject, alerts.AlertSubject(tt.alert))
		})
	}
}

// Package e2e — this file covers matrix rows U26 and U27 (docs/plans/E2E_RECOVERY_PLAN.md, "## P7 — The
// E2E proof harness", ~line 834): alerting.
//
//   - U26: a new issue arrives with an alert configured → the notifier is invoked within one event.
//     This is S8's regression test (docs/memory/VERIFIED_STATE.md): NewProcessorService never
//     constructed the dispatcher, so alerting — ~425 LOC, ~1,100 lines of passing package tests —
//     never ran in production. It has since been wired (apps/processor-go/service/processor_service.go)
//     and had two further delivery-blocking bugs fixed (channel_config was scanned and discarded;
//     the new/regressed gate cancelled out the frequency-threshold counter). This file proves it fires
//     against the real deployed processor binary, not just in-process package tests.
//   - U27: an alert config created "in the UI" takes effect without a 5-minute wait.
//
// # Observable signal (read this before trusting a PASS)
//
// A Go test cannot read a Slack workspace or an inbox. Of the three candidates the assignment lists:
//
//  1. Webhook channel: does NOT exist. alert_configs.channel has `CHECK (channel IN ('email',
//     'telegram'))` (packages/db-migrations/migrations/1716508800_init.sql:66) and
//     apps/processor-go/alerts/notify.go's BuildSender switches on exactly those two values, dropping
//     anything else. There is no third channel type to stand a webhook listener behind.
//  2. A DB delivery-log row: does NOT exist either. `alert_configs` only stores config; `audit_logs`
//     only ever gets `issue_upserted` / `occurrence_created` rows (processor_service.go). The
//     dispatcher and both notifiers (email/telegram) write to no table on send, success, or failure.
//  3. Processor logs: this is therefore the ONLY signal available without modifying the processor
//     (forbidden by this assignment) or restarting the compose stack (forbidden by the coordinator,
//     shared with other agents). Every test below reads `podman logs sentinel-processor` (falling back
//     to `docker logs`).
//
// Signal strength, stated plainly:
//   - TestU26_NoConfigMeansNoDispatch and TestU27's failure-mode line
//     (`alerts: email channel_config missing "to" for project=<uuid> issue=<uuid>...`) embed this
//     fixture's own project/issue UUIDs, so those matches are precisely attributable to this test run.
//   - TestU26_ExistingConfigFiresNotifierWithinOneEvent's positive signal
//     (`Email attempt N failed` / `Email sent successfully` / `Email failed after N attempts`, all in
//     apps/processor-go/notifiers/email.go) does NOT embed a project or issue id — email.go logs only
//     the recipient address, and only on the success line, which this environment can never reach (see
//     below). Under concurrent alerting e2e runs from other agents against the same shared processor,
//     there is a small residual chance of a false PASS from another test's email dispatch landing in
//     this test's `--since` window. There is no plausible false NEGATIVE: if dispatch genuinely doesn't
//     fire for this fixture, nothing puts these strings in the logs on our behalf. This is the
//     "weakest, but honestly labeled" signal the assignment anticipates.
//
// This environment cannot deliver real email or Telegram: the compose processor service (docker-compose.yml)
// sets no ALERT_* environment variables, so apps/processor-go/alerts/notify.go's NotifierConfigFromEnv
// defaults apply — SMTP host "localhost:587" (nothing listens inside the processor container) and an
// empty Telegram bot token/chat id. That is exactly why the *retry-and-fail* log lines, not the
// success line, are what this file asserts on for the email channel: they are proof the real
// EmailWorker.Send was invoked and attempted a real SMTP dial, which is as far as "the notifier was
// invoked" can be observed without a working mail relay.
//
// # A genuine defect found while building this: channel_config field-name drift (not S8)
//
// apps/dashboard-web/src/routes/api/alerts/+server.ts's POST handler writes
// `channelConfig: { target: body.channelTarget }` (line 122) — key "target". But
// apps/processor-go/alerts/notify.go's BuildSender reads `alertCfg.ChannelConfig["to"]` for the email
// channel and `alertCfg.ChannelConfig["chat_id"]` for telegram (lines 78, 100). Neither processor-side
// key is ever present in a config the dashboard API writes: "target" matches nothing. Every alert
// config created through the real, live /api/alerts route is therefore permanently undeliverable —
// the processor's own log line for it literally says `channel_config missing "to"`, dropping every
// single occurrence, forever, regardless of how quickly the dispatcher's cache picks the row up. This
// is a distinct defect from S8 (S8 was "never constructed"; this is "constructed and reachable, wired
// correctly to the DB row, but the two sides of the wire contract disagree on a JSON key" — exactly the
// B5 cross-boundary-payload class CLAUDE.md warns about). TestU27 below is written to reveal this
// rather than route around it: see its doc comment.
//
// # Config-refresh cadence: a second, separate gap TestU27 exists to surface
//
// apps/processor-go/alerts/dispatcher.go's Dispatcher loads alert_configs exactly twice in its
// lifetime per row: once synchronously at construction (NewDispatcher, called once by
// NewProcessorService, called once by processor main() at boot) and then only on a hardcoded
// `time.NewTicker(5 * time.Minute)` (loadConfigs, dispatcher.go:117) — there is no LISTEN/NOTIFY, no
// per-request check, no shorter interval, and no way to force a refresh from outside the process
// (RefreshConfigsForTest is an unexported-query wrapper only reachable through a Go method call, which
// only an in-process test — see tests/integration/procgo_alerting_degradation_test.go:371-374 — can
// reach; an external e2e test talking to the real container cannot). Concretely: a config row created
// any time after the processor's boot-time snapshot is invisible to Dispatch for up to five minutes.
// TestU27 creates its config well after the processor booted and checks for effect well inside that
// window, on purpose — that gap is precisely what U27's row exists to catch.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	osexec "os/exec"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------------
// Helpers (area-prefixed "alerts" so they cannot collide with another agent's file in this package)
// ---------------------------------------------------------------------------------------------------

// alertsProcessorContainer is the compose container_name for the processor service
// (docker-compose.yml), fixed regardless of whether the stack was brought up with docker-compose or
// podman-compose.
const alertsProcessorContainer = "sentinel-processor"

// alertsTickerPeriod mirrors apps/processor-go/alerts/dispatcher.go's loadConfigs ticker. See the
// package doc comment above ("Config-refresh cadence").
const alertsTickerPeriod = 5 * time.Minute

// alertsExistingConfigBudget bounds TestU26_ExistingConfigFiresNotifierWithinOneEvent. It has to
// exceed alertsTickerPeriod: the config is seeded well after this (already-running, shared) processor
// booted, so — unlike the in-process integration test that can construct the Dispatcher *after*
// seeding — an external e2e test genuinely cannot make the row visible faster than the next tick.
// What IS still "within one event" here, and what this budget does not paper over, is that once the
// config is loaded, dispatch is synchronous per-occurrence (Dispatcher.Dispatch, called inline from
// ProcessorService.dispatchAlert): no separate wait is needed between the config becoming visible and
// the very next occurrence triggering it. See alertsPollForNotifierEvidence's resend loop.
const alertsExistingConfigBudget = alertsTickerPeriod + 45*time.Second

// alertsResendInterval is how often the resend loop in alertsPollForNotifierEvidence re-ingests the
// same event while waiting for the config to become visible. Coarser than the harness's normal
// waitFor cadence (150ms) deliberately: this loop's side effect is a real HTTP ingest, and the wait
// budget above can run to nearly six minutes.
const alertsResendInterval = 8 * time.Second

// alertsFreshConfigBudget bounds TestU27. It is deliberately far below alertsTickerPeriod — that gap
// between "5 minutes" and this budget is the entire point of U27's row.
const alertsFreshConfigBudget = 90 * time.Second

// alertsSeedConfig inserts an alert_configs row directly, scoped to f.ProjectID (cleaned up by
// fixture.cleanup, which already deletes `alert_configs WHERE project_id = $1`). Deliberately bypasses
// the dashboard's /api/alerts write path: this helper writes the exact channel_config keys
// apps/processor-go/alerts/notify.go's BuildSender reads ("to" for email, "chat_id" for telegram), so
// TestU26 isolates the dispatcher/notifier wiring (S8) from the separate field-name-drift defect the
// dashboard API has (see the package doc comment) — that defect is TestU27's concern, exercised there
// through the real API as the assignment requires.
func alertsSeedConfig(t *testing.T, f *fixture, channel string, channelConfig map[string]any, threshold, windowSeconds int, enabled bool) string {
	t.Helper()

	raw, err := json.Marshal(channelConfig)
	if err != nil {
		t.Fatalf("marshalling alert channel_config: %v", err)
	}

	var id string
	if err := pool.QueryRow(context.Background(),
		// organization_id is NOT NULL as of 1722100000 (alert configs are two-layer: project-scoped when
		// project_id is set, organization-wide when it is NULL). Deriving it from the project rather than
		// passing it in also documents the invariant the composite FK enforces — a config's organization
		// must be the one that owns its project.
		`INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES ($1, (SELECT organization_id FROM projects WHERE id = $1), $2, $3::jsonb, $4, $5, $6) RETURNING id::text`,
		f.ProjectID, channel, raw, threshold, windowSeconds, enabled,
	).Scan(&id); err != nil {
		t.Fatalf("seeding alert_configs for project %s: %v", f.ProjectID, err)
	}
	return id
}

// alertsSeedProjectMember gives userID project-level access to f's project via `project_members`
// (packages/db-migrations/migrations/1716550000_add_project_members.sql), which is what
// apps/dashboard-web/src/routes/api/alerts/+server.ts's POST handler actually authorizes against.
// newDashboardUser (harness_test.go) only seeds an *organization*-level membership
// (organization_members), which this route never queries — without this, every POST to /api/alerts
// would 403 with "Access denied to this project" regardless of role. Cleaned up twice, harmlessly, by
// both f.cleanup (by project_id) and the dashboardUser's own t.Cleanup (by user_id).
func alertsSeedProjectMember(t *testing.T, f *fixture, userID, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO project_members (user_id, project_id, role) VALUES ($1, $2, $3)`,
		userID, f.ProjectID, role,
	); err != nil {
		t.Fatalf("seeding project_members (user=%s project=%s role=%s): %v", userID, f.ProjectID, role, err)
	}
}

// alertsProcessorLogsSince returns the processor container's combined stdout/stderr emitted at or
// after `since`. See the package doc comment for why this is the observable signal this file uses.
func alertsProcessorLogsSince(t *testing.T, since time.Time) string {
	t.Helper()

	ts := since.UTC().Format("2006-01-02T15:04:05.000000000Z")
	var lastErr error
	for _, bin := range []string{"podman", "docker"} {
		out, err := osexec.Command(bin, "logs", "--since", ts, alertsProcessorContainer).CombinedOutput()
		if err == nil {
			return string(out)
		}
		lastErr = fmt.Errorf("%s logs %s: %w (output: %s)", bin, alertsProcessorContainer, err, string(out))
	}
	t.Fatalf("could not read %s container logs via podman or docker: %v", alertsProcessorContainer, lastErr)
	return ""
}

// alertsTail returns at most the last n lines of s, for compact failure diagnostics against what can
// be a multi-minute, multi-thousand-line log capture.
func alertsTail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return "…(truncated)…\n" + strings.Join(lines[len(lines)-n:], "\n")
}

// alertsPollForNotifierEvidence resends ev (a fixed, already-valid event) roughly every
// alertsResendInterval until the processor's logs (captured from `since`) contain one of `markers`, or
// `deadline` elapses. Resending is necessary, not cosmetic: apps/processor-go/alerts.Dispatcher.Dispatch
// is a per-occurrence, synchronous check against whatever config snapshot is currently loaded — a
// config that becomes visible to the dispatcher a minute from now does nothing for an occurrence
// processed a minute ago. Because Dispatch deletes each issue's counter immediately after it fires
// (dispatcher.go:224-226), a frequency_threshold=1 config dispatches again on every resend once loaded,
// not just the first time — so this loop reliably observes the config the moment the processor's
// background ticker (or, in principle, a lucky construction-time load) makes it visible.
func alertsPollForNotifierEvidence(t *testing.T, f *fixture, ev event, since time.Time, deadline time.Duration, markers ...string) (matched string, logs string) {
	t.Helper()

	deadlineAt := time.Now().Add(deadline)
	for {
		res := f.ingest(ev)
		if res.Status != http.StatusAccepted {
			t.Fatalf("ingest during alert-evidence poll: status %d body %s", res.Status, res.Body)
		}

		checkUntil := time.Now().Add(alertsResendInterval)
		for time.Now().Before(checkUntil) {
			logs = alertsProcessorLogsSince(t, since)
			for _, m := range markers {
				if strings.Contains(logs, m) {
					return m, logs
				}
			}
			if time.Now().After(deadlineAt) {
				t.Fatalf("timed out after %s waiting for any of %v in processor logs for project %s\n  log tail:\n%s",
					deadline, markers, f.ProjectID, alertsTail(logs, 40))
			}
			time.Sleep(2 * time.Second)
		}
		if time.Now().After(deadlineAt) {
			t.Fatalf("timed out after %s waiting for any of %v in processor logs for project %s\n  log tail:\n%s",
				deadline, markers, f.ProjectID, alertsTail(logs, 40))
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// U26 — new issue with an alert configured → notifier invoked within one event (S8)
// ---------------------------------------------------------------------------------------------------

// TestU26_ExistingConfigFiresNotifierWithinOneEvent proves matrix row U26: given an enabled alert
// config the dispatcher has loaded, a new issue's very first occurrence dispatches to the real,
// wired notifier — not the pre-S8-fix no-op ("ALERT: %s via %s - %s", logged only when
// Dispatcher.SetSender was never called, dispatcher.go:239) and not merely a passing package test.
//
// frequency_threshold is set to 1 deliberately: dispatcher.go's comment on this exact point (and
// VERIFIED_STATE.md's S8 entry) explains that gating dispatch on "new or regressed" while ALSO
// requiring frequency_threshold (DEFAULT 50) hits before sending meant no realistic config could ever
// fire, because an issue is new exactly once. threshold=1 is what makes "alert on first sight" express
// -able at all post-fix, and is exactly what "within one event" means here: the first (and, in this
// test, only meaningfully distinct) occurrence must be enough.
//
// Signal: apps/processor-go/notifiers/email.go's retry-loop log lines ("Email attempt N failed" /
// "Email sent successfully" / "Email failed after N attempts"). These originate only inside
// EmailWorker.processQueue, reached only via a real EmailWorker.Send call from
// alerts.BuildSender's email case — which itself is only reached when Dispatcher.sendAlert's
// senderForTest is non-nil, i.e., only when NewProcessorService's SetSender wiring is intact. If S8
// regressed (dispatcher never constructed, or SetSender never called), sendAlert would fall back to
// logging "ALERT: ... via email - ..." instead — a string this test does not match — so this
// assertion fails cleanly on that regression rather than passing by accident.
//
// This environment has no working SMTP relay (see package doc comment), so the specific outcome
// expected is "Email attempt 1 failed" (and, ~6s later per email.go's 1s/5s backoff schedule, "Email
// failed after 3 attempts") rather than the success line — that failure is itself proof positive that
// EmailWorker attempted a real network send, which is as far as "notifier invoked" can be observed
// without a real mail relay in this stack.
func TestU26_ExistingConfigFiresNotifierWithinOneEvent(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	to := "alert-" + f.ProjectName + "@example.test"
	alertsSeedConfig(t, f, "email", map[string]any{"to": to}, 1, 60, true)

	since := time.Now()
	ev := f.newEvent()

	marker, logs := alertsPollForNotifierEvidence(t, f, ev, since, alertsExistingConfigBudget,
		`email channel_config missing "to"`, // see below: this would itself be a surprising, separate failure
		"Email attempt",
		"Email sent successfully",
		"Email failed after",
	)

	if strings.Contains(marker, `missing "to"`) {
		t.Fatalf(
			"processor dropped the alert for project=%s citing a missing \"to\" field, but this test's "+
				"channel_config was seeded directly with {\"to\": %q} — the config the dispatcher loaded "+
				"does not match what was written; this is a different, new bug from the field-name drift "+
				"TestU27 documents (that one is about the DASHBOARD's write path, not direct SQL)\n  log tail:\n%s",
			f.ProjectID, to, alertsTail(logs, 40))
	}

	t.Logf("observed notifier invocation via processor log marker %q", marker)
}

// TestU26_NoConfigMeansNoDispatch is a cheap negative control for the test above: it proves
// alertsPollForNotifierEvidence's detection method is not a rubber stamp that would report a PASS
// regardless of whether alerting actually ran. With no alert_configs row at all for this project,
// Dispatcher.Dispatch's very first line (`cfg, exists := d.configs[projectID]; if !exists ... return`)
// means BuildSender is never even called, so none of BuildSender's log lines — every one of which
// includes `project=<uuid>` (apps/processor-go/alerts/notify.go) — can mention this fixture's project
// id. This also stands in for "would this test fail if dispatch stopped happening": if
// NewProcessorService ever stops wiring the dispatcher (the literal S8 regression) or Dispatch's
// early-return logic changes to fire regardless of config existence, THIS test starts failing on its
// own project-scoped grep, independent of the (weaker, non-project-scoped) signal the test above uses.
func TestU26_NoConfigMeansNoDispatch(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	since := time.Now()
	res := f.ingest(f.newEvent())
	if res.Status != http.StatusAccepted {
		t.Fatalf("ingest: status %d body %s", res.Status, res.Body)
	}
	// Confirms the event pipeline itself works, isolating "no alert config configured" from "the
	// whole pipeline is broken" as the reason nothing alert-related shows up below.
	f.waitForOccurrences(1)

	const window = 20 * time.Second
	needle := fmt.Sprintf("project=%s", f.ProjectID)
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		logs := alertsProcessorLogsSince(t, since)
		if strings.Contains(logs, needle) {
			t.Fatalf("processor logged an alert-related line mentioning %s even though project %s has no "+
				"alert_configs row at all — Dispatch should be a no-op here:\n%s", needle, f.ProjectID, alertsTail(logs, 40))
		}
		time.Sleep(2 * time.Second)
	}
}

// ---------------------------------------------------------------------------------------------------
// U27 — alert config created in the UI takes effect without a 5-minute wait
// ---------------------------------------------------------------------------------------------------

// TestU27_ConfigCreatedViaDashboardTakesEffectPromptly proves (or disproves) matrix row U27 against the
// REAL dashboard API, per the assignment's instruction to exercise creation "through the real API".
//
// Given everything documented in the package comment above, the expected, predicted outcome of this
// test — verified by reading, not assumed — is FAIL, for two independent, compounding reasons this test
// is written to surface rather than route around:
//
//  1. Field-name drift: POST /api/alerts writes channel_config as {"target": <address>}
//     (+server.ts:122); the processor's BuildSender reads ChannelConfig["to"] for email. A config
//     created this way is unconditionally undeliverable — the processor logs
//     `alerts: email channel_config missing "to" for project=<id> issue=<id>, dropping alert` for
//     every single occurrence, forever, independent of timing.
//  2. Refresh cadence: even setting (1) aside, this config is created long after the shared processor
//     booted, and the dispatcher has no way to notice a new row before its next 5-minute ticker tick
//     (dispatcher.go:117); alertsFreshConfigBudget (90s) is deliberately far short of that.
//
// This test asserts the full, correct bar — real notifier invocation, promptly — via the same markers
// as TestU26 above. If it fails, the failure message distinguishes which of the two reasons (or
// neither symptom at all, meaning something else) was observed, because that distinction is itself the
// finding to report, not something to average away.
func TestU27_ConfigCreatedViaDashboardTakesEffectPromptly(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	user := f.newDashboardUser("admin") // org-level; see alertsSeedProjectMember for why this alone is not enough
	alertsSeedProjectMember(t, f, user.ID, "admin")

	to := "alert-" + f.ProjectName + "@example.test"
	createRes := dashboardRequest(t, http.MethodPost, "/api/alerts", user, map[string]any{
		"projectId":          f.ProjectID,
		"channel":            "email",
		"channelTarget":      to,
		"frequencyThreshold": 1,
		"windowSeconds":      60,
		"enabled":            true,
	})
	if createRes.Status != http.StatusCreated {
		t.Fatalf("POST /api/alerts: status %d body %s", createRes.Status, createRes.Body)
	}
	var created struct {
		ID            string `json:"id"`
		ChannelTarget string `json:"channelTarget"`
	}
	createRes.JSON(t, &created)
	if created.ChannelTarget != to {
		t.Fatalf("dashboard echoed channelTarget=%q, want %q — config was not created as requested", created.ChannelTarget, to)
	}
	if created.ID != "" {
		t.Cleanup(func() {
			exec(context.Background(), `DELETE FROM alert_configs WHERE id = $1`, created.ID)
		})
	}

	since := time.Now()
	res := f.ingest(f.newEvent())
	if res.Status != http.StatusAccepted {
		t.Fatalf("ingest: status %d body %s", res.Status, res.Body)
	}

	deadlineAt := time.Now().Add(alertsFreshConfigBudget)
	missingToNeedle := fmt.Sprintf(`email channel_config missing "to" for project=%s`, f.ProjectID)
	invokedMarkers := []string{"Email attempt", "Email sent successfully", "Email failed after"}

	for {
		logs := alertsProcessorLogsSince(t, since)
		if strings.Contains(logs, missingToNeedle) {
			t.Fatalf("U27 FAILS: the dashboard-created config WAS picked up within %s (no 5-minute stale "+
				"cache here) — but the processor immediately dropped it: %q. Root cause: "+
				"apps/dashboard-web/src/routes/api/alerts/+server.ts:122 writes channel_config as "+
				"{\"target\": ...}, but apps/processor-go/alerts/notify.go:78 reads ChannelConfig[\"to\"]. "+
				"This is a field-name drift defect distinct from S8 (S8 was 'never constructed'; this is "+
				"'constructed, wired, reachable, and still undeliverable'); every alert config created "+
				"through the real API is permanently inert until the two sides agree on a key.",
				time.Since(since).Round(time.Second), missingToNeedle)
		}
		for _, m := range invokedMarkers {
			if strings.Contains(logs, m) {
				t.Logf("U27 PASSES: observed %q within %s of config creation — no 5-minute wait needed", m, time.Since(since).Round(time.Second))
				return
			}
		}
		if time.Now().After(deadlineAt) {
			t.Fatalf("U27 FAILS: no alert-related processor log line for project=%s appeared within %s of "+
				"creating the config via the real dashboard API. apps/processor-go/alerts/dispatcher.go's "+
				"Dispatcher only reloads alert_configs at process construction and on a hardcoded "+
				"5-minute ticker (loadConfigs, dispatcher.go:117) with no faster invalidation path for a "+
				"long-running processor — this config was created well after this shared processor "+
				"booted, so it is invisible to Dispatch until that ticker next fires. This is the literal "+
				"stale-cache gap U27 exists to catch.\n  log tail:\n%s", f.ProjectID,
				alertsFreshConfigBudget, alertsTail(logs, 40))
		}
		time.Sleep(2 * time.Second)
	}
}

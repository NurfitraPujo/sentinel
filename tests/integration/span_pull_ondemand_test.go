package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/spanpull"
)

// sentinel-occurrence-spans TASK-02: the dashboard (and the agent API) arm a span_pull_queue row for ONE immediate fetch:
// stage 1, on_demand = true, next_fetch_at = now. The worker must treat it like any other row (allow-list re-check, size
// bound, retries) and record the terminal outcome the UI reads (migration 1725800000). Real Postgres, like the rest of
// this file's family.

// armOnDemand inserts the row exactly as apps/dashboard-web/src/lib/server/span-pull.ts does for a new trace.
func (h *pullHarness) armOnDemand(t *testing.T, org string) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), `INSERT INTO span_pull_queue
		(org_id, trace_id, stage, next_fetch_at, on_demand, requested_by, requested_by_kind, requested_at)
		VALUES ($1::uuid, $2, 1, now() - interval '1 minute', true, 'user-1', 'user', now())`, org, pullTrace)
	require.NoError(t, err)
}

func (h *pullHarness) queueRow(t *testing.T, org string) (stage int, outcome string, onDemand bool) {
	t.Helper()
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT stage, outcome, on_demand FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, org, pullTrace).Scan(&stage, &outcome, &onDemand))
	return
}

func TestSpanPull_OnDemandRowIsFetchedOnceAndRecordsStored(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.stub.body.Store(otlpBody(time.Now().Add(-time.Minute), 3))
	h.armOnDemand(t, h.tenant.OrgID)

	n, err := h.worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.EqualValues(t, 1, h.stub.requests.Load())
	stage, outcome, onDemand := h.queueRow(t, h.tenant.OrgID)
	assert.Equal(t, 2, stage, "one successful fetch finishes a stage-1 row: no second fetch is scheduled")
	assert.Equal(t, spanpull.OutcomeStored, outcome)
	assert.True(t, onDemand)
	assert.Equal(t, 3, h.spanRows(t))

	*h.clock = h.clock.Add(time.Hour)
	_, err = h.worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, h.stub.requests.Load(), "finished rows are never claimed again")
}

func TestSpanPull_OnDemandNotFoundIsATerminalOutcome(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	h.stub.status.Store(404)
	h.stub.body.Store("trace not found")
	h.armOnDemand(t, h.tenant.OrgID)

	_, err := h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	stage, outcome, _ := h.queueRow(t, h.tenant.OrgID)
	assert.Equal(t, 2, stage)
	assert.Equal(t, spanpull.OutcomeNotFound, outcome, "the UI can tell 'the store has no such trace' from 'not pulled yet'")
	assert.Zero(t, h.spanRows(t))
}

func TestSpanPull_OnDemandTooLargeAndDelistedKeepTheirOutcome(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()

	h.stub.status.Store(500)
	h.stub.body.Store(tooLargeBody)
	h.armOnDemand(t, h.tenant.OrgID)
	_, err := h.worker.RunOnce(ctx)
	require.NoError(t, err)
	stage, outcome, _ := h.queueRow(t, h.tenant.OrgID)
	assert.Equal(t, 2, stage)
	assert.Equal(t, spanpull.OutcomeTooLarge, outcome)

	// The allow-list re-check is the backstop for an on-demand row too: another organization's row is dropped unfetched.
	before := h.stub.requests.Load()
	h.armOnDemand(t, h.other.OrgID)
	_, err = h.worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, h.stub.requests.Load(), "a delisted organization's on-demand row is never fetched")
	stage, outcome, _ = h.queueRow(t, h.other.OrgID)
	assert.Equal(t, 2, stage)
	assert.Equal(t, spanpull.OutcomeNotAllowed, outcome)
}

func TestSpanPull_ClaimLabelsOnDemandRows(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.armOnDemand(t, h.tenant.OrgID)
	_, err := h.pool.Exec(ctx, `INSERT INTO span_pull_queue (org_id, trace_id, next_fetch_at) VALUES ($1, $2, now() - interval '1 minute')`,
		h.tenant.OrgID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)

	jobs, err := h.queue.Claim(ctx, 10)
	require.NoError(t, err)
	got := map[string]string{}
	for _, j := range jobs {
		got[j.TraceID] = j.Trigger()
	}
	assert.Equal(t, map[string]string{pullTrace: spanpull.TriggerOnDemand, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": spanpull.TriggerAuto}, got)
	_, err = h.pool.Exec(ctx, `DELETE FROM span_pull_queue WHERE org_id = $1`, h.tenant.OrgID)
	require.NoError(t, err)
}

package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// httpIssueSnapshot is the subset of `GET /api/agent/issues/:id`'s `issue` object the runner's
// precondition re-check needs (plan §3, SENTINEL_AGENT_GUIDE.md §5.3): the same
// assigneeType/assignedTo/status fields the events feed embeds (§3), read fresh with one GET.
type httpIssueSnapshot struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	AssigneeType *string `json:"assigneeType"`
	AssignedTo   *string `json:"assignedTo"`
}

type httpIssueDetail struct {
	Issue httpIssueSnapshot `json:"issue"`
}

// HTTPIssueReader adapts a *sentinel.Client to the loop.IssueReader seam.
type HTTPIssueReader struct {
	Client *sentinel.Client
}

// GetIssue implements IssueReader via GET /api/agent/issues/:id, routed through
// sentinel.Client.GetIssue (not a second hand-rolled r.Client.Do call) so the wire shape this
// package actually sends is covered by client_test.go's goldens.
func (r HTTPIssueReader) GetIssue(ctx context.Context, issueID string) (IssueSnapshot, error) {
	res, err := r.Client.GetIssue(ctx, issueID)
	if err != nil {
		return IssueSnapshot{}, err
	}
	if res.Status < 200 || res.Status >= 300 {
		// Wrapped as *sentinel.StatusError (not a bare fmt.Errorf) so Runner.Run can classify the
		// failure via sentinel.ClassifyEnvelope -- specifically to tell a 404 (issue deleted between
		// the event and the job, C14 -- routine, not exceptional) apart from a transient 5xx, instead
		// of treating every precondition-read failure identically.
		return IssueSnapshot{}, &sentinel.StatusError{Status: res.Status, Header: res.Header, Body: res.Body}
	}
	var detail httpIssueDetail
	if err := json.Unmarshal(res.Body, &detail); err != nil {
		return IssueSnapshot{}, fmt.Errorf("parsing issue detail: %w", err)
	}
	snap := IssueSnapshot{ID: detail.Issue.ID, Status: detail.Issue.Status}
	if detail.Issue.AssigneeType != nil {
		snap.AssigneeType = *detail.Issue.AssigneeType
	}
	if detail.Issue.AssignedTo != nil {
		snap.AssignedTo = *detail.Issue.AssignedTo
	}
	return snap, nil
}

// HTTPClaimer adapts a *sentinel.Client to the loop.Claimer seam (plan §2.2/C1's "ensure-claimed").
type HTTPClaimer struct {
	Client *sentinel.Client
}

// EnsureClaimed implements Claimer via sentinel.Client.ClaimIssue (not a second hand-rolled
// c.Client.Do call), so this is the same POST /api/agent/issues/:id/claim wrapper client_test.go's
// goldens cover, and its 409 branch actually reaches this package's caller instead of being parsed
// only in unreachable code. 200 (fresh claim or idempotent self-reclaim) means held; 409 (claimed
// by someone else, per SENTINEL_AGENT_GUIDE.md §4 / C1) means not held -- that is not itself an
// error condition, so it is returned as (false, claimedBy, nil), claimedBy carrying the foreign
// claimant's agent id parsed from the 409 body when present.
func (c HTTPClaimer) EnsureClaimed(ctx context.Context, issueID string) (bool, string, error) {
	res, conflict, err := c.Client.ClaimIssue(ctx, issueID)
	if err != nil {
		return false, "", err
	}
	switch {
	case res.Status >= 200 && res.Status < 300:
		return true, "", nil
	case res.Status == http.StatusConflict:
		claimedBy := ""
		if conflict != nil {
			claimedBy = conflict.ClaimedBy
		}
		return false, claimedBy, nil
	default:
		return false, "", fmt.Errorf("POST /api/agent/issues/%s/claim: %d %s", issueID, res.Status, sentinel.ErrorMessage(res.Body))
	}
}

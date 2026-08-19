package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// fakeChat is a scripted llm.Chat for followup tests: it returns responses in order, ignoring
// ToolCalls handling (tests here never register tool calls to keep the fixture small — toolloop's
// own tool-loop behaviour is covered by llm package tests).
type fakeChat struct {
	responses []llm.Response
	calls     int
}

func (f *fakeChat) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	if f.calls >= len(f.responses) {
		return llm.Response{}, context.DeadlineExceeded
	}
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

type fakeResolver struct {
	ctx FollowupIssueContext
	err error
}

func (f fakeResolver) ResolveFollowupContext(_ context.Context, _ string) (FollowupIssueContext, error) {
	return f.ctx, f.err
}

func newFollowupTestServer(t *testing.T, issueBody, commentsBody string, issueStatus, commentsStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/issues/issue-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(issueStatus)
		w.Write([]byte(issueBody))
	})
	mux.HandleFunc("/api/agent/issues/issue-1/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(commentsStatus)
		w.Write([]byte(commentsBody))
	})
	mux.HandleFunc("/api/agent/issues/issue-1/occurrences", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"occurrences":[]}`))
	})
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"issues":[]}`))
	})
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"projects":[]}`))
	})
	return httptest.NewServer(mux)
}

func TestFollowupAdvisor_Decide_ProducesValidDecision(t *testing.T) {
	srv := newFollowupTestServer(t,
		`{"issue":{"message":"boom","errorClass":"NPE","status":"unresolved","issueType":"user_report","projectId":"proj-1"}}`,
		`{"comments":[{"authorType":"user","bodyMd":"still broken"}]}`,
		200, 200,
	)
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	decisionJSON := `{"action":"reply","body":"Thanks, looking into it."}`
	chat := &fakeChat{responses: []llm.Response{
		{Text: decisionJSON, StopReason: llm.StopEndTurn},
	}}

	adv := &FollowupAdvisor{
		Client:   client,
		Resolver: fakeResolver{ctx: FollowupIssueContext{ProjectID: "proj-1", IssueType: "user_report"}},
		Primary:  chat,
	}

	dec, err := adv.Decide(context.Background(), Input{JobID: "job-1", IssueID: "issue-1", Kind: "followup", TriggerSeq: 1})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Kind != "followup" {
		t.Errorf("Kind = %q, want followup", dec.Kind)
	}
	var fd FollowupDecision
	if err := json.Unmarshal(dec.Raw, &fd); err != nil {
		t.Fatalf("decoding raw decision: %v", err)
	}
	if fd.Action != "reply" || fd.Body != "Thanks, looking into it." {
		t.Errorf("decoded decision = %+v, want action=reply body set", fd)
	}
}

func TestFollowupAdvisor_Decide_SchemaInvalidIsPermanentAfterReasks(t *testing.T) {
	srv := newFollowupTestServer(t,
		`{"issue":{"message":"boom","status":"unresolved","issueType":"user_report","projectId":"proj-1"}}`,
		`{"comments":[]}`,
		200, 200,
	)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	// action "bogus" is not in the enum -- every attempt (initial + 2 re-asks) fails validation.
	bad := llm.Response{Text: `{"action":"bogus","body":"x"}`, StopReason: llm.StopEndTurn}
	chat := &fakeChat{responses: []llm.Response{bad, bad, bad}}

	adv := &FollowupAdvisor{
		Client:   client,
		Resolver: fakeResolver{ctx: FollowupIssueContext{ProjectID: "proj-1"}},
		Primary:  chat,
	}
	_, err := adv.Decide(context.Background(), Input{JobID: "job-1", IssueID: "issue-1", Kind: "followup", TriggerSeq: 1})
	if err == nil {
		t.Fatalf("expected error for schema-invalid decision")
	}
	var perr *llm.PermanentError
	if !errorsAsPermanent(err, &perr) {
		t.Errorf("error = %v, want *llm.PermanentError", err)
	}
}

func TestFollowupAdvisor_Decide_IssueFetchFailurePropagates(t *testing.T) {
	srv := newFollowupTestServer(t, `{"error":"nope"}`, `{"comments":[]}`, 500, 200)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	adv := &FollowupAdvisor{
		Client:   client,
		Resolver: fakeResolver{ctx: FollowupIssueContext{ProjectID: "proj-1"}},
		Primary:  &fakeChat{},
	}
	_, err := adv.Decide(context.Background(), Input{JobID: "job-1", IssueID: "issue-1", Kind: "followup", TriggerSeq: 1})
	if err == nil || !strings.Contains(err.Error(), "fetching issue") {
		t.Fatalf("err = %v, want fetching-issue error", err)
	}
}

// errorsAsPermanent is a tiny local errors.As wrapper so the test file doesn't need to import
// "errors" just for this one assertion given the package already imports several others.
func errorsAsPermanent(err error, target **llm.PermanentError) bool {
	type asTarget interface{ As(interface{}) bool }
	for err != nil {
		if pe, ok := err.(*llm.PermanentError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

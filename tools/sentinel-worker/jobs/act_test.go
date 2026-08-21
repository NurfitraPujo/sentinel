package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }

func baseTriageCtx() ActContext {
	return ActContext{JobID: "job-1", IssueID: "issue-1", IssueType: "user_report"}
}

// --- CompileTriage golden op ordering + idempotency keys ---------------------------------------

func TestCompileTriage_CommentOnly_OrderAndKeys(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionCommentOnly), Summary: "looks benign", Severity: strp("low")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	if compiled.Question != nil {
		t.Fatalf("comment_only must not produce a question")
	}
	wantOps := []string{"issues.comment", "issues.report.severity", "issues.claim.release"}
	if len(compiled.Ops) != len(wantOps) {
		t.Fatalf("op count = %d, want %d: %+v", len(compiled.Ops), len(wantOps), compiled.Ops)
	}
	for i, op := range compiled.Ops {
		if op.Op != wantOps[i] {
			t.Errorf("op[%d] = %q, want %q", i, op.Op, wantOps[i])
		}
	}
	// Golden idempotency keys: <jobId>:<opIndex>, 0-based, stable per position.
	wantKeys := []string{"job-1:0", "job-1:1", "job-1:2"}
	for i, op := range compiled.Ops {
		params, _ := op.Params.(map[string]interface{})
		if params == nil {
			t.Fatalf("op[%d] params not a map: %v", i, op.Params)
		}
		if params["idempotency_key"] != wantKeys[i] {
			t.Errorf("op[%d] idempotency_key = %v, want %v", i, params["idempotency_key"], wantKeys[i])
		}
	}
	if compiled.EnqueueFix {
		t.Error("comment_only must never enqueue a FIX job")
	}
}

func TestCompileTriage_NeedsInfo_KeepsClaimAndQuestionPrecedesBatch(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionNeedsInfo), Summary: "need more info", Question: strp("what version?")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	if compiled.Question == nil {
		t.Fatal("needs_info must produce a question")
	}
	if compiled.Question.Body != "what version?" {
		t.Errorf("question body = %q", compiled.Question.Body)
	}
	if compiled.Question.IdempotencyKey != "job-1:0" {
		t.Errorf("question idempotency key = %q, want job-1:0", compiled.Question.IdempotencyKey)
	}
	// The comment's key must be the NEXT index after the question's, proving the question is
	// numbered (and thus, per the shared documented contract, ordered) before the batch.
	if len(compiled.Ops) != 1 || compiled.Ops[0].Op != "issues.comment" {
		t.Fatalf("expected exactly one issues.comment op (claim kept, no release), got %+v", compiled.Ops)
	}
	params := compiled.Ops[0].Params.(map[string]interface{})
	if params["idempotency_key"] != "job-1:1" {
		t.Errorf("comment idempotency key = %v, want job-1:1", params["idempotency_key"])
	}
	// needs_info KEEPS the claim: no claim.release op anywhere.
	for _, op := range compiled.Ops {
		if op.Op == "issues.claim.release" {
			t.Fatal("needs_info must NOT release the claim (plan §4.2)")
		}
	}
}

func TestCompileTriage_NeedsHuman_SeverityCriticalAndRelease(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionNeedsHuman), Summary: "over my head"}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	wantOps := []string{"issues.comment", "issues.report.severity", "issues.claim.release"}
	if len(compiled.Ops) != len(wantOps) {
		t.Fatalf("op count = %d, want %d: %+v", len(compiled.Ops), len(wantOps), compiled.Ops)
	}
	for i, op := range compiled.Ops {
		if op.Op != wantOps[i] {
			t.Errorf("op[%d] = %q, want %q", i, op.Op, wantOps[i])
		}
	}
	sevParams := compiled.Ops[1].Params.(map[string]interface{})
	if sevParams["severity"] != "critical" {
		t.Errorf("needs_human severity = %v, want critical", sevParams["severity"])
	}
	commentParams := compiled.Ops[0].Params.(map[string]interface{})
	body, _ := commentParams["body_md"].(string)
	if !strings.HasPrefix(body, "🤖 Escalation: ") {
		t.Errorf("needs_human comment must use the Escalation prefix, got %q", body)
	}
}

func TestCompileTriage_Duplicate_RelationAndRelease(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionDuplicate), Summary: "dup", DuplicateOf: strp("issue-2")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	wantOps := []string{"issues.comment", "issues.relations.add", "issues.claim.release"}
	for i, op := range compiled.Ops {
		if op.Op != wantOps[i] {
			t.Errorf("op[%d] = %q, want %q", i, op.Op, wantOps[i])
		}
	}
	relParams := compiled.Ops[1].Params.(map[string]interface{})
	if relParams["target_issue_id"] != "issue-2" || relParams["relation_type"] != "duplicate_of" {
		t.Errorf("relation params = %+v", relParams)
	}
}

func TestCompileTriage_Fixable_AboveThreshold_KeepsClaimAndEnqueues(t *testing.T) {
	actx := baseTriageCtx()
	actx.FixEnabled = true
	d := TriageDecision{Disposition: string(DispositionFixable), Summary: "can fix", FixBrief: strp("do the thing"), Confidence: 0.9}
	compiled, err := CompileTriage(actx, d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	if !compiled.EnqueueFix {
		t.Error("expected EnqueueFix true above threshold with FIX enabled")
	}
	for _, op := range compiled.Ops {
		if op.Op == "issues.claim.release" {
			t.Fatal("fixable above threshold must keep the claim")
		}
	}
}

func TestCompileTriage_Fixable_BelowThreshold_ReleasesAndDoesNotEnqueue(t *testing.T) {
	actx := baseTriageCtx()
	actx.FixEnabled = true
	d := TriageDecision{Disposition: string(DispositionFixable), Summary: "maybe fix", FixBrief: strp("do the thing"), Confidence: 0.1}
	compiled, err := CompileTriage(actx, d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	if compiled.EnqueueFix {
		t.Error("below-threshold fixable must not enqueue FIX")
	}
	found := false
	for _, op := range compiled.Ops {
		if op.Op == "issues.claim.release" {
			found = true
		}
	}
	if !found {
		t.Error("below-threshold fixable must release the claim (comment only + release)")
	}
}

// --- C8: severity is user_report only ------------------------------------------------------------

func TestCompileTriage_C8_SeverityOmittedForNonUserReport(t *testing.T) {
	actx := baseTriageCtx()
	actx.IssueType = "system_error"
	d := TriageDecision{Disposition: string(DispositionCommentOnly), Summary: "sys err", Severity: strp("high")}
	compiled, err := CompileTriage(actx, d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	for _, op := range compiled.Ops {
		if op.Op == "issues.report.severity" {
			t.Fatal("severity op must never be compiled for a non-user_report issue (C8)")
		}
	}
}

func TestCompileTriage_C8_NeedsHumanSeverityOmittedForNonUserReport(t *testing.T) {
	actx := baseTriageCtx()
	actx.IssueType = "system_error"
	d := TriageDecision{Disposition: string(DispositionNeedsHuman), Summary: "sys err escalate"}
	compiled, err := CompileTriage(actx, d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	for _, op := range compiled.Ops {
		if op.Op == "issues.report.severity" {
			t.Fatal("needs_human must not force severity on a non-user_report issue (C8)")
		}
	}
}

// --- C7: never in_progress; only unresolved|resolved|ignored ------------------------------------

func TestCompileFollowup_C7_ResolveNeverEmitsInProgress(t *testing.T) {
	d := FollowupDecision{Action: string(ActionResolve), Body: "fixed in 1.2.3", ResolvedInVersion: strp("1.2.3")}
	compiled, err := CompileFollowup(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	var statusOp *sentinel.BatchOperation
	for i := range compiled.Ops {
		if compiled.Ops[i].Op == "issues.status" {
			statusOp = &compiled.Ops[i]
		}
	}
	if statusOp == nil {
		t.Fatal("resolve action must compile an issues.status op")
	}
	params := statusOp.Params.(map[string]interface{})
	if params["status"] != StatusResolved {
		t.Errorf("status = %v, want %v", params["status"], StatusResolved)
	}
	if params["status"] == "in_progress" {
		t.Fatal("C7 violated: in_progress must never be compiled")
	}
}

// --- FOLLOW-UP compile shape ----------------------------------------------------------------------

func TestCompileFollowup_Reply_KeepsClaim(t *testing.T) {
	d := FollowupDecision{Action: string(ActionReply), Body: "here's an update"}
	compiled, err := CompileFollowup(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	for _, op := range compiled.Ops {
		if op.Op == "issues.claim.release" {
			t.Fatal("reply must keep the claim")
		}
	}
	if len(compiled.Ops) != 1 || compiled.Ops[0].Op != "issues.comment" {
		t.Fatalf("expected a single issues.comment op, got %+v", compiled.Ops)
	}
}

func TestCompileFollowup_Release_ReleasesClaim(t *testing.T) {
	d := FollowupDecision{Action: string(ActionRelease), Body: "handing back"}
	compiled, err := CompileFollowup(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	last := compiled.Ops[len(compiled.Ops)-1]
	if last.Op != "issues.claim.release" {
		t.Fatalf("release action must end with claim.release, got %+v", compiled.Ops)
	}
}

func TestCompileFollowup_AttemptFix_AboveThreshold_KeepsClaimAndEnqueues(t *testing.T) {
	actx := baseTriageCtx()
	actx.FixEnabled = true
	d := FollowupDecision{Action: string(ActionAttemptFix), Body: "trying a fix", FixBrief: strp("brief"), Confidence: f64p(0.9)}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	if !compiled.EnqueueFix {
		t.Error("expected EnqueueFix true")
	}
	for _, op := range compiled.Ops {
		if op.Op == "issues.claim.release" {
			t.Fatal("attempt_fix above threshold must keep the claim")
		}
	}
}

// TestCompileFollowup_AttemptFix_BelowThreshold_ReleasesClaim is the "advisor-toolchain-act" fix
// for the major finding: a below-threshold (or FIX-disabled) attempt_fix must release the claim,
// matching CompileTriage's below-threshold `fixable` branch — otherwise the claim strands forever
// (no question means waiting_on is unset, so the §4.3 nag sweep never scans it, and only the §2.7
// heartbeat keeps it alive indefinitely).
func TestCompileFollowup_AttemptFix_BelowThreshold_ReleasesClaim(t *testing.T) {
	actx := baseTriageCtx()
	actx.FixEnabled = true
	d := FollowupDecision{Action: string(ActionAttemptFix), Body: "trying a fix", FixBrief: strp("brief"), Confidence: f64p(0.1)}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	if compiled.EnqueueFix {
		t.Error("expected EnqueueFix false below the confidence threshold")
	}
	last := compiled.Ops[len(compiled.Ops)-1]
	if last.Op != "issues.claim.release" {
		t.Fatalf("below-threshold attempt_fix must release the claim, got %+v", compiled.Ops)
	}
}

// TestCompileFollowup_AttemptFix_FixDisabled_ReleasesClaim covers the same finding when FIX is
// disabled entirely rather than confidence being low.
func TestCompileFollowup_AttemptFix_FixDisabled_ReleasesClaim(t *testing.T) {
	actx := baseTriageCtx()
	actx.FixEnabled = false
	d := FollowupDecision{Action: string(ActionAttemptFix), Body: "trying a fix", FixBrief: strp("brief"), Confidence: f64p(0.9)}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	if compiled.EnqueueFix {
		t.Error("expected EnqueueFix false when FIX is disabled")
	}
	last := compiled.Ops[len(compiled.Ops)-1]
	if last.Op != "issues.claim.release" {
		t.Fatalf("FIX-disabled attempt_fix must release the claim, got %+v", compiled.Ops)
	}
}

// --- guard.Check gate: mutation target -----------------------------------------------------------

// TestCompileTriage_GuardGate_BlocksExfiltrationSummary is the §8 mutation target: "remove the
// Check call → red". A summary that is >25% verbatim copy of a tool output must be rejected before
// any op is compiled.
func TestCompileTriage_GuardGate_BlocksExfiltrationSummary(t *testing.T) {
	secretDump := strings.Repeat("SENTINEL_AGENT_KEY=abcdef0123456789 leaked-contents-of-agent-key-json ", 20)
	actx := baseTriageCtx()
	actx.ToolOutputs = []string{secretDump}

	d := TriageDecision{Disposition: string(DispositionCommentOnly), Summary: secretDump}
	_, err := CompileTriage(actx, d)
	if err == nil {
		t.Fatal("expected the guard gate to reject a near-verbatim tool-output dump as the summary")
	}
	var gerr *GateRejectedError
	if !errors.As(err, &gerr) {
		t.Fatalf("expected *GateRejectedError, got %T: %v", err, err)
	}
	if gerr.Field != guard.FieldSummary {
		t.Errorf("Field = %v, want FieldSummary", gerr.Field)
	}
}

func TestCompileTriage_GuardGate_BlocksSecretValue(t *testing.T) {
	actx := baseTriageCtx()
	actx.Secrets = []string{"ghp_supersecrettoken1234567890"}
	d := TriageDecision{Disposition: string(DispositionCommentOnly), Summary: "the token is ghp_supersecrettoken1234567890 apparently"}
	_, err := CompileTriage(actx, d)
	var gerr *GateRejectedError
	if !errors.As(err, &gerr) {
		t.Fatalf("expected *GateRejectedError for a leaked secret, got %v", err)
	}
}

// TestCompileTriage_MaxVerbatim_ThreadsFromActContext is the real-path proof for plan §5/§4.6
// finding 3: WORKER_GATE_MAX_VERBATIM must actually change the gate threshold gate() enforces, via
// ActContext.MaxVerbatim -> guard.CheckWithConfig -- not the hardcoded guard.DefaultMaxVerbatim
// act.go:136 (pre-fix) always used regardless of config. The candidate summary is engineered to
// have ~40% verbatim coverage of the tool corpus: rejected at the DEFAULT threshold (0.25), but
// allowed once ActContext.MaxVerbatim is raised to 0.5 -- the same Decision, the same tool corpus,
// only the configured cap differs.
func TestCompileTriage_MaxVerbatim_ThreadsFromActContext(t *testing.T) {
	verbatimBlock := strings.Repeat("X", 40) // one 40-byte run, well above guard's 8-byte k-gram floor
	toolOutput := "some other unrelated tool output content around it " + verbatimBlock + " and more unrelated content"
	filler := "the quick brown fox jumps over the lazy dog 0123456789 zyx" // 60 bytes, no 8-byte overlap with the X run
	summary := verbatimBlock + filler                                      // ~40/100 bytes verbatim => ~40% coverage

	actx := baseTriageCtx()
	actx.ToolOutputs = []string{toolOutput}
	d := TriageDecision{Disposition: string(DispositionCommentOnly), Summary: summary}

	// Default threshold (ActContext.MaxVerbatim left zero-valued -> guard.DefaultMaxVerbatim=0.25):
	// ~40% coverage must be rejected.
	if _, err := CompileTriage(actx, d); err == nil {
		t.Fatalf("expected the default 0.25 verbatim cap to reject a ~40%%-verbatim summary")
	} else {
		var gerr *GateRejectedError
		if !errors.As(err, &gerr) || gerr.Field != guard.FieldSummary {
			t.Fatalf("expected a *GateRejectedError for FieldSummary, got %v", err)
		}
	}

	// Raising WORKER_GATE_MAX_VERBATIM to 0.5 via ActContext.MaxVerbatim must let the SAME
	// candidate through -- proving the value actually reaches guard.CheckWithConfig's threshold,
	// not just that Check runs at all.
	actx.MaxVerbatim = 0.5
	if _, err := CompileTriage(actx, d); err != nil {
		t.Fatalf("expected MaxVerbatim=0.5 to allow a ~40%% verbatim summary, got %v", err)
	}
}

func TestCompileFollowup_GuardGate_AppliesToReplyBody(t *testing.T) {
	actx := baseTriageCtx()
	actx.Secrets = []string{"ghp_supersecrettoken1234567890"}
	d := FollowupDecision{Action: string(ActionReply), Body: "the token is ghp_supersecrettoken1234567890"}
	_, err := CompileFollowup(actx, d)
	var gerr *GateRejectedError
	if !errors.As(err, &gerr) {
		t.Fatalf("expected *GateRejectedError, got %v", err)
	}
	if gerr.Field != guard.FieldReplyBody {
		t.Errorf("Field = %v, want FieldReplyBody", gerr.Field)
	}
}

// --- resolved_in_version publish gate (finding 6, core-robustness round 3) ---------------------

// TestCompileFollowup_ResolvedInVersion_RejectsSecretLeak proves resolved_in_version now goes
// through the SAME secret-leak gate every other published field gets: a configured secret value
// riding along inside resolvedInVersion must be rejected, not published verbatim. Red-first:
// before this fix, ResolvedInVersion was copied straight into the issues.status params with no
// gate call at all, so this would have compiled successfully and leaked the secret.
func TestCompileFollowup_ResolvedInVersion_RejectsSecretLeak(t *testing.T) {
	actx := baseTriageCtx()
	actx.Secrets = []string{"ghp_supersecrettoken1234567890"}
	d := FollowupDecision{
		Action:            string(ActionResolve),
		Body:              "fixed",
		ResolvedInVersion: strp("ghp_supersecrettoken1234567890"),
	}
	_, err := CompileFollowup(actx, d)
	var gerr *GateRejectedError
	if !errors.As(err, &gerr) {
		t.Fatalf("expected *GateRejectedError for a secret-carrying resolvedInVersion, got %v", err)
	}
}

// TestCompileFollowup_ResolvedInVersion_StripsInjectionCharset proves an injection-y
// resolvedInVersion value (markdown/HTML/control-character payload, the kind an
// attacker-influenced Advisor decision could carry) is charset-restricted to a version-shaped
// string before publish, rather than passed through byte-for-byte.
func TestCompileFollowup_ResolvedInVersion_StripsInjectionCharset(t *testing.T) {
	actx := baseTriageCtx()
	d := FollowupDecision{
		Action:            string(ActionResolve),
		Body:              "fixed",
		ResolvedInVersion: strp("1.2.3<script>alert(1)</script>[malicious](http://evil.example)"),
	}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	var statusOp *sentinel.BatchOperation
	for i := range compiled.Ops {
		if compiled.Ops[i].Op == "issues.status" {
			statusOp = &compiled.Ops[i]
		}
	}
	if statusOp == nil {
		t.Fatal("expected an issues.status op")
	}
	params := statusOp.Params.(map[string]interface{})
	got, _ := params["resolved_in_version"].(string)
	// Only the markdown/HTML-significant DELIMITER characters need to be gone -- the charset
	// allowlist is conservative about punctuation, not about which words survive (matching
	// fix_pr.go's titleCharsetPattern posture for the analogous errorClass sanitizer).
	for _, bad := range []string{"<", ">", "[", "]", "(", ")"} {
		if strings.Contains(got, bad) {
			t.Fatalf("resolved_in_version = %q, must not contain %q after sanitization", got, bad)
		}
	}
	if !strings.HasPrefix(got, "1.2.3") {
		t.Fatalf("resolved_in_version = %q, expected the legitimate version-shaped prefix 1.2.3 to survive", got)
	}
}

// TestCompileFollowup_ResolvedInVersion_LengthCapped proves a pathologically long
// resolvedInVersion is capped rather than published as a multi-KB blob.
func TestCompileFollowup_ResolvedInVersion_LengthCapped(t *testing.T) {
	actx := baseTriageCtx()
	long := strings.Repeat("9.", 2000) + "0"
	d := FollowupDecision{Action: string(ActionResolve), Body: "fixed", ResolvedInVersion: strp(long)}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	var statusOp *sentinel.BatchOperation
	for i := range compiled.Ops {
		if compiled.Ops[i].Op == "issues.status" {
			statusOp = &compiled.Ops[i]
		}
	}
	params := statusOp.Params.(map[string]interface{})
	got, _ := params["resolved_in_version"].(string)
	if len(got) > 64 {
		t.Fatalf("resolved_in_version length = %d, want <= 64", len(got))
	}
}

// TestCompileFollowup_ResolvedInVersion_MutationTest deletes the sanitization (by asserting a
// value with characters resolvedInVersionForPublish is documented to strip would have survived
// verbatim under the pre-fix code) -- pins that "v1.0.0 " (with a trailing space, and a leading
// "v" -- both legitimate version-string characters that must survive) round-trips through
// unchanged aside from charset/whitespace normalization, so a future regression that starts
// stripping legitimate version characters (over-eager sanitization) is caught too, not just an
// under-sanitizing regression.
func TestCompileFollowup_ResolvedInVersion_MutationTest(t *testing.T) {
	actx := baseTriageCtx()
	d := FollowupDecision{Action: string(ActionResolve), Body: "fixed", ResolvedInVersion: strp("v1.0.0-rc.1+build.5")}
	compiled, err := CompileFollowup(actx, d)
	if err != nil {
		t.Fatalf("CompileFollowup: %v", err)
	}
	var statusOp *sentinel.BatchOperation
	for i := range compiled.Ops {
		if compiled.Ops[i].Op == "issues.status" {
			statusOp = &compiled.Ops[i]
		}
	}
	params := statusOp.Params.(map[string]interface{})
	if got := params["resolved_in_version"]; got != "v1.0.0-rc.1+build.5" {
		t.Fatalf("resolved_in_version = %v, want unchanged legitimate version string %q", got, "v1.0.0-rc.1+build.5")
	}
}

// --- dry-run: zero POSTs ---------------------------------------------------------------------

type recordingSender struct {
	calls    []string
	requests []sentinel.BatchRequest
}

func (s *recordingSender) PostQuestion(_ context.Context, issueID string, _ map[string]interface{}, key string) (*sentinel.Result, error) {
	s.calls = append(s.calls, "question:"+issueID+":"+key)
	return &sentinel.Result{Status: 201}, nil
}

func (s *recordingSender) PostBatch(_ context.Context, req sentinel.BatchRequest) (*sentinel.Result, error) {
	s.calls = append(s.calls, "batch")
	s.requests = append(s.requests, req)
	// Body carries one {"status":200} results[] entry per op so checkBatchResults' per-op walk
	// (finding 1, jobs/fix_pr.go) sees every op as a success by default, matching this fake's own
	// implied "everything landed" semantics -- a bare Status:200 with no Body previously looked
	// identical to an unparsable 2xx body (ClassPermanent, perOp==nil) once callers started
	// actually inspecting results[].
	results := make([]string, len(req.Operations))
	for i := range results {
		results[i] = `{"status":200}`
	}
	body := []byte(`{"completed":` + fmt.Sprint(len(req.Operations)) + `,"results":[` + strings.Join(results, ",") + `]}`)
	return &sentinel.Result{Status: 200, Body: body}, nil
}

func TestAct_DryRun_SendsNothing(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionNeedsInfo), Summary: "need info", Question: strp("what?")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	sender := &recordingSender{}
	outcome, qRes, bRes, err := Act(context.Background(), sender, compiled, false)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.Sent {
		t.Fatal("dry-run must report Sent=false")
	}
	if qRes != nil || bRes != nil {
		t.Fatal("dry-run must not return any wire results")
	}
	if len(sender.calls) != 0 {
		t.Fatalf("dry-run must issue zero POSTs, got %v", sender.calls)
	}
	// The compiled payload itself must still be present for the caller to journal verbatim.
	if outcome.Compiled.Question == nil || len(outcome.Compiled.Ops) == 0 {
		t.Fatal("dry-run outcome must still carry the full compiled payload to journal")
	}
}

func TestAct_Execute_SendsQuestionThenBatch(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionNeedsInfo), Summary: "need info", Question: strp("what?")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	sender := &recordingSender{}
	outcome, qRes, bRes, err := Act(context.Background(), sender, compiled, true)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if !outcome.Sent {
		t.Fatal("execute=true must report Sent=true")
	}
	if qRes == nil || bRes == nil {
		t.Fatal("expected both a question and a batch result")
	}
	if len(sender.calls) != 2 || sender.calls[0] != "question:issue-1:job-1:0" || sender.calls[1] != "batch" {
		t.Fatalf("expected question call THEN batch call, got %v", sender.calls)
	}
}

// failingQuestionStatusSender is recordingSender's PostQuestion, but returning a non-2xx Status
// with a nil error -- the exact "transport succeeded, application failed" shape PostQuestion's
// real HTTP implementation produces (sentinel.Client.Do never turns a non-2xx response into a Go
// error itself; it just carries the status in Result.Status).
type failingQuestionStatusSender struct {
	recordingSender
	questionStatus int
}

func (s *failingQuestionStatusSender) PostQuestion(_ context.Context, issueID string, _ map[string]interface{}, key string) (*sentinel.Result, error) {
	s.calls = append(s.calls, "question:"+issueID+":"+key)
	return &sentinel.Result{Status: s.questionStatus, Body: []byte(`{"error":"nope"}`)}, nil
}

// TestAct_Execute_QuestionNon2xx_SurfacesErrorAndSkipsBatch is finding 3's red-first proof: Act
// previously checked only PostQuestion's transport err, never qRes.Status, so a 500 (or 400/403/
// 409) question response was treated as a successful ask -- the caller (RealActor.Act) would then
// journal StateQuestioned with an empty commentId and never send the batch that follows.
// Post-fix, a non-2xx question response must (a) surface a non-nil error from Act, (b) classify via
// sentinel.ClassifyEnvelope as the expected class, and (c) never reach PostBatch.
func TestAct_Execute_QuestionNon2xx_SurfacesErrorAndSkipsBatch(t *testing.T) {
	d := TriageDecision{Disposition: string(DispositionNeedsInfo), Summary: "need info", Question: strp("what?")}
	compiled, err := CompileTriage(baseTriageCtx(), d)
	if err != nil {
		t.Fatalf("CompileTriage: %v", err)
	}
	sender := &failingQuestionStatusSender{questionStatus: 500}
	_, qRes, bRes, err := Act(context.Background(), sender, compiled, true)
	if err == nil {
		t.Fatal("Act: got nil error for a 500 question response, want an error (finding 3)")
	}
	var statusErr *sentinel.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap a *sentinel.StatusError", err)
	}
	if statusErr.Status != 500 {
		t.Errorf("StatusError.Status = %d, want 500", statusErr.Status)
	}
	if class := sentinel.ClassifyEnvelope(statusErr.Status, false, false); class != sentinel.ClassTransient {
		t.Errorf("ClassifyEnvelope(500) = %v, want ClassTransient", class)
	}
	if bRes != nil {
		t.Fatal("finding 3: the batch must never be sent after a failed question POST")
	}
	if len(sender.calls) != 1 {
		t.Fatalf("expected exactly the question call (no batch), got %v", sender.calls)
	}
	_ = qRes // qRes is returned for the caller's own inspection/logging; not asserted further here.
}

// --- DecodeTriage/DecodeFollowup round-trip ------------------------------------------------------

func TestDecodeTriage_RoundTrip(t *testing.T) {
	want := TriageDecision{
		Severity:    strp("high"),
		Disposition: string(DispositionDuplicate),
		DuplicateOf: strp("issue-9"),
		CausedBy:    nil,
		Summary:     "looks like a dup",
		Question:    nil,
		FixBrief:    nil,
		Confidence:  0.42,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeTriage(Decision{Kind: "triage", Raw: raw})
	if err != nil {
		t.Fatalf("DecodeTriage: %v", err)
	}
	if got.Disposition != want.Disposition || got.Summary != want.Summary || got.Confidence != want.Confidence {
		t.Errorf("DecodeTriage round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
	if got.Severity == nil || *got.Severity != *want.Severity {
		t.Errorf("Severity round-trip mismatch: got %v, want %v", got.Severity, want.Severity)
	}
	if got.DuplicateOf == nil || *got.DuplicateOf != *want.DuplicateOf {
		t.Errorf("DuplicateOf round-trip mismatch: got %v, want %v", got.DuplicateOf, want.DuplicateOf)
	}
	if got.CausedBy != nil || got.Question != nil || got.FixBrief != nil {
		t.Errorf("expected nil-carrying fields to stay nil: %+v", got)
	}
}

func TestDecodeTriage_InvalidJSON(t *testing.T) {
	_, err := DecodeTriage(Decision{Kind: "triage", Raw: json.RawMessage(`not json`)})
	if err == nil {
		t.Fatal("DecodeTriage with invalid JSON: want an error, got nil")
	}
}

func TestDecodeFollowup_RoundTrip(t *testing.T) {
	want := FollowupDecision{
		Action:            string(ActionAttemptFix),
		Body:              "trying a fix",
		ResolvedInVersion: nil,
		FixBrief:          strp("patch the null check"),
		Confidence:        f64p(0.9),
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeFollowup(Decision{Kind: "followup", Raw: raw})
	if err != nil {
		t.Fatalf("DecodeFollowup: %v", err)
	}
	if got.Action != want.Action || got.Body != want.Body || *got.FixBrief != *want.FixBrief || *got.Confidence != *want.Confidence {
		t.Errorf("DecodeFollowup round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestDecodeFollowup_InvalidJSON(t *testing.T) {
	_, err := DecodeFollowup(Decision{Kind: "followup", Raw: json.RawMessage(`not json`)})
	if err == nil {
		t.Fatal("DecodeFollowup with invalid JSON: want an error, got nil")
	}
}

// --- unknown disposition/action -> typed error at the compiler boundary --------------------------

func TestCompileTriage_UnknownDisposition_TypedError(t *testing.T) {
	d := TriageDecision{Disposition: "sabotage", Summary: "whatever"}
	_, err := CompileTriage(baseTriageCtx(), d)
	if err == nil {
		t.Fatal("expected an error for an unknown disposition")
	}
	var uerr *UnknownDecisionValueError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *UnknownDecisionValueError, got %T: %v", err, err)
	}
	if uerr.Kind != "disposition" || uerr.Value != "sabotage" {
		t.Errorf("unexpected error fields: %+v", uerr)
	}
}

func TestCompileFollowup_UnknownAction_TypedError(t *testing.T) {
	d := FollowupDecision{Action: "sabotage", Body: "whatever"}
	_, err := CompileFollowup(baseTriageCtx(), d)
	if err == nil {
		t.Fatal("expected an error for an unknown action")
	}
	var uerr *UnknownDecisionValueError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *UnknownDecisionValueError, got %T: %v", err, err)
	}
	if uerr.Kind != "action" || uerr.Value != "sabotage" {
		t.Errorf("unexpected error fields: %+v", uerr)
	}
}

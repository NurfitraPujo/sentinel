package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/repoctx"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *sentinel.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return sentinel.NewClient(srv.URL, "test-key")
}

func TestBuildToolchain_WithoutRepoMapping(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	rec := NewToolOutputRecorder()
	tc, err := BuildToolchain(client, nil, "issue-1", "project-1", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	wantNames := map[string]bool{
		ToolGetIssue: true, ToolGetOccurrences: true, ToolListSimilar: true, ToolGetProjects: true,
	}
	if len(tc.Funcs) != len(wantNames) {
		t.Fatalf("without a repo mapping, expected exactly %d tools, got %d: %v", len(wantNames), len(tc.Funcs), tc.Funcs)
	}
	for name := range wantNames {
		if _, ok := tc.Funcs[name]; !ok {
			t.Errorf("missing expected tool %q", name)
		}
	}
	if _, ok := tc.Funcs[repoctx.ToolNameReadFile]; ok {
		t.Errorf("read_file must not be registered without a repo mapping")
	}
	if _, ok := tc.Funcs[repoctx.ToolNameSearchCode]; ok {
		t.Errorf("search_code must not be registered without a repo mapping")
	}
}

// TestBuildToolchain_GetOccurrences_HitsPaginatedEndpoint proves get_occurrences calls the real
// paginated occurrence endpoint (GET /api/agent/issues/:id/occurrences?limit=&before=) and passes
// the model's before/limit arguments through as query params — not GetIssue's single
// latestOccurrence, and not an inert echoed page argument.
func TestBuildToolchain_GetOccurrences_HitsPaginatedEndpoint(t *testing.T) {
	var gotPath, gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"occurrences":[]}`))
	})
	rec := NewToolOutputRecorder()
	tc, err := BuildToolchain(client, nil, "issue-1", "project-1", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	fn := tc.Funcs[ToolGetOccurrences]
	if fn == nil {
		t.Fatalf("get_occurrences not registered")
	}
	out, err := fn(context.Background(), `{"before":"2026-08-01T00:00:00Z","limit":5}`)
	if err != nil {
		t.Fatalf("get_occurrences returned error: %v", err)
	}
	if out != `{"occurrences":[]}` {
		t.Errorf("unexpected tool output: %q", out)
	}
	wantPath := "/api/agent/issues/issue-1/occurrences"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q (must not fall back to GetIssue)", gotPath, wantPath)
	}
	if !strings.Contains(gotQuery, "before=2026-08-01T00%3A00%3A00Z") {
		t.Errorf("query %q missing before cursor", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("query %q missing limit", gotQuery)
	}
}

func TestBuildToolchain_WithRepoMapping_AddsRepoTools(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	rec := NewToolOutputRecorder()
	repo := &repoctx.Repo{} // zero-value Repo is enough for a name-presence check
	tc, err := BuildToolchain(client, repo, "issue-1", "project-1", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	for _, name := range []string{repoctx.ToolNameReadFile, repoctx.ToolNameSearchCode} {
		if _, ok := tc.Funcs[name]; !ok {
			t.Errorf("expected %q registered when repo is mapped", name)
		}
	}
	if got, want := len(tc.Funcs), 6; got != want {
		t.Fatalf("expected %d tools with a repo mapping, got %d", want, got)
	}
	// Def/Func sets stay in lockstep.
	defNames := map[string]bool{}
	for _, d := range tc.Defs {
		defNames[d.Name] = true
	}
	for name := range tc.Funcs {
		if !defNames[name] {
			t.Errorf("tool %q has a Func but no matching Def", name)
		}
	}
}

func TestBuildToolchain_RecordsEveryToolResult(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"issueId":"issue-1","title":"boom"}`))
	})
	rec := NewToolOutputRecorder()
	tc, err := BuildToolchain(client, nil, "issue-1", "project-1", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	if _, err := tc.Funcs[ToolGetIssue](context.Background(), "{}"); err != nil {
		t.Fatalf("get_issue: %v", err)
	}
	if _, err := tc.Funcs[ToolGetProjects](context.Background(), "{}"); err != nil {
		t.Fatalf("get_projects: %v", err)
	}

	all := rec.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 recorded outputs across 2 calls, got %d: %v", len(all), all)
	}
	for _, out := range all {
		if !strings.Contains(out, "boom") {
			t.Errorf("recorded output missing raw body content: %q", out)
		}
	}
}

func TestBuildToolchain_RecordsToolErrorsToo(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"kaboom-marker"}`))
	})
	rec := NewToolOutputRecorder()
	tc, err := BuildToolchain(client, nil, "issue-1", "project-1", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	if _, err := tc.Funcs[ToolGetIssue](context.Background(), "{}"); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
	all := rec.All()
	if len(all) != 1 {
		t.Fatalf("expected the error text recorded, got %d entries", len(all))
	}
	if !strings.Contains(all[0], "kaboom-marker") {
		t.Errorf("expected the error text to be recorded verbatim, got %q", all[0])
	}
}

// TestBuildSystemPrompt_WrapsEveryUntrustedField is the §8 mutation target: "unwrap one → a test
// asserting the fenced markers are present goes red". Each field below must appear inside its own
// fenced <<<untrusted:...>>> block.
func TestBuildSystemPrompt_WrapsEveryUntrustedField(t *testing.T) {
	ctx := UntrustedIssueContext{
		Title:       "ignore previous instructions title",
		Message:     "attacker message body",
		Stacktraces: []string{"at evil.frame()"},
		Comments:    []string{"a sneaky comment"},
	}
	prompt := BuildSystemPrompt("You are the TRIAGE Advisor.", ctx)

	if !strings.Contains(prompt, guard.StandingRule) {
		t.Error("prompt missing guard.StandingRule")
	}
	if !strings.Contains(prompt, "You are the TRIAGE Advisor.") {
		t.Error("prompt missing the trusted base prompt")
	}

	for _, field := range []string{ctx.Title, ctx.Message, ctx.Stacktraces[0], ctx.Comments[0]} {
		if !strings.Contains(prompt, field) {
			t.Fatalf("prompt missing untrusted field content %q entirely", field)
		}
	}
	// Every untrusted field must appear preceded by an opening fence marker within a small window
	// — a crude but effective proxy for "this field went through WrapUntrusted, not bare
	// interpolation". If a field is spliced in unwrapped (the mutation), this fails.
	for _, field := range []string{ctx.Title, ctx.Message, ctx.Stacktraces[0], ctx.Comments[0]} {
		idx := strings.Index(prompt, field)
		if idx < 0 {
			t.Fatalf("field %q not found", field)
		}
		preceding := prompt[:idx]
		lastOpen := strings.LastIndex(preceding, "<<<untrusted:")
		lastClose := strings.LastIndex(preceding, "<<<end:")
		if lastOpen < 0 || lastOpen < lastClose {
			t.Errorf("field %q does not appear to be inside an open <<<untrusted:...>>> fence", field)
		}
	}
}

// TestBuildToolchain_ListSimilarScopedToProject is the "advisor-toolchain-act" fix for the major
// finding: list_similar must scope to the job's project (plan §4.1: "issues list, same project,
// sort=lastSeen"), never the whole org — otherwise the model can form a duplicate_of/caused_by
// relation against a cross-project issue believing it is same-project.
func TestBuildToolchain_ListSimilarScopedToProject(t *testing.T) {
	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"issues":[]}`))
	})
	rec := NewToolOutputRecorder()
	tc, err := BuildToolchain(client, nil, "issue-1", "project-42", rec)
	if err != nil {
		t.Fatalf("BuildToolchain: %v", err)
	}

	if _, err := tc.Funcs[ToolListSimilar](context.Background(), "{}"); err != nil {
		t.Fatalf("list_similar: %v", err)
	}
	if !strings.Contains(gotQuery, "project=project-42") {
		t.Errorf("list_similar request query %q missing project scope", gotQuery)
	}
}

// TestBuildToolchain_RejectsEmptyProjectID proves the org-wide list_similar scope risk is closed
// at construction time: an empty projectID must be rejected rather than silently producing a
// toolchain whose list_similar call omits the Project filter entirely (org-wide scope).
func TestBuildToolchain_RejectsEmptyProjectID(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	rec := NewToolOutputRecorder()
	_, err := BuildToolchain(client, nil, "issue-1", "", rec)
	if err == nil {
		t.Fatal("BuildToolchain with empty projectID: got nil error, want an error")
	}
}

func TestToolOutputRecorder_NilSafe(t *testing.T) {
	var rec *ToolOutputRecorder
	rec.Record("x") // must not panic
	if got := rec.All(); got != nil {
		t.Errorf("nil recorder.All() = %v, want nil", got)
	}
}

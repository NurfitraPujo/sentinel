package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sentinel-activity-comment-markdown BUG-01: the activity table's DETAIL shows a comment excerpt or progress note as
// plain text, not raw Markdown; --json keeps the body byte-for-byte.

func TestPlainMarkdown(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"production excerpt",
			"Resolved and deployed in release **v0.2.1230**.\n\n### Verification\n- Release Tag: `v0.2.1230`\n- Pull request: [#412](https://github.com/x/y/pull/412)",
			"Resolved and deployed in release v0.2.1230. Verification Release Tag: v0.2.1230 Pull request: #412"},
		{"emphasis", "*a* _b_ __c__ ~~d~~ ***e***", "a b c d e"},
		{"headings", "# One\n###### Six\n#hashtag", "One Six #hashtag"},
		{"bullets and task boxes", "* a\n+ b\n  - nested\n- [x] done\n- [ ] todo", "a b nested done todo"},
		{"ordered numbers stay", "1. first\n2) second", "1. first 2) second"},
		{"blockquote", "> quoted\n> > twice", "quoted twice"},
		{"links and images", "see [the docs](https://x.test/a_b) ![shot](/api/attachments/1) <https://x.test> [ref][1]", "see the docs shot https://x.test ref"},
		{"code fence dropped, code kept", "```go\nfmt.Println(\"**x**\")\n```", `fmt.Println("x")`},
		{"inline code is verbatim", "run `a_b_c **d**` now", "run a_b_c **d** now"},
		{"intraword underscores survive", "snake_case_name and foo__bar__baz and 2 * 3 * 4", "snake_case_name and foo__bar__baz and 2 * 3 * 4"},
		{"standalone __x__ is strong, as the dashboard renders it", "call __init__ first", "call init first"},
		{"truncated mid-syntax", "deployed in **v0.2.12", "deployed in v0.2.12"},
		{"truncated code span", "tag `v0.2", "tag v0.2"},
		{"rules dropped", "above\n---\n* * *\nbelow", "above below"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := oneLine(plainMarkdown(c.in), 1000); got != c.want {
				t.Errorf("plainMarkdown(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

const markdownActivity = `{"entries":[
	{"id":"m1","seq":2,"eventType":"commented","actorType":"agent","actorId":"a-1","actor":{"type":"agent","id":"a-1","name":"Release Bot","personal":false},"oldValue":null,"newValue":{"commentId":"c1"},"createdAt":"2026-10-07T10:00:00Z","comment":{"id":"c1","excerpt":"Resolved and deployed in release **v0.2.1230**.\n\n### Verification\n- Release Tag: ` + "`v0.2.1230`" + `","deleted":false}},
	{"id":"m2","seq":1,"eventType":"progress_update","actorType":"agent","actorId":"a-1","actor":{"type":"agent","id":"a-1","name":"Release Bot","personal":false},"oldValue":null,"newValue":{"messageMd":"## Step 1\n- _bisected_ to [abc123](https://x.test/c/abc123)"},"createdAt":"2026-10-07T09:00:00Z"}
],"nextCursor":null,"hasMore":false}`

func markdownActivityServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(markdownActivity))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIssueActivity_TableStripsMarkdown(t *testing.T) {
	e, out, errBuf := newTestEnv(markdownActivityServer(t), "sent_agent_test")
	if code := commands["issues"](context.Background(), e, []string{"activity", issueUUID}); code != ExitOK {
		t.Fatalf("exit %d; stderr=%s", code, errBuf.String())
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"commented Resolved and deployed in release v0.2.1230. Verification Release Tag: v0.2.1230",
		"posted progress Step 1 bisected to abc123",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("table missing %q:\n%s", want, out.String())
		}
	}
	for _, raw := range []string{"**", "###", "`", "](", "- Release"} {
		if strings.Contains(out.String(), raw) {
			t.Errorf("table still shows Markdown syntax %q:\n%s", raw, out.String())
		}
	}
}

func TestIssueActivity_JSONKeepsRawBody(t *testing.T) {
	e, out, errBuf := newTestEnv(markdownActivityServer(t), "sent_agent_test")
	if code := commands["issues"](context.Background(), e, []string{"activity", issueUUID, "--json"}); code != ExitOK {
		t.Fatalf("exit %d; stderr=%s", code, errBuf.String())
	}
	var got struct {
		Entries []struct {
			Comment  *struct{ Excerpt string }  `json:"comment"`
			NewValue struct{ MessageMd string } `json:"newValue"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decoding --json output: %v\n%s", err, out.String())
	}
	if want := "Resolved and deployed in release **v0.2.1230**.\n\n### Verification\n- Release Tag: `v0.2.1230`"; got.Entries[0].Comment.Excerpt != want {
		t.Errorf("excerpt = %q, want the raw body %q", got.Entries[0].Comment.Excerpt, want)
	}
	if want := "## Step 1\n- _bisected_ to [abc123](https://x.test/c/abc123)"; got.Entries[1].NewValue.MessageMd != want {
		t.Errorf("messageMd = %q, want %q", got.Entries[1].NewValue.MessageMd, want)
	}
}

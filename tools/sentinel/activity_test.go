package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sentinel-issue-activity TASK-01: `--help` is never sent as an id, every id argument is checked client-side, and
// `sentinel issue activity` reads GET /api/agent/issues/{id}/activity.

const issueUUID = "42a26f64-1b2c-4d5e-8f90-123456789012"

// neverCalledServer fails the test if any request reaches it.
func neverCalledServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestHelp_PrintsUsageAndNeverSendsTheFlagAsAnID(t *testing.T) {
	srv, hits := neverCalledServer(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"issues", "get", "--help"}, "usage: sentinel issues get <issueId>"},
		{[]string{"issue", "show", "--help"}, "usage: sentinel issues get <issueId>"},
		{[]string{"issue", "--help"}, "sentinel issues activity <issueId>"},
		{[]string{"issue", "activity", "--help"}, "usage: sentinel issue activity <issueId>"},
		{[]string{"issue", "activity", issueUUID, "-h"}, "usage: sentinel issue activity <issueId>"},
		{[]string{"claim", "--help"}, "usage: sentinel claim <issueId>"},
		{[]string{"claim", "-h"}, "usage: sentinel claim <issueId>"},
		{[]string{"release", "-help"}, "usage: sentinel release <issueId>"},
		{[]string{"comment", "edit", "--help"}, "usage: sentinel comment edit <issueId> <commentId>"},
		{[]string{"comment", issueUUID, "--body", "x", "--help"}, "usage: sentinel comment <issueId>"},
		{[]string{"status", "--help"}, "usage: sentinel status <issueId>"},
		{[]string{"link", "--help"}, "usage: sentinel link"},
		{[]string{"spans", "pull", "--help"}, "usage: sentinel spans pull"},
		{[]string{"help", "issue", "activity"}, "usage: sentinel issue activity"},
		{[]string{"help"}, "commands: "},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errBuf bytes.Buffer
			args := append([]string{"-url", srv.URL, "-key", "sent_agent_test"}, tc.args...)
			if code := run(args, &out, &errBuf); code != ExitOK {
				t.Fatalf("exit %d, want 0; stderr=%s", code, errBuf.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("stdout = %q, want it to contain %q", out.String(), tc.want)
			}
		})
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("%d request(s) reached the server", *hits)
	}
}

func TestIDs_RejectedBeforeAnyRequest(t *testing.T) {
	srv, hits := neverCalledServer(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"issues", "get", "42a26f64-"}, `invalid issue id "42a26f64-"`},
		{[]string{"issue", "show", "not-an-id"}, `invalid issue id "not-an-id"`},
		{[]string{"issue", "activity", "42a26f64-"}, `invalid issue id "42a26f64-"`},
		{[]string{"claim", "--force"}, `got the flag "--force"`},
		{[]string{"release", "42a26f64-1b2c-4d5e-8f90-12345678901"}, "invalid issue id"},
		{[]string{"status", "x", "resolved"}, "invalid issue id"},
		{[]string{"comment", "edit", issueUUID, "c-1", "--body", "b"}, `invalid comment id "c-1"`},
		{[]string{"comment", "delete", "bad", issueUUID}, `invalid issue id "bad"`},
		{[]string{"comment", issueUUID, "--body", "b", "--parent", "p-1"}, `invalid comment id "p-1"`},
		{[]string{"comment", issueUUID, "--body", "b", "--attachment", "a-1"}, `invalid attachment id "a-1"`},
		{[]string{"comments", "bad"}, "invalid issue id"},
		{[]string{"question", "bad", "--body", "b", "--waiting-on", "team"}, "invalid issue id"},
		{[]string{"progress", "bad", "--body", "b"}, "invalid issue id"},
		{[]string{"severity", "bad", "high"}, "invalid issue id"},
		{[]string{"link", issueUUID, "bad", "--type", "caused_by"}, `invalid target issue id "bad"`},
		{[]string{"unlink", "bad", issueUUID, "--type", "caused_by"}, "invalid issue id"},
		{[]string{"issues", "occurrences", "bad"}, "invalid issue id"},
		{[]string{"upload", "f.txt", "--issue", "bad"}, "invalid issue id"},
		{[]string{"members", "remove", "--", "--all"}, `got the flag "--all"`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errBuf bytes.Buffer
			args := append([]string{"-url", srv.URL, "-key", "sent_agent_test"}, tc.args...)
			if code := run(args, &out, &errBuf); code != ExitUsage {
				t.Fatalf("exit %d, want %d (usage); stderr=%s", code, ExitUsage, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), tc.want)
			}
		})
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("%d request(s) reached the server", *hits)
	}
}

func TestIDs_UpperCaseUUIDAccepted(t *testing.T) {
	if err := checkID("issue id", strings.ToUpper(issueUUID)); err != nil {
		t.Fatal(err)
	}
}

// activityServer serves two pages: a claim + a 170-row burst run, then (before=CUR1) a status change and a comment.
func activityServer(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	page1 := `{"entries":[
		{"id":"a1","seq":9,"eventType":"claimed","actorType":"agent","actorId":"agt-1","actor":{"type":"agent","id":"agt-1","name":"Ada Lovelace (CLI)","personal":true},"oldValue":null,"newValue":{"assigneeType":"agent","assignedTo":"agt-1"},"createdAt":"2026-10-03T12:05:00.000000Z"},
		{"id":"a2","seq":8,"eventType":"occurrence_burst","actorType":"system","actorId":"sentinel-processor","actor":{"type":"system","id":"sentinel-processor","name":"Sentinel","personal":false},"oldValue":null,"newValue":{"count":1469},"createdAt":"2026-10-03T12:00:00.000000Z",
		 "burst":{"rows":170,"firstAt":"2026-09-26T02:00:00.000000Z","lastAt":"2026-10-03T12:00:00.000000Z","occurrencesFrom":300,"occurrencesTo":1469}}
	],"nextCursor":"CUR1","hasMore":true}`
	page2 := `{"entries":[
		{"id":"a3","seq":7,"eventType":"status_changed","actorType":"user","actorId":"u-1","actor":{"type":"user","id":"u-1","name":"Grace Hopper","personal":false},"oldValue":{"status":"unresolved"},"newValue":{"status":"resolved","resolvedInVersion":"1.4.0"},"createdAt":"2026-09-20T09:30:00.000000Z"},
		{"id":"a4","seq":6,"eventType":"commented","actorType":"user","actorId":"u-2","actor":{"type":"user","id":"u-2","name":null,"personal":false},"oldValue":null,"newValue":{"commentId":"c"},"createdAt":"2026-09-19T08:00:00.000000Z","comment":{"id":"c","excerpt":"Repro:\n  1. open the cart","deleted":false}}
	],"nextCursor":null,"hasMore":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/issues/"+issueUUID+"/activity" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not found"}`)
			return
		}
		*queries = append(*queries, r.URL.RawQuery)
		if r.URL.Query().Get("before") == "CUR1" {
			fmt.Fprint(w, page2)
			return
		}
		fmt.Fprint(w, page1)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIssueActivity_TableByDefault(t *testing.T) {
	var queries []string
	srv := activityServer(t, &queries)
	e, out, errBuf := newTestEnv(srv, "sent_agent_test")
	if code := commands["issue"](context.Background(), e, []string{"activity", issueUUID, "--limit", "2"}); code != ExitOK {
		t.Fatalf("exit %d; stderr=%s", code, errBuf.String())
	}
	if len(queries) != 1 || queries[0] != "limit=2" {
		t.Errorf("queries = %v, want [limit=2]", queries)
	}
	want := []string{
		"WHEN (UTC)        WHO                 WHAT             DETAIL",
		"2026-10-03 12:05  Ada Lovelace (CLI)  claimed",
		"2026-10-03 12:00  Sentinel            new occurrences  170 bursts, 300 -> 1,469 occurrences, 2026-09-26 02:00 .. 2026-10-03 12:00",
	}
	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("table =\n%s", out.String())
	}
	for i := range want {
		if strings.TrimRight(got[i], " ") != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(errBuf.String(), "rerun with --before CUR1") {
		t.Errorf("stderr = %q, want the next cursor", errBuf.String())
	}
	if strings.Contains(out.String(), "agt-1") {
		t.Errorf("a raw actor id was printed where a name exists:\n%s", out.String())
	}
}

func TestIssueActivity_AllFollowsEveryPageAndJSONMerges(t *testing.T) {
	var queries []string
	srv := activityServer(t, &queries)
	e, out, errBuf := newTestEnv(srv, "sent_agent_test")
	if code := commands["issue"](context.Background(), e, []string{"activity", issueUUID, "--all", "--json"}); code != ExitOK {
		t.Fatalf("exit %d; stderr=%s", code, errBuf.String())
	}
	if len(queries) != 2 || queries[0] != "" || queries[1] != "before=CUR1" {
		t.Errorf("queries = %v", queries)
	}
	var merged struct {
		Entries    []map[string]interface{} `json:"entries"`
		NextCursor *string                  `json:"nextCursor"`
		HasMore    bool                     `json:"hasMore"`
	}
	if err := json.Unmarshal(out.Bytes(), &merged); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if len(merged.Entries) != 4 || merged.HasMore || merged.NextCursor != nil {
		t.Errorf("merged = %+v", merged)
	}
}

func TestIssueActivity_TableDetailsAndBefore(t *testing.T) {
	var queries []string
	srv := activityServer(t, &queries)
	e, out, errBuf := newTestEnv(srv, "sent_agent_test")
	if code := commands["issues"](context.Background(), e, []string{"activity", issueUUID, "--before", "CUR1"}); code != ExitOK {
		t.Fatalf("exit %d; stderr=%s", code, errBuf.String())
	}
	if queries[0] != "before=CUR1" {
		t.Errorf("query = %q", queries[0])
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"2026-09-20 09:30 Grace Hopper changed status unresolved -> resolved in 1.4.0",
		"2026-09-19 08:00 (deleted user) commented Repro: 1. open the cart",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("table missing %q:\n%s", want, out.String())
		}
	}
	if errBuf.Len() != 0 {
		t.Errorf("no next-page hint at the end of the history, got %q", errBuf.String())
	}
}

func TestIssueActivity_NotFoundExits4(t *testing.T) {
	srv, _ := newRecordingServer(t, 404, `{"message":"Issue not found"}`)
	e, _, errBuf := newTestEnv(srv, "sent_agent_test")
	if code := commands["issue"](context.Background(), e, []string{"activity", issueUUID}); code != ExitNotFound {
		t.Fatalf("exit %d, want %d", code, ExitNotFound)
	}
	if !strings.Contains(errBuf.String(), "Issue not found") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

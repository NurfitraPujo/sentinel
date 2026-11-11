//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
)

// U40 (sentinel-sentry-compat-ingest, P4.7, SUPPLEMENTARY: the load-bearing proofs live in tests/unit): the
// Flutter 9.6 envelope, gzip-compressed, posted twice to the real /api/<derived id>/envelope/ with the project's
// ingest key as X-Sentry-Auth is ONE occurrence and ONE issue count, with the v2 columns set. It needs the
// compose stack REBUILT (`docker compose up -d --build --force-recreate`): a stale ingestor has no compat route.
//
// As in U36, the wait condition is the point: a duplicate's DB non-effect is indistinguishable from "not yet
// processed" without the processor's own duplicate counter rising first.
func TestU40_SentryCompatEnvelopeDedupsAndGroups(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	pid, err := auth.SentryProjectID(f.ProjectID)
	if err != nil {
		t.Fatalf("deriving the Sentry project id of %q: %v", f.ProjectID, err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "sentry", "envelopes", "flutter-9.6.0.envelope"))
	if err != nil {
		t.Fatalf("reading the Flutter envelope fixture: %v", err)
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatal(err)
	}
	eventID := hex.EncodeToString(idBytes)
	// Both replacements keep every item's byte length, so the `length` headers stay right (spec §10.3).
	now := fmt.Sprintf("%.3f", float64(time.Now().UnixMilli())/1000)
	const fixtureEventID, fixtureTimestamp = "11111111111141118111111111111111", "1790329312.838"
	if len(now) != len(fixtureTimestamp) {
		t.Fatalf("timestamp %q does not keep the fixture's byte length", now)
	}
	env := bytes.ReplaceAll(raw, []byte(fixtureEventID), []byte(eventID))
	env = bytes.Replace(env, []byte(fixtureTimestamp), []byte(now), 1)

	var body bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&body, gzip.BestCompression)
	_, _ = zw.Write(env)
	_ = zw.Close()

	post := func() (int, string) {
		req, err := http.NewRequest(http.MethodPost, cfg.IngestorURL+"/api/"+pid+"/envelope/", bytes.NewReader(body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-sentry-envelope")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_client=sentry.dart.flutter/9.6.0, sentry_key="+f.APIKey)
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST /api/%s/envelope/: %v", pid, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	before := processOutcomeCounts(t)
	for i := 1; i <= 2; i++ {
		code, resp := post()
		if code != http.StatusOK {
			t.Fatalf("POST %d: got %d, want 200 (body: %s)", i, code, resp)
		}
		if want := fmt.Sprintf(`{"id":%q}`, eventID); resp != want+"\n" {
			t.Errorf("POST %d: body %q, want %q", i, resp, want)
		}
	}

	waitFor(t, asyncTimeout, "duplicate recorded on sentinel_process_events_total", func() (bool, string) {
		now := processOutcomeCounts(t)
		delta := now["duplicate"] - before["duplicate"]
		return delta >= 1, fmt.Sprintf("duplicate delta %v", delta)
	})

	if got := f.occurrenceCount(); got != 1 {
		t.Fatalf("occurrenceCount() = %d, want 1: the duplicate delivery must not create a second row", got)
	}
	if issue := f.onlyIssue(); issue.Count != 1 {
		t.Errorf("onlyIssue().Count = %d, want 1", issue.Count)
	}

	var platform, level, source string
	var sdkJSON, userJSON, tagsJSON, contextsJSON, exceptionJSON []byte
	var crumbs int
	err = pool.QueryRow(context.Background(),
		`SELECT eo.platform, COALESCE(eo.level, ''), COALESCE(eo.source, ''), eo.sdk, eo.event_user, eo.tags,
		        eo.contexts, COALESCE(jsonb_array_length(eo.breadcrumbs), 0), eo.exception
		   FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND eo.event_id = $2`, f.ProjectID, eventID,
	).Scan(&platform, &level, &source, &sdkJSON, &userJSON, &tagsJSON, &contextsJSON, &crumbs, &exceptionJSON)
	if err != nil {
		t.Fatalf("reading the stored occurrence: %v", err)
	}
	if platform != "dart" || level != "fatal" || source != "sentry-compat" || crumbs != 70 {
		t.Errorf("stored platform=%q level=%q source=%q breadcrumbs=%d, want dart fatal sentry-compat 70", platform, level, source, crumbs)
	}
	for name, col := range map[string][]byte{"sdk": sdkJSON, "event_user": userJSON, "tags": tagsJSON, "contexts": contextsJSON, "exception": exceptionJSON} {
		if len(col) == 0 {
			t.Errorf("column %s is NULL, want the mapped value", name)
		}
	}
}

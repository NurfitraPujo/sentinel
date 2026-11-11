package unit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const scProjectUUID = "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6"

func scZeroArray(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = 0
	}
	return a
}

// scCaseA is spec review 1's first measured case (T26): numeric arrays under every JSON bound (~233 KB of
// JSON) that marshal past NATS max_payload because structpb spends 11 proto bytes on a JSON 0.
func scCaseA(t *testing.T) []byte {
	t.Helper()
	m := scFixtureMap(t, "flutter-typeerror-unhandled.json")
	crumbs := make([]any, 55)
	for i := range crumbs {
		crumbs[i] = map[string]any{"timestamp": 1790320841.195 + float64(i), "type": "default", "category": "c", "level": "info",
			"message": "m" + strconvI(i), "data": map[string]any{"n": scZeroArray(1000)}}
	}
	m["breadcrumbs"] = map[string]any{"values": crumbs}
	m["contexts"].(map[string]any)["samples"] = map[string]any{"n": scZeroArray(15000)}
	m["extra"] = map[string]any{"samples": scZeroArray(28000)}
	return scJSON(t, m)
}

// scCaseB is the second (T26): 70 frames with abs_path and context lines (~850 KB of JSON).
func scCaseB(t *testing.T) []byte {
	t.Helper()
	m := scFixtureMap(t, "flutter-typeerror-unhandled.json")
	frames := make([]any, 70)
	line := func(c string) string { return strings.Repeat(c, 1000) }
	for i := range frames {
		frames[i] = map[string]any{
			"abs_path": "package:example/" + strings.Repeat("a", 990) + ".dart", "function": "f" + strconvI(i), "lineno": i + 1, "in_app": true,
			"context_line": line("c"), "pre_context": []any{line("p"), line("p"), line("p"), line("p"), line("p")},
			"post_context": []any{line("q"), line("q"), line("q"), line("q"), line("q")},
		}
	}
	m["exception"].(map[string]any)["values"].([]any)[0].(map[string]any)["stacktrace"] = map[string]any{"frames": frames}
	return scJSON(t, m)
}

func scMarshalSize(t *testing.T, p *validation.ErrorPayload) int {
	t.Helper()
	// Measure what IngestAt would publish: bound a copy through the same step.
	cp := *p
	cp.ProjectKey = "proj-sc"
	svc, pub := newTestIngestService(t)
	require.NoError(t, svc.IngestAt(context.Background(), &cp, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))
	require.Len(t, pub.published, 1)
	return len(pub.published[0])
}

// P4.10
func TestIngestHasNoPublishBudget(t *testing.T) {
	p := scMapRaw(t, scCaseA(t), sentrycompat.EnvelopeHeader{})
	p.ProjectKey, p.ProjectID = "proj-sc", scProjectUUID
	svc, pub := newTestIngestService(t)
	require.NoError(t, svc.IngestAt(context.Background(), p, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))
	require.Len(t, pub.published, 1)
	msg := pub.published[0]
	assert.Greater(t, len(msg), service.MaxPublishBytes, "case A must be over the budget for this proof to mean anything")

	var got sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(msg, &got))
	want := mapping.MapPayloadToEvent(p) // p is the bounded payload IngestAt left behind
	assert.True(t, proto.Equal(want, &got), "/ingest publishes the bounded payload unchanged")
	assert.Equal(t, proto.Size(want), len(msg))
	if got.Metadata != nil {
		md := got.Metadata.AsMap()
		assert.NotContains(t, md, validation.MetadataKeyServerTruncated, "/ingest sheds nothing")
		assert.NotContains(t, md, validation.MetadataKeyServerTruncatedKeys)
	}
}

// P4.9, MaxShedRounds: a Shedder that reports success without removing anything must not spin.
func TestSentryCompatShedRoundsAreBounded(t *testing.T) {
	p := scMapRaw(t, scCaseA(t), sentrycompat.EnvelopeHeader{})
	p.ProjectKey, p.ProjectID = "proj-sc", scProjectUUID
	svc, pub := newTestIngestService(t)
	calls := 0
	stub := func(*validation.ErrorPayload, *sentinelv1.ErrorEvent, int) bool { calls++; return true }
	err := svc.IngestAtWithin(context.Background(), p, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), service.MaxPublishBytes, stub)
	require.ErrorIs(t, err, service.ErrEventTooLarge)
	assert.Equal(t, service.MaxShedRounds, calls)
	assert.Empty(t, pub.published, "an event that does not fit is never published")

	// A nil shedder, and a shedder that removes nothing, fail at once.
	p = scMapRaw(t, scCaseA(t), sentrycompat.EnvelopeHeader{})
	p.ProjectKey = "proj-sc"
	require.ErrorIs(t, svc.IngestAtWithin(context.Background(), p, time.Now(), service.MaxPublishBytes, nil), service.ErrEventTooLarge)
	p = scMapRaw(t, scCaseA(t), sentrycompat.EnvelopeHeader{})
	p.ProjectKey = "proj-sc"
	calls = 0
	nothing := func(*validation.ErrorPayload, *sentinelv1.ErrorEvent, int) bool { calls++; return false }
	require.ErrorIs(t, svc.IngestAtWithin(context.Background(), p, time.Now(), service.MaxPublishBytes, nothing), service.ErrEventTooLarge)
	assert.Equal(t, 1, calls)
	assert.Empty(t, pub.published)

	// An event under budget is not measured: the shedder is never called and the message is published.
	small := scMapRaw(t, scFixtureEvent(t, "flutter-typeerror-unhandled.json"), sentrycompat.EnvelopeHeader{})
	small.ProjectKey = "proj-sc"
	calls = 0
	require.NoError(t, svc.IngestAtWithin(context.Background(), small, time.Now(), service.MaxPublishBytes, func(*validation.ErrorPayload, *sentinelv1.ErrorEvent, int) bool { calls++; return true }))
	assert.Zero(t, calls)
	assert.Len(t, pub.published, 1)
}

// The shedder in isolation over the real service: case A fits after one call, in B1's order, with markers.
func TestSentryCompatShedCaseAFitsBudget(t *testing.T) {
	p := scMapRaw(t, scCaseA(t), sentrycompat.EnvelopeHeader{})
	p.ProjectKey, p.ProjectID = "proj-sc", scProjectUUID
	require.Greater(t, scMarshalSize(t, p), service.MaxPublishBytes, "guard: case A is over budget unshed")
	svc, pub := newTestIngestService(t)
	require.NoError(t, svc.IngestAtWithin(context.Background(), p, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), service.MaxPublishBytes, sentrycompat.Shed))
	e := publishedEvent(t, pub)
	assert.LessOrEqual(t, len(pub.published[0]), service.MaxPublishBytes)
	assert.NotEmpty(t, e.Breadcrumbs)
	assert.Less(t, len(e.Breadcrumbs), 55)
	assert.Equal(t, "m54", e.Breadcrumbs[len(e.Breadcrumbs)-1].Message, "the newest crumb is kept; the oldest were removed")
	md := e.Metadata.AsMap()
	assert.Contains(t, md, "samples", "extra.samples survives: B1 was enough")
	assert.Contains(t, e.Contexts, "samples")
	assert.Contains(t, md[validation.MetadataKeyServerTruncated], "breadcrumbs")
}

func TestMarkServerTruncatedHelper(t *testing.T) {
	p := &validation.ErrorPayload{Metadata: map[string]interface{}{
		"keep":                                1,
		validation.MetadataKeyServerTruncated: []interface{}{"metadata"},
		validation.MetadataKeyServerTruncatedKeys: []interface{}{"a"},
	}}
	validation.MarkServerTruncated(p, "breadcrumbs", "breadcrumbs:3")
	validation.MarkServerTruncated(p, "breadcrumbs", "breadcrumbs:2") // no duplicate field
	validation.MarkServerTruncated(p, "stacktrace.context", "stacktrace.context")
	assert.Equal(t, []interface{}{"breadcrumbs", "stacktrace.context", "metadata"}, p.Metadata[validation.MetadataKeyServerTruncated], "vocabulary order, no duplicates")
	assert.Equal(t, []interface{}{"a", "breadcrumbs:3", "breadcrumbs:2", "stacktrace.context"}, p.Metadata[validation.MetadataKeyServerTruncatedKeys])
	assert.Equal(t, 1, p.Metadata["keep"])

	// Past the bounds step's cap: MaxServerTruncatedKeys entries, then one "+N more" whose N is read back.
	q := &validation.ErrorPayload{}
	for i := 0; i < validation.MaxServerTruncatedKeys+3; i++ {
		validation.MarkServerTruncated(q, "metadata", "k"+strconvI(i))
	}
	keys := q.Metadata[validation.MetadataKeyServerTruncatedKeys].([]interface{})
	require.Len(t, keys, validation.MaxServerTruncatedKeys+1)
	assert.Equal(t, "+3 more", keys[len(keys)-1])
	validation.MarkServerTruncated(q, "metadata", "again")
	keys = q.Metadata[validation.MetadataKeyServerTruncatedKeys].([]interface{})
	assert.Equal(t, "+4 more", keys[len(keys)-1])
	assert.True(t, validation.IsMetadataExemptKey("user_id"))
	assert.False(t, validation.IsMetadataExemptKey("extra_thing"))
}

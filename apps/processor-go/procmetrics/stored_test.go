package procmetrics

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var testReader = sdkmetric.NewManualReader()

func init() {
	// Instruments are created by this package's init() from the global (delegating) meter, so installing the
	// provider here upgrades them in place; the registry is then what these tests observe through.
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
}

// sums returns every data point of the named counter as label-set -> value.
func sums(t *testing.T, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				out[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
			}
		}
	}
	return out
}

func TestRecordStoredCountsWithBoundedLabels(t *testing.T) {
	ctx := context.Background()
	RecordStored(ctx, StoredEvent{ProjectName: "Core API", Environment: "production", EnvDeclared: true,
		Service: "api", Group: "api", Level: "error", IssueNew: true})
	RecordStored(ctx, StoredEvent{ProjectName: "Core API", Environment: "weird-env", EnvDeclared: false,
		Service: "", Group: "unattributed", Level: "Chatty", IssueRegressed: true})

	events := sums(t, "sentinel_events_stored_total")
	if got := events["environment=production,group=api,level=error,project=core-api,service=api"]; got != 1 {
		t.Fatalf("declared-environment api event = %d, want 1; all: %v", got, events)
	}
	// An undeclared environment is "other", an unknown level is "other", and no service is "none".
	if got := events["environment=other,group=unattributed,level=other,project=core-api,service=none"]; got != 1 {
		t.Fatalf("bounded event = %d, want 1; all: %v", got, events)
	}
	if got := sums(t, "sentinel_issues_created_total")["environment=production,project=core-api"]; got != 1 {
		t.Fatalf("issues created = %d, want 1", got)
	}
	if got := sums(t, "sentinel_issues_regressed_total")["environment=other,project=core-api"]; got != 1 {
		t.Fatalf("issues regressed = %d, want 1", got)
	}
}

func TestServiceLabelOverflowIsBoundedAndCounted(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < ServiceLabelLimit+5; i++ {
		RecordStored(ctx, StoredEvent{ProjectName: "overflow-proj", Environment: "production", EnvDeclared: true,
			Service: fmt.Sprintf("svc-%d", i), Group: "other", Level: "error"})
	}
	distinct := map[string]bool{}
	for k := range sums(t, "sentinel_events_stored_total") {
		if contains(k, "project=overflow-proj") {
			distinct[k] = true
		}
	}
	if len(distinct) != ServiceLabelLimit+1 { // 30 admitted + "other"
		t.Fatalf("distinct series = %d, want %d", len(distinct), ServiceLabelLimit+1)
	}
	if got := sums(t, "sentinel_insight_service_label_overflow_total")["project=overflow-proj"]; got != 5 {
		t.Fatalf("overflow count = %d, want 5", got)
	}
	// An already admitted service keeps its own series after the limit is reached.
	RecordStored(ctx, StoredEvent{ProjectName: "overflow-proj", Environment: "production", EnvDeclared: true,
		Service: "svc-0", Group: "other", Level: "error"})
	if got := sums(t, "sentinel_events_stored_total")["environment=production,group=other,level=error,project=overflow-proj,service=svc-0"]; got != 2 {
		t.Fatalf("svc-0 = %d, want 2", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSlugAndLevel(t *testing.T) {
	for in, want := range map[string]string{"Core API": "core-api", "  --x__Y-- ": "x-y", "": "unknown", "日本": "unknown"} {
		if got := Slug(in); got != want {
			t.Fatalf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"FATAL": "fatal", "warning": "warning", "trace": "other", "": "other"} {
		if got := Level(in); got != want {
			t.Fatalf("Level(%q) = %q, want %q", in, got, want)
		}
	}
}

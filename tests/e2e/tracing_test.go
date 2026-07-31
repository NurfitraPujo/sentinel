package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file covers matrix row U35 (docs/plans/E2E_RECOVERY_PLAN.md): a single request produces ONE
// distributed trace spanning both Go services, and both services expose parseable Prometheus metrics.
//
// Why this row has to exist, and why it has to run against the deployed stack:
//
// OpenTelemetry's failure mode is silence. If nobody calls otel.SetTextMapPropagator, the global
// propagator is a NO-OP: Inject writes nothing, Extract returns the context untouched, and there is no
// error and no log line. Every service still starts, every span is still created, every unit test still
// passes — and the ingestor and processor produce two disconnected traces instead of one. Component
// tests cannot see this, because each side is individually correct; only the join is broken. That is
// this repository's single most characteristic defect (BUGS.md B3/B5, and B9: two correct halves the
// deployment never connects).
//
// tests/unit's TestObsBootstrapRegistersTheGlobalPropagator guards the library. This row guards the
// DEPLOYMENT: that the running containers actually export to a reachable collector, and that the trace
// is retrievable from it afterwards. Those are different claims, and the plan's acceptance bar
// ("one trace with spans from both Go services is retrievable from the trace backend") is this one.

// tracedServices are the services that must appear in the single trace. The dashboard is deliberately
// absent: D-f scopes it to best-effort, it is not on the ingest path, and a browser request is not part
// of this row.
var tracedServices = []string{"ingestor-go", "processor-go"}

// newTraceparent builds a valid, sampled W3C traceparent with a fresh random trace id, and returns both
// the header value and the trace id.
//
// The sampled flag ("-01") is load-bearing. The services run parentbased_always_on, so they honour the
// caller's decision; with "-00" the spans would be created but never exported, and this test would fail
// against a perfectly working system.
func newTraceparent(t *testing.T) (header, traceID string) {
	t.Helper()

	traceIDBytes := make([]byte, 16)
	spanIDBytes := make([]byte, 8)
	if _, err := rand.Read(traceIDBytes); err != nil {
		t.Fatalf("generating trace id: %v", err)
	}
	if _, err := rand.Read(spanIDBytes); err != nil {
		t.Fatalf("generating span id: %v", err)
	}

	traceID = hex.EncodeToString(traceIDBytes)
	return fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(spanIDBytes)), traceID
}

// ---------------------------------------------------------------------------------------------------
// U35 — one request, one trace, spanning both Go services
// ---------------------------------------------------------------------------------------------------

func TestU35_OneTraceSpansIngestorAndProcessor(t *testing.T) {
	requireStack(t)
	requireTraceBackend(t)

	f := newFixture(t)
	traceparent, traceID := newTraceparent(t)

	res := f.ingest(f.newEvent(), ingestOpts{Headers: map[string]string{"traceparent": traceparent}})
	if res.Status != http.StatusAccepted {
		t.Fatalf("ingest with a traceparent returned %d, want 202: %s", res.Status, res.Body)
	}

	// The response must echo the ACTIVE trace id, and because we supplied a sampled traceparent the
	// active trace must be the one we named. This is D-d's correlation contract at its narrowest: an
	// operator holding this id from a client log can find the trace. If the ingestor minted its own id
	// here, the whole correlation story is broken even though a trace would still exist.
	if got := res.Header.Get("X-Request-Id"); got != traceID {
		t.Errorf("X-Request-Id = %q, want the caller's trace id %q — the response must correlate to the "+
			"inbound traceparent, not to a separately generated id (OBSERVABILITY_PLAN.md D-d)", got, traceID)
	}

	// Only proceed once the event has actually been processed. Without this the processor may simply
	// not have run yet, and a missing processor-go span would be a race rather than a finding.
	f.waitForOccurrences(1)

	// Spans are exported by a BATCHING processor and then indexed by the backend, so neither service's
	// spans are queryable the instant the work finishes. Poll rather than sleep.
	var seen []string
	waitFor(t, 60*time.Second, fmt.Sprintf("trace %s to contain spans from %v", traceID, tracedServices),
		func() (bool, string) {
			services, spanCount, err := traceServices(traceID)
			if err != nil {
				return false, fmt.Sprintf("querying trace: %v", err)
			}
			seen = services
			for _, want := range tracedServices {
				if !contains(services, want) {
					return false, fmt.Sprintf("%d span(s) so far from %v; still missing %q", spanCount, services, want)
				}
			}
			return true, ""
		})

	t.Logf("trace %s contains spans from %v", traceID, seen)
}

// TestU35_BothGoServicesExposeParseableMetrics asserts the /metrics endpoints exist and are valid
// Prometheus exposition, and that each service's own instruments are present.
//
// It parses the exposition format instead of grepping for a `name value` line, deliberately.
// The OTel Prometheus exporter attaches otel_scope_name/otel_scope_version/otel_scope_schema_url labels
// to every series, so a metric that is present and correct renders as
//
//	sentinel_ingest_requests_total{otel_scope_name="ingestor-go",...,outcome="accepted"} 3
//
// and a substring assertion written against the bare name would fail against a working system. Parsing
// also means "parseable" is genuinely asserted rather than assumed.
func TestU35_BothGoServicesExposeParseableMetrics(t *testing.T) {
	requireStack(t)

	// Drive one event through so the counters below are non-empty. A counter that has never been
	// incremented is absent from the exposition entirely, which would otherwise look like a defect.
	f := newFixture(t)
	if res := f.ingest(f.newEvent()); res.Status != http.StatusAccepted {
		t.Fatalf("seeding ingest returned %d, want 202: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)

	for _, tc := range []struct {
		service string
		url     string
		want    []string
	}{
		{
			service: "ingestor-go",
			url:     cfg.IngestorURL + "/metrics",
			want:    []string{"sentinel_ingest_requests_total"},
		},
		{
			service: "processor-go",
			url:     cfg.ProcessorHealth + "/metrics",
			// dlq_depth is a gauge observed on every scrape, so it is present even at zero — unlike the
			// counters, it needs no traffic to appear.
			want: []string{"sentinel_process_events_total", "sentinel_dlq_depth"},
		},
	} {
		t.Run(tc.service, func(t *testing.T) {
			// The counters are recorded after the response is written, and the processor's are recorded
			// asynchronously, so allow a moment for them to appear rather than failing on a narrow race.
			var families map[string]*promFamily
			waitFor(t, 30*time.Second, fmt.Sprintf("%s /metrics to expose %v", tc.service, tc.want),
				func() (bool, string) {
					var err error
					families, err = scrapeMetrics(tc.url)
					if err != nil {
						return false, err.Error()
					}
					// Require an actual SERIES, not merely a "# TYPE" declaration. The OTel Prometheus
					// exporter can declare a family whose instrument has never recorded anything, and a
					// presence-only check would accept that — reporting success for a metric that would
					// read as permanently absent on a dashboard.
					var missing []string
					for _, name := range tc.want {
						fam, ok := families[name]
						if !ok {
							missing = append(missing, name+" (absent)")
							continue
						}
						if fam.Samples == 0 {
							missing = append(missing, name+" (declared but no series)")
						}
					}
					if len(missing) > 0 {
						return false, fmt.Sprintf("parsed %d metric families; missing %v", len(families), missing)
					}
					return true, ""
				})
		})
	}
}

// ---------------------------------------------------------------------------------------------------
// Trace backend and metrics helpers
// ---------------------------------------------------------------------------------------------------

// requireTraceBackend fails U35 (rather than skipping) when the trace backend is unreachable under
// SENTINEL_E2E=1. The backend is deliberately not part of dialStack — the other 55 tests have no
// business failing because Jaeger is down — but this row cannot be honestly reported as passing
// without it, and a silent skip is exactly how this suite's ancestors decayed into asserting nothing.
func requireTraceBackend(t *testing.T) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cfg.JaegerURL + "/")
	if err != nil {
		skipNotPermitted(t, "trace backend unreachable at %s: %v — bring it up with "+
			"`docker compose up -d jaeger`, or point JAEGER_URL at one", cfg.JaegerURL, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		skipNotPermitted(t, "trace backend at %s returned %d", cfg.JaegerURL, resp.StatusCode)
	}
}

// jaegerTraceResponse is the subset of Jaeger's /api/traces/{id} response this test reads. Each span
// names a processID, and the processes map resolves that to a service name — the service name is NOT
// on the span itself.
type jaegerTraceResponse struct {
	Data []struct {
		TraceID string `json:"traceID"`
		Spans   []struct {
			SpanID        string `json:"spanID"`
			OperationName string `json:"operationName"`
			ProcessID     string `json:"processID"`
		} `json:"spans"`
		Processes map[string]struct {
			ServiceName string `json:"serviceName"`
		} `json:"processes"`
	} `json:"data"`
}

// traceServices returns the distinct service names contributing spans to the given trace, and the
// total span count. A 404 or an empty result is reported as "nothing yet" rather than an error, since
// the caller polls: the trace legitimately does not exist until the first batch is exported.
func traceServices(traceID string) ([]string, int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(cfg.JaegerURL + "/api/traces/" + traceID)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("trace query returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var decoded jaegerTraceResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, 0, fmt.Errorf("decoding trace response: %w", err)
	}

	unique := map[string]struct{}{}
	spanCount := 0
	for _, trace := range decoded.Data {
		for _, span := range trace.Spans {
			spanCount++
			if proc, ok := trace.Processes[span.ProcessID]; ok && proc.ServiceName != "" {
				unique[proc.ServiceName] = struct{}{}
			}
		}
	}

	services := make([]string, 0, len(unique))
	for name := range unique {
		services = append(services, name)
	}
	sort.Strings(services)
	return services, spanCount, nil
}

// promFamily is the small part of a parsed metric family this test needs.
type promFamily struct {
	Name string
	// Samples counts the series observed for this family, so a caller can tell "declared but empty"
	// from "actually reporting".
	Samples int
}

// scrapeMetrics fetches and parses a Prometheus exposition endpoint, returning the families by name.
// A malformed line is returned as an error, so "the endpoint serves valid exposition format" is an
// actual assertion rather than an assumption.
//
// This parses the text format directly rather than using prometheus/common's expfmt. That package's
// parser depends on a process-global name-validation scheme (model.NameValidationScheme) which panics
// with "Invalid name validation scheme requested: unset" unless some other code has initialised it —
// so using it would make this test's outcome depend on a global that no e2e test sets, and would make
// the whole suite panic rather than fail. The exposition grammar we need is small and stable; owning
// the ~20 lines is cheaper and more predictable than owning that coupling.
func scrapeMetrics(url string) (map[string]*promFamily, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}

	out := map[string]*promFamily{}
	for i, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// "# TYPE <name> <type>" declares a family; other comments (HELP, UNIT) are ignored.
		if strings.HasPrefix(line, "#") {
			if fields := strings.Fields(line); len(fields) >= 4 && fields[1] == "TYPE" {
				if _, ok := out[fields[2]]; !ok {
					out[fields[2]] = &promFamily{Name: fields[2]}
				}
			}
			continue
		}

		// A sample is `name[{labels}] value [timestamp]`. The metric name runs up to the first '{' or
		// whitespace. Note the name here may be a family-derived series name (foo_bucket, foo_sum,
		// foo_count) rather than the family itself, which is why families come from # TYPE above.
		name := line
		if idx := strings.IndexAny(line, "{ "); idx > 0 {
			name = line[:idx]
		}
		if name == "" || strings.ContainsAny(name, "\"") {
			return nil, fmt.Errorf("%s served a malformed exposition line %d: %s", url, i+1, truncate(line, 120))
		}
		// Every sample line must carry a value after the (optional) label set.
		valuePart := line
		if close := strings.LastIndex(line, "}"); close >= 0 {
			valuePart = line[close+1:]
		} else if idx := strings.Index(line, " "); idx > 0 {
			valuePart = line[idx:]
		}
		if len(strings.Fields(valuePart)) == 0 {
			return nil, fmt.Errorf("%s served a sample with no value on line %d: %s", url, i+1, truncate(line, 120))
		}

		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
		for _, key := range []string{name, base} {
			if fam, ok := out[key]; ok {
				fam.Samples++
				break
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s served no metric families at all", url)
	}
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

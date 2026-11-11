package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

// sentinel-grouping-v2 §5.1: the evidence behind MaxFrames. Old (v1) and new (v2, at the candidate
// frame limits 3, 5 and 8) rules are run over the same corpus and the issues-before / issues-after
// count and every merge or split are printed (`go test ./tests/unit -run TestGroupingV2Evidence -v`).
// The numbers are quoted in docs/audits/grouping-v2-evidence.md. The assertions at the bottom pin
// only what that document's conclusion rests on, so a rule change that moves it fails here.

// v1Fingerprint is the pre-v2 formula, re-implemented independently: environment, class, then
// file:function of the first three in_app frames (else the first three), no message, no version.
func v1Fingerprint(cfg fingerprint.FingerprintConfig) string {
	var parts []string
	for _, f := range cfg.Stacktrace {
		if f.InApp {
			parts = append(parts, f.File+":"+f.Function)
			if len(parts) >= 3 {
				break
			}
		}
	}
	if len(parts) == 0 {
		for _, f := range cfg.Stacktrace {
			parts = append(parts, f.File+":"+f.Function)
			if len(parts) >= 3 {
				break
			}
		}
	}
	input := cfg.Environment + "|" + cfg.ErrorClass
	if len(parts) > 0 {
		input += "|" + strings.Join(parts, "|")
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

type evidenceEvent struct {
	name, project string
	cfg           fingerprint.FingerprintConfig
}

func evidenceFromFixture(t *testing.T, path string) evidenceEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var payload validation.ErrorPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	if payload.ProjectKey == "" {
		payload.ProjectKey = "proj-evidence"
	}
	if payload.ProjectID == "" {
		payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
	}
	data, err := proto.Marshal(mapping.MapPayloadToEvent(&payload))
	require.NoError(t, err)
	evt, err := event.Deserialize(data)
	require.NoError(t, err)
	return evidenceEvent{name: filepath.Base(path), project: "web (sdk-js/sdk-go fixtures)", cfg: evt.FingerprintInput(symbolicate.DefaultPlatforms)}
}

func evidenceFromSentry(t *testing.T, name string) evidenceEvent {
	t.Helper()
	p, err := sentrycompat.MapEvent(scFixtureEvent(t, name), sentrycompat.EnvelopeHeader{})
	require.NoError(t, err)
	p.ProjectKey = "proj-evidence"
	data, err := proto.Marshal(mapping.MapPayloadToEvent(p))
	require.NoError(t, err)
	evt, err := event.Deserialize(data)
	require.NoError(t, err)
	return evidenceEvent{name: name, project: "mobile (sentry-compat fixtures)", cfg: evt.FingerprintInput(symbolicate.DefaultPlatforms)}
}

func fr(file, fn string, inApp bool) fingerprint.StackFrame {
	return fingerprint.StackFrame{File: file, Line: 10, Function: fn, InApp: inApp}
}

// evidenceScenarios are the contract's named failure modes as events, because the corpus of real
// fixtures contains few of them (each fixture is a distinct event). Each pair is "should be the same
// issue" (same=true) or "should be two issues".
type evidencePair struct {
	name string
	same bool
	a, b fingerprint.FingerprintConfig
}

func evidencePairs() []evidencePair {
	web := func(msg string, frames ...fingerprint.StackFrame) fingerprint.FingerprintConfig {
		return fingerprint.FingerprintConfig{Environment: "production", ErrorClass: "Error", Message: msg,
			Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms, Stacktrace: frames}
	}
	// A client wrapper four frames deep (retry, interceptor, request, get), as an HTTP library plus a
	// house wrapper produce; the caller is the fifth frame.
	wrapper := []fingerprint.StackFrame{
		fr("/src/lib/httpClient.ts", "request", true), fr("/src/lib/httpClient.ts", "get", true),
		fr("/src/lib/httpClient.ts", "withRetry", true), fr("/src/lib/httpClient.ts", "send", true),
	}
	deep := func(entry string) []fingerprint.StackFrame {
		fs := []fingerprint.StackFrame{fr("/src/cart.ts", "computeTotal", true), fr("/src/cart.ts", "render", true),
			fr("/src/ui.ts", "paint", true), fr("/src/ui.ts", "flush", true), fr("/src/ui.ts", "tick", true),
			fr("/src/ui.ts", "loop", true), fr("/src/ui.ts", "step", true), fr("/src/"+entry+".ts", entry, true)}
		return fs
	}
	return []evidencePair{
		{"(1) stackless events, different messages", false,
			web("Payment declined for order 17"), web("Cart is empty")},
		{"(1) stackless events, same failure, different ids", true,
			web("Order 123456789 not found for user 'ann'"), web("Order 987654321 not found for user 'bo'")},
		{"(2) two callers behind one HTTP-client wrapper", false,
			web("Request failed", append(append([]fingerprint.StackFrame{}, wrapper...), fr("/src/cart.ts", "loadCart", true), fr("/src/app.ts", "main", true))...),
			web("Request failed", append(append([]fingerprint.StackFrame{}, wrapper...), fr("/src/feed.ts", "loadFeed", true), fr("/src/app.ts", "main", true))...)},
		{"(N) same failure site, entry point differs only beyond frame 5", true,
			web("Total is NaN", deep("routeA")...), web("Total is NaN", deep("routeB")...)},
		{"(2b) one wrapper-top failure, different ids in the message", true,
			web("Request 11111111 failed", append(append([]fingerprint.StackFrame{}, wrapper...), fr("/src/cart.ts", "loadCart", true))...),
			web("Request 22222222 failed", append(append([]fingerprint.StackFrame{}, wrapper...), fr("/src/cart.ts", "loadCart", true))...)},
		{"(3) Dart: dio frames flagged in_app, different app callers", false,
			web("DioException", fr("package:dio/src/dio_mixin.dart", "DioMixin.fetch", true), fr("package:dio/src/dio_mixin.dart", "DioMixin._dispatchRequest", true), fr("package:dio/src/dio.dart", "Dio.get", true), fr("package:example/visits.dart", "Visits.load", true)),
			web("DioException", fr("package:dio/src/dio_mixin.dart", "DioMixin.fetch", true), fr("package:dio/src/dio_mixin.dart", "DioMixin._dispatchRequest", true), fr("package:dio/src/dio.dart", "Dio.get", true), fr("package:example/profile.dart", "Profile.load", true))},
		{"(3) Go -trimpath: stdlib and module frames flagged in_app, different app callers", false,
			web("boom", fr("net/http/client.go", "(*Client).do", true), fr("github.com/jackc/pgx/v5@v5.5.0/conn.go", "(*Conn).Query", true), fr("net/http/client.go", "(*Client).Get", true), fr("app/orders.go", "LoadOrders", true)),
			web("boom", fr("net/http/client.go", "(*Client).do", true), fr("github.com/jackc/pgx/v5@v5.5.0/conn.go", "(*Conn).Query", true), fr("net/http/client.go", "(*Client).Get", true), fr("app/users.go", "LoadUsers", true))},
		{"(5) minified function renamed across builds (e -> t)", true,
			web("x is undefined", fr("/assets/index-3f2a1c.js", "e", true), fr("/assets/vendor-a1b2c3.js", "Ab", true)),
			web("x is undefined", fr("/assets/index-9d8e7f.js", "t", true), fr("/assets/vendor-4d5e6f.js", "Cd", true))},
		{"(1b) minified: different errors in functions e,t vs n,r of one bundle", false,
			web("x is undefined", fr("/assets/index-3f2a1c.js", "e", true), fr("/assets/index-3f2a1c.js", "t", true)),
			web("y is not a function", fr("/assets/index-3f2a1c.js", "n", true), fr("/assets/index-3f2a1c.js", "r", true))},
		{"(3b) Dart: postgrest/supabase/gotrue above visits vs profile (list only, no main.dart rule)", false,
			web("PostgrestException", fr("package:postgrest/src/a.dart", "A.then", true), fr("package:supabase/src/b.dart", "B.rpc", true), fr("package:gotrue/src/c.dart", "C.refresh", true), fr("package:zasri/visits.dart", "Visits.load", true), fr("package:zasri/main.dart", "main", true)),
			web("PostgrestException", fr("package:postgrest/src/a.dart", "A.then", true), fr("package:supabase/src/b.dart", "B.rpc", true), fr("package:gotrue/src/c.dart", "C.refresh", true), fr("package:zasri/profile.dart", "Profile.load", true), fr("package:zasri/main.dart", "main", true))},
		{"(3c) Dart: same failure with and without a trailing main.dart frame and a sibling app_core frame", true,
			web("PostgrestException", fr("package:postgrest/src/a.dart", "A.then", true), fr("package:app/cart.dart", "Cart.load", true), fr("package:app_core/api.dart", "Api.get", true), fr("package:app/a.dart", "a", true), fr("package:app/b.dart", "b", true), fr("package:app/c.dart", "c", true)),
			web("PostgrestException", fr("package:postgrest/src/a.dart", "A.then", true), fr("package:app/cart.dart", "Cart.load", true), fr("package:app_core/api.dart", "Api.get", true), fr("package:app/a.dart", "a", true), fr("package:app/b.dart", "b", true), fr("package:app/c.dart", "c", true), fr("package:app/main.dart", "main", true))},
		{"(2c) two callers behind one wrapper, no source map: same file, same message, real names", false,
			web("Request failed", fr("/static/js/api.js", "request", true), fr("/static/js/api.js", "loadCart", true)),
			web("Request failed", fr("/static/js/api.js", "request", true), fr("/static/js/api.js", "loadFeed", true))},
		{"(4) same bug, symbolicated vs not -> still two until the retry resolves it (not decided by Compute)", false,
			web("x is undefined", fr("/assets/index-3f2a1c.js", "e", true)),
			web("x is undefined", func() fingerprint.StackFrame {
				f := fr("src/cart/total.ts", "computeTotal", true)
				f.Symbolicated = true
				return f
			}())},
		{"line numbers only differ", true,
			web("boom", fr("/src/a.ts", "run", true)),
			web("boom", func() fingerprint.StackFrame { f := fr("/src/a.ts", "run", true); f.Line = 999; return f }())},
	}
}

func TestGroupingV2Evidence(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(sdkJSFixtureDir, "*.json"))
	require.NoError(t, err)
	paths = append(paths, "../fixtures/events/sdk-go-contract-event.json", "../fixtures/fingerprints/normalizable-class.json")
	sort.Strings(paths)
	var corpus []evidenceEvent
	for _, p := range paths {
		corpus = append(corpus, evidenceFromFixture(t, p))
	}
	for _, n := range []string{"flutter-typeerror-unhandled.json", "flutter-dio-error.json"} {
		corpus = append(corpus, evidenceFromSentry(t, n))
	}

	limits := []int{3, 5, 8}
	byProject := map[string][]evidenceEvent{}
	for _, e := range corpus {
		byProject[e.project] = append(byProject[e.project], e)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n| corpus | events | v1 issues | v2 N=3 | v2 N=5 | v2 N=8 |\n|---|---|---|---|---|---|\n")
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		evs := byProject[p]
		v1 := map[string]bool{}
		v2 := map[int]map[string]bool{3: {}, 5: {}, 8: {}}
		for _, e := range evs {
			v1[v1Fingerprint(e.cfg)] = true
			for _, n := range limits {
				v2[n][fingerprint.ComputeWithFrameLimit(e.cfg, n)] = true
			}
		}
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %d | %d |\n", p, len(evs), len(v1), len(v2[3]), len(v2[5]), len(v2[8]))
	}

	// Every merge or split between v1 and v2(N=5) on the fixtures: pairs of events whose
	// same-issue verdict differs.
	sb.WriteString("\nfixture pairs whose verdict differs between v1 and v2 (N=5):\n")
	changed := 0
	for i := range corpus {
		for j := i + 1; j < len(corpus); j++ {
			a, b := corpus[i], corpus[j]
			was := v1Fingerprint(a.cfg) == v1Fingerprint(b.cfg)
			now := fingerprint.ComputeWithFrameLimit(a.cfg, 5) == fingerprint.ComputeWithFrameLimit(b.cfg, 5)
			if was != now {
				changed++
				verdict := "MERGE"
				if was {
					verdict = "SPLIT"
				}
				fmt.Fprintf(&sb, "  %s: %s <-> %s\n", verdict, a.name, b.name)
			}
		}
	}
	if changed == 0 {
		sb.WriteString("  (none)\n")
	}

	sb.WriteString("\nnamed failure modes (expected: same issue? / v1 says / v2 N=3,5,8 say):\n")
	wrong := map[string]int{}
	for _, p := range evidencePairs() {
		v1s := v1Fingerprint(p.a) == v1Fingerprint(p.b)
		line := fmt.Sprintf("  %-95s want same=%-5v v1 same=%-5v v2:", p.name, p.same, v1s)
		for _, n := range limits {
			s := fingerprint.ComputeWithFrameLimit(p.a, n) == fingerprint.ComputeWithFrameLimit(p.b, n)
			line += fmt.Sprintf(" N=%d same=%-5v", n, s)
			if s != p.same {
				wrong[fmt.Sprint(n)]++
			}
		}
		if v1s != p.same {
			wrong["v1"]++
		}
		sb.WriteString(line + "\n")
	}
	fmt.Fprintf(&sb, "\nwrong verdicts of %d named cases: v1=%d N=3=%d N=5=%d N=8=%d\n", len(evidencePairs()), wrong["v1"], wrong["3"], wrong["5"], wrong["8"])
	t.Log(sb.String())

	require.Zero(t, wrong["5"], "v2 at the chosen limit must get every named failure mode right")
	require.Equal(t, 5, fingerprint.MaxFrames, "the evidence document argues for 5; change both together")
}

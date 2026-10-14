package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

func sum16(input string) string {
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:16]
}

func web(msg string, frames ...StackFrame) FingerprintConfig {
	return FingerprintConfig{Environment: "production", ErrorClass: "TypeError", Message: msg,
		Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms, Stacktrace: frames}
}

func fr(file, fn string) StackFrame {
	return StackFrame{File: file, Line: 7, Function: fn, InApp: true}
}

// sfr is a frame symbolication rewrote: its function is a real name and an input.
func sfr(file, fn string) StackFrame {
	f := fr(file, fn)
	f.Symbolicated = true
	return f
}

// Golden: the v2 input, byte for byte. The expected hash is of a literal input string, not of a
// second call into Compute, so a change to any separator, ordering or token fails here.
func TestCompute_GoldenInputs(t *testing.T) {
	cases := []struct {
		name  string
		cfg   FingerprintConfig
		input string
	}{
		{"in-app frames, no message", web("ignored", sfr("/src/a.ts", "run"), sfr("/src/b.ts", "goNext")),
			"v2|production|TypeError|/src/a.ts:run|/src/b.ts:goNext"},
		{"stackless: the normalised message joins", web("Order 42 not found for 'ann'"),
			"v2|production|TypeError|msg=Order <N> not found for <STR>"},
		{"wrapper on top: frames then message", web("Request failed", sfr("/src/lib/httpClient.ts", "request"), sfr("/src/cart.ts", "loadCart")),
			"v2|production|TypeError|/src/lib/httpClient.ts:request|/src/cart.ts:loadCart|msg=Request failed"},
		{"library frames skipped though flagged in_app", FingerprintConfig{Environment: "p", ErrorClass: "E", Stacktrace: []StackFrame{
			fr("package:dio/src/dio.dart", "Dio.get"), fr("dart:async/zone.dart", "run"), fr("package:app/visits.dart", "load")}},
			"v2|p|E|package:app/visits.dart:load"},
		{"unsymbolicated gated frame: minified name folds, message joins", web("Boom 7", fr("/assets/index-3f2a1c.js", "e")),
			"v2|production|TypeError|/assets/index-*.js:*|msg=Boom <N>"},
		{"unsymbolicated gated frame: a real name stays", web("Boom 7", fr("/static/js/api.js", "loadCart")),
			"v2|production|TypeError|/static/js/api.js:loadCart|msg=Boom <N>"},
		{"symbolicated frame keeps its name", web("m", StackFrame{File: "src/cart.ts", Function: "e", InApp: true, Symbolicated: true}),
			"v2|production|TypeError|src/cart.ts:e"},
		{"Dart: app package from main.dart, other packages skipped", FingerprintConfig{Environment: "p", ErrorClass: "E", Stacktrace: []StackFrame{
			fr("package:postgrest/src/a.dart", "A.run"), fr("package:zasri/visits.dart", "load"), fr("package:zasri/main.dart", "main")}},
			"v2|p|E|package:zasri/visits.dart:load|package:zasri/main.dart:main"},
		{"only the first MaxFrames frames", web("m", sfr("/1", "a1x"), sfr("/2", "a2x"), sfr("/3", "a3x"), sfr("/4", "a4x"), sfr("/5", "a5x"), sfr("/6", "a6x")),
			"v2|production|TypeError|/1:a1x|/2:a2x|/3:a3x|/4:a4x|/5:a5x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := Compute(c.cfg), sum16(c.input); got != want {
				t.Fatalf("Compute = %s, want %s (input %q)", got, want, c.input)
			}
		})
	}
	if AlgorithmVersion != "v2" || MaxFrames != 5 {
		t.Fatalf("version/limit changed: %s/%d", AlgorithmVersion, MaxFrames)
	}
}

func TestCompute_CustomFingerprintStaysVerbatimAndUnversioned(t *testing.T) {
	cfg := web("m", fr("/a.ts", "f"))
	cfg.CustomFingerprint = "checkout-timeout"
	if got := Compute(cfg); got != "checkout-timeout" {
		t.Fatalf("got %q", got)
	}
}

func TestCompute_Properties(t *testing.T) {
	same := func(name string, a, b FingerprintConfig) {
		t.Helper()
		if Compute(a) != Compute(b) {
			t.Errorf("%s: want the same issue", name)
		}
	}
	differ := func(name string, a, b FingerprintConfig) {
		t.Helper()
		if Compute(a) == Compute(b) {
			t.Errorf("%s: want two issues", name)
		}
	}

	moved := fr("/src/a.ts", "run")
	moved.Line, moved.Function = 900, "run"
	same("line and column changes", web("m", fr("/src/a.ts", "run")), web("m", moved))

	same("build-hash rename of a chunk",
		web("m", fr("/assets/index-3f2a1c.js", "boot")), web("m", fr("/assets/index-9d8e7f.js", "boot")))
	same("minified function rename (e -> t, a.b -> c.d)",
		web("m", fr("/assets/index-3f2a1c.js", "e"), fr("/assets/v-a1b2c3.js", "a.b")),
		web("m", fr("/assets/index-9d8e7f.js", "t"), fr("/assets/v-4d5e6f.js", "c.d")))
	// R1: a minified name carries no identity, so two different errors in e,t vs n,r of one bundle
	// must not merge on the strength of the shared file; their messages tell them apart.
	differ("different errors in different minified functions of one bundle",
		web("x is undefined", fr("/assets/index-3f2a1c.js", "e"), fr("/assets/index-3f2a1c.js", "t")),
		web("y is not a function", fr("/assets/index-3f2a1c.js", "n"), fr("/assets/index-3f2a1c.js", "r")))
	same("the same error across a rename of every minified function",
		web("x is undefined", fr("/assets/index-3f2a1c.js", "e"), fr("/assets/index-3f2a1c.js", "t")),
		web("x is undefined", fr("/assets/index-9d8e7f.js", "n"), fr("/assets/index-9d8e7f.js", "r")))

	differ("stackless events with different messages", web("Payment declined"), web("Cart is empty"))
	same("same failure, different ids/numbers/quoted strings in the message",
		web("Order 123456789 for 'ann' (0xdeadbeef) not found"), web("Order 987654321 for 'bo' (0xcafebabe) not found"))

	wrapper := func(caller string) FingerprintConfig {
		return web("Request failed",
			sfr("/src/lib/httpClient.ts", "request"), sfr("/src/lib/httpClient.ts", "get"),
			sfr("/src/lib/httpClient.ts", "withRetry"), sfr("/src/lib/httpClient.ts", "send"),
			sfr("/src/"+caller+".ts", caller))
	}
	differ("two callers behind the same wrapper frames", wrapper("loadCart"), wrapper("loadFeed"))

	// Library frames flagged in_app do not decide: a different library frame under the same app
	// frame is the same failure; a different app frame is not.
	dart := func(lib, app string) FingerprintConfig {
		return FingerprintConfig{Environment: "p", ErrorClass: "DioException", Stacktrace: []StackFrame{
			fr("package:dio/"+lib+".dart", lib), fr("package:app/"+app+".dart", app)}}
	}
	same("library frame churn under one app frame", dart("fetch", "visits"), dart("dispatch", "visits"))
	differ("different app frame behind the same library frames", dart("fetch", "visits"), dart("fetch", "profile"))

	// R5: a Dart frame is library code iff its package is on the deterministic list, whatever else
	// is in the stack (no main.dart heuristic).
	supa := func(app string) FingerprintConfig {
		return FingerprintConfig{Environment: "p", ErrorClass: "PostgrestException", Stacktrace: []StackFrame{
			fr("package:postgrest/src/postgrest_builder.dart", "PostgrestBuilder.then"),
			fr("package:supabase/src/supabase_client.dart", "SupabaseClient.rpc"),
			fr("package:gotrue/src/gotrue_client.dart", "GoTrueClient.refresh"),
			fr("package:zasri/"+app+".dart", app), fr("package:zasri/main.dart", "main")}}
	}
	differ("postgrest/supabase/gotrue frames above visits vs profile", supa("visits"), supa("profile"))
	same("listed library frame churn under one app frame", supa("visits"), func() FingerprintConfig {
		c := supa("visits")
		c.Stacktrace[0].File = "package:storage_client/x.dart"
		return c
	}())

	differ("environment still separates", web("m", sfr("/a.ts", "f")), func() FingerprintConfig {
		c := web("m", sfr("/a.ts", "f"))
		c.Environment = "staging"
		return c
	}())
}

func TestIsLibraryFrame(t *testing.T) {
	lib := []string{
		"dart:async/zone.dart", "dart:core/errors.dart", "package:flutter/src/widgets/framework.dart", "package:dio/src/dio.dart",
		"/usr/local/go/src/net/http/client.go", "net/http/client.go", "runtime/panic.go", "database/sql/sql.go",
		"/home/u/go/pkg/mod/github.com/jackc/pgx/v5@v5.5.0/conn.go", "github.com/jackc/pgx/v5@v5.5.0/conn.go",
		"/app/vendor/github.com/x/y/z.go", "vendor/github.com/x/y/z.go", "package:supabase_flutter/src/x.dart", "package:postgrest/src/y.dart", "/app/node_modules/react/index.js",
	}
	own := []string{
		"package:daya/main.dart", "app/orders.go", "github.com/acme/shop/orders.go", "/srv/app/main.go",
		"/src/components/Cart.tsx", "sentinel/internal/x.go", "internal/svc/x.go", "vendor/x.rb", "src/vendor/x.ts", "app/vendor/lib.php",
	}
	for _, f := range lib {
		if !isLibraryFrame(StackFrame{File: f}) {
			t.Errorf("%s should be library code", f)
		}
	}
	for _, f := range own {
		if isLibraryFrame(StackFrame{File: f}) {
			t.Errorf("%s should be the app's own", f)
		}
	}
}

func TestNormalizeMessage(t *testing.T) {
	cases := map[string]string{
		"User 4242 logged in":                          "User <N> logged in",
		"id 123e4567-e89b-12d3-a456-426614174000 gone": "id <UUID> gone",
		`bad value "abc" and 'def'`:                    "bad value <STR> and <STR>",
		"at 0xdeadbeef01":                              "at <HEX_ADDR>",
		"  spaced   out ":                              "spaced out",
	}
	for in, want := range cases {
		if got := NormalizeMessage(in); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", in, got, want)
		}
	}
	long := make([]rune, 1000)
	for i := range long {
		long[i] = 'x'
	}
	if n := len([]rune(NormalizeMessage(string(long)))); n != maxMessageRunes {
		t.Errorf("length %d, want %d", n, maxMessageRunes)
	}
}

// The Dart library decision is stack-independent: the same failure hashes the same with and without
// a `package:<app>/main.dart` frame, and a sibling app package (app_core) is app code in both.
func TestCompute_DartDecisionIsStackIndependent(t *testing.T) {
	// Five app frames fill MaxFrames, so a trailing main.dart frame is not itself hashed: the only
	// thing it could change is the library/app decision, which must not depend on it.
	core := []StackFrame{
		{File: "package:postgrest/src/builder.dart", Function: "execute", InApp: true},
		{File: "package:app/cart_repo.dart", Function: "CartRepo.load", InApp: true},
		{File: "package:app_core/api.dart", Function: "Api.get", InApp: true},
		{File: "package:app/a.dart", Function: "a", InApp: true},
		{File: "package:app/b.dart", Function: "b", InApp: true},
		{File: "package:app/c.dart", Function: "c", InApp: true},
	}
	withMain := append(append([]StackFrame{}, core...), StackFrame{File: "package:app/main.dart", Function: "main", InApp: true})
	hash := func(frames []StackFrame) string {
		return Compute(FingerprintConfig{ErrorClass: "PostgrestException", Message: "boom", Stacktrace: frames})
	}
	if a, b := hash(core), hash(withMain); a != b {
		t.Fatalf("main.dart in the stack changed the hash: %s vs %s", a, b)
	}
	// The sibling package frame is app code with or without main.dart: dropping it changes identity.
	noSibling := append([]StackFrame{core[0], core[1]}, core[3:]...)
	if hash(core) == hash(noSibling) {
		t.Fatal("a sibling app package (app_core) must be hashed, with or without main.dart")
	}
}

func TestIsLibraryFrame_DartIsPerPackage(t *testing.T) {
	if !isLibraryFrame(StackFrame{File: "package:supabase_flutter/src/x.dart"}) {
		t.Error("supabase_flutter is on the library list")
	}
	for _, f := range []string{"package:zasri/visits.dart", "package:app_core/api.dart", "package:zasri/main.dart"} {
		if isLibraryFrame(StackFrame{File: f}) {
			t.Errorf("%s is app code", f)
		}
	}
}

// gv2-dart-library-denylist, coordinator decision: contract §1.3 is narrowed to a maintained library
// list (docs/audits/grouping-v2-evidence.md §6). The reviewer's probe holds for LISTED dependencies: five
// frames from the supabase family above one app frame, no main.dart, no release signal. The app frame
// decides the identity, and churn in the dependency frames does not split the issue.
func TestCompute_DartListedDependenciesProperty(t *testing.T) {
	cfg := func(release, appFile, variant string) FingerprintConfig {
		return FingerprintConfig{Environment: "p", ErrorClass: "PostgrestException", Platform: "dart", Stacktrace: []StackFrame{
			fr("package:postgrest/src/postgrest_builder.dart", "PostgrestBuilder.then"),
			fr("package:postgrest/src/"+variant+".dart", "Filter.eq"),
			fr("package:supabase/src/supabase_client.dart", "SupabaseClient.rpc"),
			fr("package:gotrue/src/gotrue_client.dart", "GoTrueClient.refresh"),
			fr("package:gotrue/src/"+variant+".dart", "Api."+variant),
			fr("package:zasri/"+appFile+".dart", "load")}}
	}
	if Compute(cfg("", "visits", "a")) == Compute(cfg("", "profile", "a")) {
		t.Error("two different app frames behind the same listed dependency frames share a fingerprint")
	}
	if Compute(cfg("", "visits", "a")) != Compute(cfg("", "visits", "b")) {
		t.Error("listed dependency churn above one app frame split the issue")
	}
}

// The ACCEPTED LIMIT of the list (coordinator decision): a dependency nobody listed is treated as app
// code. Here five unlisted-package frames fill the frame budget above one app frame, so two different
// app frames hash ALIKE (§1.3's over-merge), and the same unlisted frames as the only frame source are
// still hashed. Listing the package (as supabase was) is the fix; if this test goes red because the
// rule gained an own-package signal, update the evidence note's §6 decision with it.
func TestCompute_DartUnlistedDependencyIsAcceptedLimit(t *testing.T) {
	cfg := func(appFile string) FingerprintConfig {
		st := []StackFrame{}
		for _, f := range []string{"a", "b", "c", "d", "e"} {
			st = append(st, fr("package:some_unlisted_sdk/src/"+f+".dart", "Sdk."+f))
		}
		st = append(st, fr("package:zasri/"+appFile+".dart", "load"))
		return FingerprintConfig{Environment: "p", ErrorClass: "SdkException", Platform: "dart", Stacktrace: st}
	}
	if Compute(cfg("visits")) != Compute(cfg("profile")) {
		t.Error("the accepted limit disappeared: unlisted dependency frames no longer fill the budget")
	}
	only := func(file string) FingerprintConfig {
		return FingerprintConfig{Environment: "p", ErrorClass: "E", Platform: "dart",
			Stacktrace: []StackFrame{fr("package:some_unlisted_sdk/src/"+file+".dart", "f")}}
	}
	if Compute(only("a")) == Compute(only("b")) {
		t.Error("an unlisted dependency as the only frame source must still be hashed")
	}
}

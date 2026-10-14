package fingerprint

import (
	"path"
	"regexp"
	"strings"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
)

// AlgorithmVersion is the version of the server-side grouping rules (sentinel-grouping-v2 §1.5). It
// is the first token of every computed hash input, so a change to the rules is recognisable and
// schedulable instead of silently re-splitting every issue. Bump it, and only it, whenever Compute's
// input changes; a client-supplied fingerprint is stored verbatim and never carries it.
const AlgorithmVersion = "v2"

const (
	// MaxFrames is how many frames of the stack are hashed (line numbers excluded). The choice of 5
	// over 3 and 8 is argued from data in docs/audits/grouping-v2-evidence.md.
	MaxFrames = 5

	// maxMessageRunes bounds the normalised message that joins the input when the stack cannot
	// decide, so a long message with a variable tail does not split an issue.
	maxMessageRunes = 256
)

var (
	quotedRegex = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`")
	hexRunRegex = regexp.MustCompile(`\b(?:0x[0-9a-fA-F]+|[0-9a-fA-F]{8,})\b`)
	digitsRegex = regexp.MustCompile(`\d+`)
	spacesRegex = regexp.MustCompile(`\s+`)
)

// NormalizeMessage folds the parts of a message that vary between occurrences of one failure: the
// existing normaliser's UUIDs, long ids, addresses, emails, versions and user paths, then quoted
// strings, any remaining hex run, and every remaining run of digits. Whitespace is collapsed and the
// result bounded.
func NormalizeMessage(msg string) string {
	msg = normalizer.NewNormalizer().NormalizeString(msg)
	msg = quotedRegex.ReplaceAllString(msg, "<STR>")
	msg = hexRunRegex.ReplaceAllString(msg, "<HEX>")
	msg = digitsRegex.ReplaceAllString(msg, "<N>")
	msg = strings.TrimSpace(spacesRegex.ReplaceAllString(msg, " "))
	if r := []rune(msg); len(r) > maxMessageRunes {
		msg = string(r[:maxMessageRunes])
	}
	return msg
}

// goStdlibRoots are the top-level import paths of the Go standard library that can appear as the
// first segment of a `-trimpath` frame (`net/http/client.go`). A module path that collides with one
// of these is not a realistic application module. `internal` and `vendor` are deliberately absent:
// a module-relative `internal/svc/x.go` is app code (the stdlib's own internal/ packages never
// reach a user-visible stack as the top-level segment worth skipping).
var goStdlibRoots = map[string]bool{
	"archive": true, "bufio": true, "bytes": true, "compress": true, "container": true, "context": true,
	"crypto": true, "database": true, "debug": true, "embed": true, "encoding": true, "errors": true,
	"expvar": true, "flag": true, "fmt": true, "go": true, "hash": true, "html": true, "image": true,
	"index": true, "io": true, "iter": true, "log": true, "maps": true, "math": true,
	"mime": true, "net": true, "os": true, "path": true, "plugin": true, "reflect": true, "regexp": true,
	"runtime": true, "slices": true, "sort": true, "strconv": true, "strings": true, "sync": true,
	"syscall": true, "testing": true, "text": true, "time": true, "unicode": true, "unique": true,
	"unsafe": true,
}

// dartLibraryPackages are widely used third-party and framework Dart packages. The decision is
// STACK-INDEPENDENT on purpose: whether a `package:` frame is library code depends only on its own
// package name, never on what else is in the stack, so one bug hashes the same whether or not a
// `package:<app>/main.dart` frame happens to be present (a sibling app package such as app_core is
// app code in both cases). A `package:` frame from one of these is library code even when the SDK
// flagged it in_app (observed: dio).
var dartLibraryPackages = map[string]bool{
	"flutter": true, "flutter_web_plugins": true, "flutter_test": true, "dio": true, "http": true,
	"http_parser": true, "provider": true, "riverpod": true, "flutter_riverpod": true, "bloc": true,
	"flutter_bloc": true, "get": true, "get_it": true, "rxdart": true, "async": true, "collection": true,
	"stream_transform": true, "sentry": true, "sentry_flutter": true, "sentinel_flutter": true,
	"sqflite": true, "drift": true, "hive": true, "shared_preferences": true, "firebase_core": true,
	"cloud_firestore": true, "firebase_auth": true, "go_router": true, "retrofit": true, "chopper": true,
	"freezed_annotation": true, "json_annotation": true, "equatable": true, "meta": true, "path_provider": true,
	"url_launcher": true, "cached_network_image": true, "connectivity_plus": true, "web_socket_channel": true,
	// The supabase family and its transitive packages (named case 3b in the evidence note).
	"supabase": true, "supabase_flutter": true, "postgrest": true, "gotrue": true, "realtime_client": true,
	"storage_client": true, "functions_client": true, "yet_another_json_isolate": true, "mutex": true,
	// Other widely used Flutter plugins.
	"package_info_plus": true, "device_info_plus": true, "flutter_secure_storage": true,
	"firebase_messaging": true, "firebase_analytics": true, "firebase_crashlytics": true,
	"firebase_storage": true, "firebase_remote_config": true,
}

// dartPackage returns the package of a `package:<pkg>/<path>` file, or "" for anything else.
func dartPackage(file string) string {
	if !strings.HasPrefix(file, "package:") {
		return ""
	}
	pkg := strings.TrimPrefix(file, "package:")
	if i := strings.IndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[:i]
	}
	return pkg
}

// isLibraryFrame reports whether a frame is clearly library or platform code whatever the SDK
// flagged: Dart `dart:` frames and `package:` frames of the dartLibraryPackages list, Go standard-library and module-cache or vendored paths
// (including `-trimpath` forms), and `node_modules`.
func isLibraryFrame(f StackFrame) bool {
	file := f.File
	switch {
	case strings.HasPrefix(file, "dart:"):
		return true
	case strings.HasPrefix(file, "package:"):
		return dartLibraryPackages[dartPackage(file)]
	}
	slashed := strings.ReplaceAll(file, "\\", "/")
	if strings.Contains(slashed, "/node_modules/") || strings.HasPrefix(slashed, "node_modules/") {
		return true
	}
	isGo := strings.HasSuffix(slashed, ".go") || strings.Contains(slashed, ".go:")
	if strings.Contains(slashed, "/pkg/mod/") || strings.Contains(slashed, "/go/src/") && strings.Contains(slashed, "/usr/") {
		return true
	}
	// A vendor directory is library code only for Go; `src/vendor/x.ts` or a PHP/Ruby vendor/ is not
	// ours to call a library.
	if isGo && (strings.Contains(slashed, "/vendor/") || strings.HasPrefix(slashed, "vendor/")) {
		return true
	}
	if isGo {
		// `-trimpath` forms: a module-cache dependency keeps its `@version` directory; stdlib
		// loses GOROOT and starts at its import path.
		if strings.Contains(slashed, "@v") {
			return true
		}
		first := slashed
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		if !strings.Contains(first, ".") && goStdlibRoots[first] {
			return true
		}
		if strings.Contains(slashed, "/usr/local/go/src/") || strings.Contains(slashed, "/usr/lib/go") ||
			strings.Contains(slashed, "/go/src/runtime/") || strings.HasPrefix(slashed, "$GOROOT/") {
			return true
		}
	}
	return false
}

// wrapperBaseNames and wrapperFunctions are the short, code-level default list of frames that sit
// between many callers and the failure: HTTP clients, interceptors, middleware, loggers and
// error-reporting helpers. When the top usable frame is one, the stack alone says "something went
// wrong via the wrapper", so the normalised message joins the input.
var wrapperBaseNames = map[string]bool{
	"logger": true, "log": true, "logging": true, "errorhandler": true, "error-handler": true,
	"error_handler": true, "interceptor": true, "interceptors": true,
	"httpclient": true, "http-client": true, "http_client": true, "apiclient": true, "api-client": true,
	"api_client": true, "fetcher": true, "request": true, "requests": true, "axios": true, "client": true,
	"wrapper": true,
}

var wrapperFunctions = map[string]bool{
	"log": true, "logerror": true, "error": true, "warn": true, "captureexception": true,
	"capturemessage": true, "handleerror": true, "reporterror": true, "request": true,
	"fetch": true, "send": true, "wrapper": true, "middleware": true, "interceptor": true,
	"onerror": true,
}

func isWrapperFrame(f StackFrame) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(f.File, "\\", "/")))
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if wrapperBaseNames[base] {
		return true
	}
	fn := strings.ToLower(f.Function)
	if i := strings.LastIndexAny(fn, "./#:"); i >= 0 {
		fn = fn[i+1:]
	}
	return wrapperFunctions[fn]
}

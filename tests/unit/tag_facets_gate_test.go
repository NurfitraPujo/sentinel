package unit

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sentinel-issue-tag-facets spec §10.1 "The guard" (contract decision 5): the tag-facets proofs run under
// `task test-integration` and cannot skip. This test keeps it that way, statically, with no exec:
//
//   - no tests/integration/tag_facets_*.go file names requireInfra, newPostgresPool, newEIDStoreAndPoolFromEnv,
//     migTestDB, t.Skip/Skipf/SkipNow or testing.Short (a go/scanner pass: comments and strings do not count);
//   - exactly one tagFacetsDB exists, its body names none of those, and it calls no package-local function
//     declared outside a tag_facets_* file (so a skip cannot hide one call away);
//   - both dashboard suites throw under TAG_FACETS_INTEGRATION_REQUIRED and scope the check query ($1); the
//     retention suite takes the shared retention advisory lock; Taskfile.yml's test-integration runs
//     scripts/test-dashboard-pg.sh; ci.yml's dashboard job sets the flag; the script runs both suites with it;
//   - retention's route budgets fit the curl timeout and the stale bound (spec §5), and the compose loop's
//     timeout equals the chart's;
//   - the sentinel-retention alert group exists and names only outcomes metrics.ts declares.

var tfRoot = filepath.Join("..", "..")

// tfForbidden are the helpers that reach requireInfra (and so t.Skipf without SENTINEL_E2E=1).
var tfForbidden = []string{"requireInfra", "newPostgresPool", "newEIDStoreAndPoolFromEnv", "migTestDB"}

func tfIsForbidden(name string) bool {
	for _, f := range tfForbidden {
		if name == f {
			return true
		}
	}
	return false
}

// tfSkipTokens reports every forbidden identifier and skip call in Go source, by token (so a comment or a
// string mentioning them does not count).
func tfSkipTokens(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var found []string
	var prev1, prev2 string // the two previous tokens' text, most recent first
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		text := lit
		if text == "" {
			text = tok.String()
		}
		if tok == token.IDENT {
			switch {
			case tfIsForbidden(lit):
				found = append(found, fset.Position(pos).String()+": "+lit)
			case prev1 == "." && (lit == "Skip" || lit == "Skipf" || lit == "SkipNow"):
				found = append(found, fset.Position(pos).String()+": "+prev2+"."+lit)
			case prev1 == "." && prev2 == "testing" && lit == "Short":
				found = append(found, fset.Position(pos).String()+": testing.Short")
			}
		}
		prev2, prev1 = prev1, text
	}
	return found
}

func tfIntegrationFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(tfRoot, "tests", "integration", "tag_facets_*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no tests/integration/tag_facets_*.go files")
	return files
}

func TestTagFacetsGate_NoProofFileCanSkip(t *testing.T) {
	files := tfIntegrationFiles(t)
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		require.Emptyf(t, tfSkipTokens(t, f, src),
			"%s: a tag-facets proof must never skip (decision 5); get the database from tagFacetsDB", f)
	}
	// The helper and the migration proofs (plan addition A2) are among the files this guard reads.
	require.Contains(t, names, "tag_facets_db_test.go")
	require.Contains(t, names, "tag_facets_migration_test.go")
}

func TestTagFacetsGate_HelperCannotSkipOneCallAway(t *testing.T) {
	dir := filepath.Join(tfRoot, "tests", "integration")
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	// Every package-level function of tests/integration, and the file that declares it.
	declaredIn := map[string]string{}
	var helpers []*ast.FuncDecl
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		require.NoError(t, err, e.Name())
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			declaredIn[fd.Name.Name] = e.Name()
			if fd.Name.Name == "tagFacetsDB" {
				helpers = append(helpers, fd)
			}
		}
	}
	require.Len(t, helpers, 1, "tests/integration must declare exactly one tagFacetsDB")
	require.True(t, strings.HasPrefix(declaredIn["tagFacetsDB"], "tag_facets_"), "tagFacetsDB must live in a tag_facets_* file")

	var problems []string
	ast.Inspect(helpers[0].Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if tfIsForbidden(x.Name) {
				problems = append(problems, fset.Position(x.Pos()).String()+": names "+x.Name)
			}
		case *ast.SelectorExpr:
			if s := x.Sel.Name; s == "Skip" || s == "Skipf" || s == "SkipNow" {
				problems = append(problems, fset.Position(x.Pos()).String()+": calls "+s)
			}
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "testing" && x.Sel.Name == "Short" {
				problems = append(problems, fset.Position(x.Pos()).String()+": calls testing.Short")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok {
				if file, local := declaredIn[id.Name]; local && !strings.HasPrefix(file, "tag_facets_") {
					problems = append(problems, fset.Position(x.Pos()).String()+": calls "+id.Name+", declared in "+file)
				}
			}
		}
		return true
	})
	require.Empty(t, problems, "tagFacetsDB must provision and ping its own database and t.Fatalf on error, calling only tag_facets_* helpers")
}

func tfRead(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{tfRoot}, rel...)...))
	require.NoError(t, err)
	return string(b)
}

const (
	tfRetentionSuite = "src/lib/server/retention.tag-facets.integration.test.ts"
	tfSearchSuite    = "src/lib/server/search.tag-facets.integration.test.ts"
)

var (
	tfRequiredThrow = regexp.MustCompile(`(?s)process\.env\.TAG_FACETS_INTEGRATION_REQUIRED === '1'.*?if \(required && [^)]*\)\) \{\s*throw new Error\(`)
	tfScopedCheck   = regexp.MustCompile(`sql\.unsafe\(TAG_FACETS_CHECK_SQL, \[[^\]]+\]\)`)
)

func TestTagFacetsGate_DashboardSuitesCannotSkip(t *testing.T) {
	for _, suite := range []string{tfRetentionSuite, tfSearchSuite} {
		src := tfRead(t, "apps", "dashboard-web", suite)
		require.Regexpf(t, tfRequiredThrow, src, "%s must throw at module load under TAG_FACETS_INTEGRATION_REQUIRED=1", suite)
		require.Regexpf(t, tfScopedCheck, src, "%s must pass the check query a scope ($1)", suite)
	}
	retention := tfRead(t, "apps", "dashboard-web", tfRetentionSuite)
	require.Contains(t, retention, "pg_advisory_lock(hashtext('sentinel.retention.integration'))",
		"the retention suite must serialise with the other retention suites")

	// tfr-gate-dashboard-proofs-never-run: the go test line used to run as its own Taskfile.yml cmd,
	// directly ahead of ./scripts/test-dashboard-pg.sh, so a failing go test aborted the task before the
	// dashboard script ever ran. Both now run from inside one script that always runs both and aggregates
	// their exit codes, so Taskfile.yml itself must no longer contain either command directly.
	taskfile := readHelmValues(t, filepath.Join(tfRoot, "Taskfile.yml"))
	ti, ok := yamlMap(taskfile, "tasks", "test-integration")
	require.True(t, ok, "Taskfile.yml: tasks.test-integration missing")
	cmds, ok := ti["cmds"].([]any)
	require.True(t, ok, "Taskfile.yml: tasks.test-integration.cmds is not a list")
	gate := -1
	for i, c := range cmds {
		line, _ := c.(string)
		require.NotContainsf(t, line, "go test ./tests/integration/",
			"Taskfile.yml test-integration must not run go test directly; delegate to scripts/test-integration-gate.sh so a failure there cannot skip the dashboard proofs")
		require.NotEqualf(t, "./scripts/test-dashboard-pg.sh", strings.TrimSpace(line),
			"Taskfile.yml test-integration must not run ./scripts/test-dashboard-pg.sh directly; delegate to scripts/test-integration-gate.sh")
		if strings.TrimSpace(line) == "./scripts/test-integration-gate.sh" {
			gate = i
		}
	}
	require.GreaterOrEqual(t, gate, 0, "Taskfile.yml test-integration must run ./scripts/test-integration-gate.sh")

	gateSh := tfRead(t, "scripts", "test-integration-gate.sh")
	require.NotRegexpf(t, `(?m)^\s*set\s+-[a-z]*e[a-z]*\b`, gateSh,
		"scripts/test-integration-gate.sh must not use errexit (set -e): it must run go test AND the dashboard script regardless of either one's exit code")
	require.Contains(t, gateSh, "go test ./tests/integration/", "scripts/test-integration-gate.sh must run the Go integration suite")
	require.Contains(t, gateSh, "./scripts/test-dashboard-pg.sh", "scripts/test-integration-gate.sh must run the dashboard proofs")
	// Code only, comment lines stripped — the header comment mentions both commands by name too, and a
	// plain substring search over the whole file would match those mentions instead of the real calls.
	var codeLines []string
	for _, line := range strings.Split(gateSh, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		codeLines = append(codeLines, line)
	}
	code := strings.Join(codeLines, "\n")
	goIdx := strings.Index(code, "go test ./tests/integration/")
	dashIdx := strings.Index(code, "./scripts/test-dashboard-pg.sh")
	require.True(t, goIdx >= 0 && dashIdx >= 0 && dashIdx > goIdx, "the dashboard script must be invoked unconditionally after the go test line, not inside a conditional on its exit code")
	between := code[goIdx:dashIdx]
	require.NotContains(t, between, "exit", "nothing between the go test line and the dashboard script invocation may exit early on the go test result")
	require.NotContains(t, between, "&&", "the dashboard script must not be chained with && after go test (that would skip it on a go test failure)")

	ci := readHelmValues(t, filepath.Join(tfRoot, ".github", "workflows", "ci.yml"))
	env, ok := yamlMap(ci, "jobs", "dashboard", "env")
	require.True(t, ok, "ci.yml: jobs.dashboard.env missing")
	require.Equal(t, "1", env["TAG_FACETS_INTEGRATION_REQUIRED"], `ci.yml dashboard job: TAG_FACETS_INTEGRATION_REQUIRED: "1"`)

	sh := tfRead(t, "scripts", "test-dashboard-pg.sh")
	for _, want := range []string{tfRetentionSuite, tfSearchSuite, "TAG_FACETS_INTEGRATION_REQUIRED=1", "-p 127.0.0.1::5432", "-target dashboard"} {
		require.Containsf(t, sh, want, "scripts/test-dashboard-pg.sh must contain %q", want)
	}
}

// tfRunGate executes the real scripts/test-integration-gate.sh against a stubbed "go" (on PATH, so the
// script's literal `go test ...` invokes it) and a stubbed scripts/test-dashboard-pg.sh (a sibling of the
// gate script inside a throwaway ROOT, since the gate script cds to its own parent directory), each of
// which exits goExit / dashExit and writes a marker file recording that it ran. It proves the two run
// INDEPENDENTLY: both markers exist no matter which one "fails", and the gate's own exit code is non-zero
// iff either did.
func tfRunGate(t *testing.T, goExit, dashExit int) (gateExit int, goRan, dashRan bool, output string) {
	t.Helper()
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	require.NoError(t, os.MkdirAll(scriptsDir, 0o755))

	realGate := tfRead(t, "scripts", "test-integration-gate.sh")
	require.NoError(t, os.WriteFile(filepath.Join(scriptsDir, "test-integration-gate.sh"), []byte(realGate), 0o755))

	goMarker := filepath.Join(root, "go-ran")
	dashMarker := filepath.Join(root, "dash-ran")
	dashScript := "#!/usr/bin/env bash\ntouch " + dashMarker + "\nexit " + strconv.Itoa(dashExit) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(scriptsDir, "test-dashboard-pg.sh"), []byte(dashScript), 0o755))

	binDir := t.TempDir()
	goStub := "#!/usr/bin/env bash\ntouch " + goMarker + "\nexit " + strconv.Itoa(goExit) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755))

	cmd := exec.Command("bash", filepath.Join(scriptsDir, "test-integration-gate.sh"))
	cmd.Dir = scriptsDir
	// exec.Cmd keeps only the LAST value for a duplicate env key, so the stub PATH must be appended
	// after a filtered copy of the real environment, not prepended to it.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+binDir+":"+os.Getenv("PATH"))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	output = string(out)

	gateExit = 0
	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "unexpected error running test-integration-gate.sh: %v\n%s", err, output)
		gateExit = exitErr.ExitCode()
	}
	_, goErr := os.Stat(goMarker)
	_, dashErr := os.Stat(dashMarker)
	return gateExit, goErr == nil, dashErr == nil, output
}

// TestTagFacetsGate_IntegrationGateRunsBothIndependently proves, by executing the real script, the
// behaviour tfr-gate-dashboard-proofs-never-run required: a failing go test must not stop the dashboard
// script from running, and the gate must still fail overall when either step failed.
func TestTagFacetsGate_IntegrationGateRunsBothIndependently(t *testing.T) {
	cases := []struct {
		name             string
		goExit, dashExit int
		wantGateNonZero  bool
	}{
		{"both pass", 0, 0, false},
		{"go test fails, dashboard proofs still run and the gate fails", 1, 0, true},
		{"dashboard proofs fail, gate fails", 0, 1, true},
		{"both fail, gate fails", 1, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gateExit, goRan, dashRan, output := tfRunGate(t, c.goExit, c.dashExit)
			require.Truef(t, goRan, "the go test stub never ran\n%s", output)
			require.Truef(t, dashRan, "the dashboard script stub never ran (a failing go test must not skip it)\n%s", output)
			if c.wantGateNonZero {
				require.NotEqualf(t, 0, gateExit, "gate should have exited non-zero\n%s", output)
			} else {
				require.Equalf(t, 0, gateExit, "gate should have exited zero\n%s", output)
			}
		})
	}
}

// TestTagFacetsGate_DashboardFailureIsReadableByBaseline proves tfr-gate-dashboard-proofs-never-run
// (reopened): the plane's NO_WORSE comparison (waycore/baseline.py, wayexec/failures.py) finds findings by
// matching Go-test-shaped `--- FAIL: <name>` lines, so a dashboard half that fails or crashes must print
// one, or the comparison against a base revision that never ran the dashboard proofs cannot see it as new.
func TestTagFacetsGate_DashboardFailureIsReadableByBaseline(t *testing.T) {
	const marker = "--- FAIL: DashboardTagFacetsPgProofs"

	_, _, _, passOutput := tfRunGate(t, 0, 0)
	require.NotContainsf(t, passOutput, marker, "the marker must be absent when the dashboard half passes\n%s", passOutput)

	_, _, _, failOutput := tfRunGate(t, 0, 1)
	require.Containsf(t, failOutput, marker, "a failing dashboard half must print a Go-test-shaped %q line so the plane's baseline comparison can see it\n%s", marker, failOutput)

	// Also readable when the go-test half is the one that failed but the dashboard half still passed:
	// the marker must stay absent, since only the dashboard half's own failure is what it reports.
	_, _, _, goOnlyFailOutput := tfRunGate(t, 1, 0)
	require.NotContainsf(t, goOnlyFailOutput, marker, "the marker must be absent when only the go test half failed\n%s", goOnlyFailOutput)
}

// tfTSConst reads `export const NAME = 123_456;` from a TypeScript module.
func tfTSConst(t *testing.T, src, name string) int64 {
	t.Helper()
	m := regexp.MustCompile(`export const ` + name + ` = ([0-9_]+);`).FindStringSubmatch(src)
	require.NotNilf(t, m, "retention-batch.ts: %s not found as a numeric export", name)
	v, err := strconv.ParseInt(strings.ReplaceAll(m[1], "_", ""), 10, 64)
	require.NoError(t, err)
	return v
}

func tfInt(t *testing.T, v any, what string) int64 {
	t.Helper()
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	t.Fatalf("%s: %v is not a number", what, v)
	return 0
}

func TestTagFacetsGate_RetentionBudgetsFitTheCurlTimeout(t *testing.T) {
	batch := tfRead(t, "apps", "dashboard-web", "src", "lib", "server", "retention-batch.ts")
	c := func(n string) int64 { return tfTSConst(t, batch, n) }

	values := readHelmValues(t, filepath.Join(tfRoot, "deploy", "helm", "sentinel", "values.yaml"))
	cron, ok := yamlMap(values, "retentionCron")
	require.True(t, ok, "values.yaml: retentionCron missing")
	timeoutS := tfInt(t, cron["timeoutSeconds"], "retentionCron.timeoutSeconds")
	activeS := tfInt(t, cron["activeDeadlineSeconds"], "retentionCron.activeDeadlineSeconds")

	steps := c("RETENTION_OCCURRENCE_BUDGET_MS") + c("RETENTION_TAG_REBUILD_BUDGET_MS") +
		c("RETENTION_TAG_REBUILD_PROBE_BUDGET_MS") + 2*c("RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS") +
		c("RETENTION_ROUTE_OTHER_STEPS_ALLOWANCE_MS")
	deadline := c("RETENTION_ROUTE_DEADLINE_MS")
	require.LessOrEqualf(t, steps, deadline, "the steps' budgets (%d ms) must fit the route deadline (%d ms)", steps, deadline)
	require.LessOrEqualf(t, deadline, timeoutS*1000-10_000,
		"the route deadline (%d ms) must end 10 s before curl's --max-time (%d s)", deadline, timeoutS)
	require.GreaterOrEqual(t, activeS, timeoutS, "activeDeadlineSeconds must be >= timeoutSeconds")

	stale := c("RETENTION_ROUTE_LOCK_STALE_MS")
	require.LessOrEqualf(t, deadline+60_000, stale, "a live run (deadline %d ms) must never be called stale (%d ms)", deadline, stale)
	require.Less(t, stale, int64(3_600_000), "the next hourly run must call a leftover holder stale")

	compose := regexp.MustCompile(`RETENTION_CRON_TIMEOUT_SECONDS[:=\-\s"]*([0-9]+)`)
	for _, rel := range [][]string{{"docker-compose.yml"}, {"scripts", "Dockerfile.cron"}, {"scripts", "cron-entrypoint.sh"}} {
		ms := compose.FindAllStringSubmatch(tfRead(t, rel...), -1)
		require.NotEmptyf(t, ms, "%s: RETENTION_CRON_TIMEOUT_SECONDS not set", filepath.Join(rel...))
		for _, m := range ms {
			require.Equalf(t, strconv.FormatInt(timeoutS, 10), m[1],
				"%s: RETENTION_CRON_TIMEOUT_SECONDS must equal retentionCron.timeoutSeconds", filepath.Join(rel...))
		}
	}
}

var (
	tfOutcomeValue  = regexp.MustCompile(`:\s*'([a-z_]+)'`)
	tfExprOutcomes  = regexp.MustCompile(`outcome=~?"([^"]+)"`)
	tfNotCompleting = regexp.MustCompile(
		`^\(sum\(increase\(sentinel_retention_runs_total\{outcome="completed"\}\[[^\]]+\]\)\) or vector\(0\)\) == 0$`)
)

func TestTagFacetsGate_RetentionAlertsMatchTheCounter(t *testing.T) {
	metrics := tfRead(t, "apps", "dashboard-web", "src", "lib", "server", "metrics.ts")
	start := strings.Index(metrics, "export const RETENTION_RUN_OUTCOME = {")
	require.GreaterOrEqual(t, start, 0, "metrics.ts: RETENTION_RUN_OUTCOME missing")
	end := strings.Index(metrics[start:], "} as const;")
	require.Greater(t, end, 0)
	declared := map[string]bool{}
	for _, m := range tfOutcomeValue.FindAllStringSubmatch(metrics[start:start+end], -1) {
		declared[m[1]] = true
	}
	for _, o := range []string{"completed", "behind", "skipped", "lock_stale", "deadline_exceeded", "connection_lost", "failed"} {
		require.Truef(t, declared[o], "RETENTION_RUN_OUTCOME must declare %q", o)
	}

	// The template is Helm, not YAML, so it is read as text: the group, then each alert's expr line.
	rule := tfRead(t, "deploy", "helm", "sentinel", "templates", "prometheusrule.yaml")
	g := strings.Index(rule, "- name: sentinel-retention")
	require.GreaterOrEqual(t, g, 0, "prometheusrule.yaml: group sentinel-retention missing")
	group := rule[g:]
	if next := strings.Index(group[1:], "- name: "); next >= 0 {
		group = group[:next+1]
	}
	exprs := map[string]string{}
	var current string
	for _, line := range strings.Split(group, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "- alert: ") {
			current = strings.TrimPrefix(l, "- alert: ")
		} else if strings.HasPrefix(l, "expr: ") && current != "" {
			exprs[current] = strings.Join(strings.Fields(strings.TrimPrefix(l, "expr: ")), " ")
		}
	}
	names := make([]string, 0, len(exprs))
	for n := range exprs {
		names = append(names, n)
	}
	sort.Strings(names)
	require.Equal(t, []string{"SentinelRetentionLockStale", "SentinelRetentionNotCompleting", "SentinelRetentionRunFailing"}, names)
	for name, expr := range exprs {
		require.Containsf(t, expr, "sentinel_retention_runs_total", "%s must read the retention counter", name)
		ms := tfExprOutcomes.FindAllStringSubmatch(expr, -1)
		require.NotEmptyf(t, ms, "%s must select an outcome", name)
		for _, m := range ms {
			for _, o := range strings.Split(m[1], "|") {
				require.Truef(t, declared[o], "%s names outcome %q, which RETENTION_RUN_OUTCOME does not declare", name, o)
			}
		}
	}
	require.Regexp(t, tfNotCompleting, exprs["SentinelRetentionNotCompleting"],
		"SentinelRetentionNotCompleting must count `completed` alone and fire with no series (or vector(0))")

	values := readHelmValues(t, filepath.Join(tfRoot, "deploy", "helm", "sentinel", "values.yaml"))
	pr, ok := yamlMap(values, "prometheusRule")
	require.True(t, ok, "values.yaml: prometheusRule missing")
	for _, k := range []string{"retentionWindow", "retentionStalledWindow"} {
		require.NotEmptyf(t, pr[k], "values.yaml: prometheusRule.%s missing", k)
	}
	sev, ok := yamlMap(values, "prometheusRule", "severity")
	require.True(t, ok, "values.yaml: prometheusRule.severity missing")
	for _, k := range []string{"retentionLockStale", "retentionFailing", "retentionNotCompleting"} {
		require.NotEmptyf(t, sev[k], "values.yaml: prometheusRule.severity.%s missing", k)
	}
}

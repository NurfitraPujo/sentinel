package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionFrom(t *testing.T) {
	bi := func(mv string, kv ...string) *debug.BuildInfo {
		b := &debug.BuildInfo{Main: debug.Module{Version: mv}}
		for i := 0; i < len(kv); i += 2 {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return b
	}
	cases := []struct {
		name, ld string
		bi       *debug.BuildInfo
		ok       bool
		want     string
	}{
		{"ldflag wins", "v0.2.41", bi("v9.9.9"), true, "v0.2.41"},
		{"module version", "", bi("v1.2.3"), true, "v1.2.3"},
		{"vcs revision", "", bi("(devel)", "vcs.revision", "0123456789abcdef"), true, "dev-0123456789ab"},
		{"vcs dirty", "", bi("(devel)", "vcs.revision", "0123456789abcdef", "vcs.modified", "true"), true, "dev-0123456789ab-dirty"},
		{"nothing", "", bi("(devel)"), true, "dev"},
		{"no buildinfo", "", nil, false, "dev"},
	}
	for _, c := range cases {
		if got := versionFrom(c.ld, c.bi, c.ok); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRun_VersionCommandAndFlag(t *testing.T) {
	old := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = old })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var out, errb bytes.Buffer
		if rc := run(args, &out, &errb); rc != 0 || strings.TrimSpace(out.String()) != "v1.2.3-test" {
			t.Errorf("%v: rc=%d out=%q err=%q", args, rc, out.String(), errb.String())
		}
	}
}

func TestClient_UserAgent(t *testing.T) {
	old := version
	version = "v7.7.7"
	t.Cleanup(func() { version = old })
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ua = r.UserAgent(); fmt.Fprint(w, "{}") }))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "k").Do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if ua != "sentinel/v7.7.7" {
		t.Errorf("User-Agent = %q", ua)
	}
}

// --- login ----------------------------------------------------------------------------------------

const testKey = "sent_agent_SECRETSECRETSECRET"

func selfServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/self" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"unauthorized"}`)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"agentId":"a1","name":"Ada Bot","organizationId":"org_42","key":{"id":"k1","prefix":"sent_agent_SECR"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func selfServerWithProfile(t *testing.T, profile string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"agentId":"a1","name":"Ada Bot","organizationId":"org_42","cliProfile":%q,"key":{"id":"k1","prefix":"sent_agent_SECR"}}`, profile)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loginHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("SENTINEL_URL", "")
	t.Setenv("SENTINEL_AGENT_KEY", "")
	t.Setenv("SENTINEL_PROFILE", "")
	return filepath.Join(home, ".config", "sentinel", "config.json")
}

func testLoginIO(stdin string, tty bool) (*loginIO, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &loginIO{
		stdin: bufio.NewReader(strings.NewReader(stdin)), isTTY: tty, stdout: &out, stderr: &errb,
		readSecret: func(r *bufio.Reader) (string, error) { return readLine(r) },
	}, &out, &errb
}

func TestLogin_SuccessWritesPrivateFileAndNeverPrintsKey(t *testing.T) {
	cfgPath := loginHome(t)
	srv := selfServer(t, 200)
	io_, out, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL + "/", "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d err=%s", rc, errb)
	}
	if strings.Contains(out.String()+errb.String(), "SECRETSECRET") {
		t.Fatalf("key leaked: %q %q", out, errb)
	}
	for _, want := range []string{"Ada Bot", "org_42", "sent_agent_SECR"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %o", st.Mode().Perm())
	}
	if d, _ := os.Stat(filepath.Dir(cfgPath)); d.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o", d.Mode().Perm())
	}
	cf, err := loadConfigFile(func(m string) { t.Error(m) })
	p := cf.Profiles["default"]
	if err != nil || p.URL != srv.URL || p.Key != testKey || cf.CurrentProfile != "default" {
		t.Errorf("cf=%+v err=%v", cf, err)
	}
}

func TestLogin_BadKeyWritesNothing(t *testing.T) {
	cfgPath := loginHome(t)
	srv := selfServer(t, 200)
	io_, out, errb := testLoginIO("sent_agent_WRONGWRONG\n", false)
	rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_)
	if rc != ExitAuth {
		t.Fatalf("rc=%d", rc)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config written on 401: %v", err)
	}
	if strings.Contains(out.String()+errb.String(), "WRONGWRONG") {
		t.Error("key leaked")
	}
}

func TestLogin_EnvOverrideWarning(t *testing.T) {
	loginHome(t)
	srv := selfServer(t, 200)
	t.Setenv("SENTINEL_URL", "https://stale.example.com")
	io_, _, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatal(rc)
	}
	if !strings.Contains(errb.String(), "SENTINEL_URL is set in your environment") {
		t.Errorf("no env warning: %q", errb)
	}
}

func TestLogin_NonTTYRequiresKeyStdin(t *testing.T) {
	cfgPath := loginHome(t)
	srv := selfServer(t, 200)
	io_, _, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL}, cfgFlags{}, io_); rc != ExitUsage {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(errb.String(), "--key-stdin") {
		t.Errorf("err: %q", errb)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("config written")
	}
}

func TestLogin_TTYPromptsForURLAndHiddenKey(t *testing.T) {
	cfgPath := loginHome(t)
	srv := selfServer(t, 200)
	io_, out, errb := testLoginIO(srv.URL+"\n"+testKey+"\n", true)
	if rc := cmdLogin(context.Background(), nil, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if !strings.Contains(errb.String(), "Agent key (input hidden)") || strings.Contains(out.String()+errb.String(), "SECRETSECRET") {
		t.Errorf("prompt/leak: %q %q", out, errb)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Error(err)
	}
}

// --- agent install --------------------------------------------------------------------------------

func installHome(t *testing.T, dirs ...string) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func runInstall(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	rc := cmdAgentInstall(args, &out, &errb)
	return rc, out.String(), errb.String()
}

func TestAgentInstall_FreshIdempotentAndUpdate(t *testing.T) {
	home := installHome(t, ".claude", ".gemini")
	claude := filepath.Join(home, ".claude/skills/sentinel-agent/SKILL.md")
	agy := filepath.Join(home, ".gemini/config/skills/sentinel-agent/SKILL.md")
	codex := filepath.Join(home, ".agents/skills/sentinel-agent/SKILL.md")

	rc, out, errb := runInstall()
	if rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if !strings.Contains(out, claude) || !strings.Contains(out, agy) {
		t.Errorf("paths not printed: %s", out)
	}
	if _, err := os.Stat(codex); !os.IsNotExist(err) {
		t.Error("codex written although ~/.codex does not exist")
	}
	b, _ := os.ReadFile(claude)
	if !strings.HasPrefix(string(b), "---\nname: sentinel-agent") || !isMarked(b) || !strings.Contains(string(b), "installed by sentinel "+buildVersion()) {
		t.Errorf("bad content:\n%.400s", b)
	}

	_, out, _ = runInstall()
	if !strings.Contains(out, "up to date") {
		t.Errorf("rerun not idempotent: %s", out)
	}

	// A different version updates in place.
	old := version
	version = "v9.9.9"
	t.Cleanup(func() { version = old })
	_, out, _ = runInstall("--claude")
	b, _ = os.ReadFile(claude)
	if !strings.Contains(out, "updated") || !strings.Contains(string(b), "installed by sentinel v9.9.9") {
		t.Errorf("no in-place update: %s", out)
	}
}

func TestAgentInstall_RefusesUnmarkedWithoutForce(t *testing.T) {
	home := installHome(t, ".claude")
	f := filepath.Join(home, ".claude/skills/sentinel-agent/SKILL.md")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("my own skill\n"), 0o644)

	rc, _, errb := runInstall()
	if rc != ExitConflict || !strings.Contains(errb, "would change") {
		t.Fatalf("rc=%d err=%s", rc, errb)
	}
	if b, _ := os.ReadFile(f); string(b) != "my own skill\n" {
		t.Fatal("unmarked file was overwritten")
	}
	if rc, _, _ := runInstall("--force"); rc != 0 {
		t.Fatal("force failed")
	}
	if b, _ := os.ReadFile(f); !isMarked(b) {
		t.Fatal("force did not replace")
	}
}

func TestAgentInstall_DryRunWritesNothing(t *testing.T) {
	home := installHome(t, ".claude")
	rc, out, _ := runInstall("--dry-run")
	if rc != 0 || !strings.Contains(out, "would write") {
		t.Fatalf("rc=%d %s", rc, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills")); !os.IsNotExist(err) {
		t.Error("dry-run created files")
	}
}

func TestAgentInstall_UninstallRemovesOnlyMarked(t *testing.T) {
	home := installHome(t, ".claude", ".codex")
	runInstall("--claude")
	codex := filepath.Join(home, ".agents/skills/sentinel-agent/SKILL.md")
	os.MkdirAll(filepath.Dir(codex), 0o755)
	os.WriteFile(codex, []byte("hand written\n"), 0o644)

	rc, _, errb := runInstall("--uninstall", "--claude", "--codex")
	if rc != ExitConflict || !strings.Contains(errb, "not installed by sentinel") {
		t.Errorf("rc=%d err=%s", rc, errb)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/sentinel-agent")); !os.IsNotExist(err) {
		t.Error("marked file/dir not removed")
	}
	if b, _ := os.ReadFile(codex); string(b) != "hand written\n" {
		t.Error("unmarked file touched by uninstall")
	}
}

func TestAgentInstall_NoToolsFound(t *testing.T) {
	installHome(t)
	if rc, _, errb := runInstall(); rc != ExitUsage || !strings.Contains(errb, "--claude") {
		t.Errorf("rc=%d %s", rc, errb)
	}
}

// --- embedded copies ------------------------------------------------------------------------------

func TestEmbeddedSkillShape(t *testing.T) {
	if !strings.HasPrefix(string(embeddedSkill), "---\nname: sentinel-agent\n") {
		t.Fatal("skill must start with frontmatter")
	}
	for _, must := range []string{"sentinel whoami", "sentinel login", "sentinel guide"} {
		if !strings.Contains(string(embeddedSkill), must) {
			t.Errorf("skill missing %q", must)
		}
	}
	for _, bad := range []string{"tools/sentinel", "this repo", "docs/agents"} {
		if strings.Contains(string(embeddedSkill), bad) {
			t.Errorf("skill must be repo-independent but mentions %q", bad)
		}
	}
}

// Drift guard when run inside the monorepo (skipped for a bare module checkout).
func TestEmbeddedCopiesMatchCanonical(t *testing.T) {
	for src, emb := range map[string][]byte{
		"../../docs/agents/SENTINEL_AGENT_GUIDE.md":    embeddedGuide,
		"../../.agents/skills/sentinel-agent/SKILL.md": embeddedSkill,
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("monorepo file not present: %v", err)
		}
		if !bytes.Equal(b, emb) {
			t.Errorf("%s differs from the embedded copy — run scripts/sync-cli-embeds.sh", src)
		}
	}
}

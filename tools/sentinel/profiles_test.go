package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func writeRaw(t *testing.T, content string) {
	t.Helper()
	p, _ := configPath()
	if err := os.MkdirAll(dirOf(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirOf(p string) string { return p[:strings.LastIndex(p, "/")] }

const twoProfiles = `{"current_profile":"production","profiles":{
 "production":{"url":"https://prod.example","agent_key":"prod_key"},
 "staging":{"url":"https://stg.example","agent_key":"stg_key"}}}`

const legacyFile = `{"url":"https://legacy.example","agent_key":"legacy_key"}`

func TestResolve_Table(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		flags      cfgFlags
		env        map[string]string
		wantURL    string
		wantKey    string
		wantProf   string
		wantURLSrc string
		wantWarn   string // substring; "" = no warning expected
		wantErr    string
	}{
		{name: "current profile from file", file: twoProfiles, wantURL: "https://prod.example", wantKey: "prod_key", wantProf: "production", wantURLSrc: "profile production"},
		{name: "--profile beats current", file: twoProfiles, flags: cfgFlags{Profile: "staging"}, wantURL: "https://stg.example", wantKey: "stg_key", wantProf: "staging", wantURLSrc: "profile staging"},
		{name: "SENTINEL_PROFILE beats current", file: twoProfiles, env: map[string]string{"SENTINEL_PROFILE": "staging"}, wantURL: "https://stg.example", wantKey: "stg_key", wantProf: "staging", wantURLSrc: "profile staging"},
		{name: "--profile beats SENTINEL_PROFILE", file: twoProfiles, flags: cfgFlags{Profile: "production"}, env: map[string]string{"SENTINEL_PROFILE": "staging"}, wantURL: "https://prod.example", wantKey: "prod_key", wantProf: "production", wantURLSrc: "profile production"},
		{name: "legacy flat file is profile default", file: legacyFile, wantURL: "https://legacy.example", wantKey: "legacy_key", wantProf: "default", wantURLSrc: "profile default"},
		{name: "env pair beats current profile", file: twoProfiles, env: map[string]string{"SENTINEL_URL": "https://env.example", "SENTINEL_AGENT_KEY": "env_key"}, wantURL: "https://env.example", wantKey: "env_key", wantProf: "", wantURLSrc: "env pair"},
		{name: "explicit --profile beats env pair", file: twoProfiles, flags: cfgFlags{Profile: "staging"}, env: map[string]string{"SENTINEL_URL": "https://env.example", "SENTINEL_AGENT_KEY": "env_key"}, wantURL: "https://stg.example", wantKey: "stg_key", wantProf: "staging", wantURLSrc: "profile staging"},
		{name: "SENTINEL_PROFILE beats env pair", file: twoProfiles, env: map[string]string{"SENTINEL_PROFILE": "staging", "SENTINEL_URL": "https://env.example", "SENTINEL_AGENT_KEY": "env_key"}, wantURL: "https://stg.example", wantKey: "stg_key", wantProf: "staging", wantURLSrc: "profile staging"},
		{name: "THE TRAP: SENTINEL_URL alone is ignored + warns", file: twoProfiles, env: map[string]string{"SENTINEL_URL": "https://stale.example"}, wantURL: "https://prod.example", wantKey: "prod_key", wantProf: "production", wantURLSrc: "profile production", wantWarn: `using profile "production"`},
		{name: "SENTINEL_URL alone ignored with legacy file", file: legacyFile, env: map[string]string{"SENTINEL_URL": "https://stale.example"}, wantURL: "https://legacy.example", wantKey: "legacy_key", wantProf: "default", wantURLSrc: "profile default", wantWarn: "unset SENTINEL_URL"},
		{name: "SENTINEL_AGENT_KEY alone is an error", file: twoProfiles, env: map[string]string{"SENTINEL_AGENT_KEY": "env_key"}, wantErr: "SENTINEL_URL is not"},
		{name: "-url/-key flags win per field over profile", file: twoProfiles, flags: cfgFlags{URL: "https://flag.example", Key: "flag_key"}, wantURL: "https://flag.example", wantKey: "flag_key", wantProf: "production", wantURLSrc: "flag"},
		{name: "-url flag + env key pair", file: twoProfiles, flags: cfgFlags{URL: "https://flag.example"}, env: map[string]string{"SENTINEL_AGENT_KEY": "env_key"}, wantURL: "https://flag.example", wantKey: "env_key", wantURLSrc: "flag"},
		{name: "unknown --profile errors naming existing", file: twoProfiles, flags: cfgFlags{Profile: "nope"}, wantErr: "production, staging"},
		{name: "unknown SENTINEL_PROFILE errors", file: twoProfiles, env: map[string]string{"SENTINEL_PROFILE": "nope"}, wantErr: "unknown profile"},
		{name: "current_profile names a missing profile", file: `{"current_profile":"gone","profiles":{"a":{"url":"https://a.example","agent_key":"k"}}}`, wantErr: "profiles that exist: a"},
		{name: "no file, no env: unset", file: "", wantURL: "", wantKey: "", wantProf: "default", wantURLSrc: "unset"},
		{name: "no file, env pair", file: "", env: map[string]string{"SENTINEL_URL": "https://env.example", "SENTINEL_AGENT_KEY": "env_key"}, wantURL: "https://env.example", wantKey: "env_key", wantURLSrc: "env pair"},
		{name: "invalid profile name", file: twoProfiles, flags: cfgFlags{Profile: "Bad Name"}, wantErr: "invalid profile name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loginHome(t)
			t.Setenv("SENTINEL_PROFILE", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if c.file != "" {
				writeRaw(t, c.file)
			}
			var warns []string
			r, err := resolveConfigSources(c.flags, func(m string) { warns = append(warns, m) })
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.URL != c.wantURL || r.Key != c.wantKey || r.Profile != c.wantProf || r.URLSource != c.wantURLSrc {
				t.Errorf("got url=%q key=%q profile=%q urlSrc=%q; want %q %q %q %q", r.URL, r.Key, r.Profile, r.URLSource, c.wantURL, c.wantKey, c.wantProf, c.wantURLSrc)
			}
			joined := strings.Join(warns, "|")
			if c.wantWarn == "" && joined != "" {
				t.Errorf("unexpected warnings: %s", joined)
			}
			if c.wantWarn != "" && !strings.Contains(joined, c.wantWarn) {
				t.Errorf("warnings %q lack %q", joined, c.wantWarn)
			}
		})
	}
}

func TestResolveConfig_StrictRequiresBoth(t *testing.T) {
	loginHome(t)
	if _, err := resolveConfig(cfgFlags{}, func(string) {}); err == nil || !strings.Contains(err.Error(), "sentinel login") {
		t.Errorf("err = %v", err)
	}
}

func TestLogin_MigratesLegacyFileAndAddsProfile(t *testing.T) {
	loginHome(t)
	writeRaw(t, legacyFile)
	srv := selfServer(t, 200)
	io_, out, errb := testLoginIO(testKey+"\n", false)
	rc := cmdLogin(context.Background(), []string{"--profile", "staging", "--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_)
	if rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	p, _ := configPath()
	raw, _ := os.ReadFile(p)
	var got ConfigFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Profiles["default"].Key != "legacy_key" || got.Profiles["staging"].Key != testKey {
		t.Errorf("profiles = %+v", got.Profiles)
	}
	if strings.Contains(string(raw), `"agent_key": "legacy_key"`) && !strings.Contains(string(raw), "profiles") {
		t.Error("file not migrated")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", st.Mode().Perm())
	}
	// Not the first profile and no --use: current must not silently flip.
	if got.CurrentProfile == "staging" {
		t.Errorf("current flipped to staging without --use")
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Error("key leaked")
	}
}

func TestLogin_UseFlagMakesCurrent(t *testing.T) {
	loginHome(t)
	writeRaw(t, twoProfiles)
	srv := selfServer(t, 200)
	io_, _, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--profile", "qa", "--use", "--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	cf, _ := loadConfigFile(func(string) {})
	if cf.CurrentProfile != "qa" || len(cf.Profiles) != 3 {
		t.Errorf("%+v", cf)
	}
}

func TestLogin_ReplacingProfileURLNeedsConsent(t *testing.T) {
	loginHome(t)
	writeRaw(t, twoProfiles)
	srv := selfServer(t, 200)
	io_, _, errb := testLoginIO(testKey+"\n", false)
	rc := cmdLogin(context.Background(), []string{"--profile", "staging", "--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_)
	if rc != ExitUsage || !strings.Contains(errb.String(), "--yes") {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if cf, _ := loadConfigFile(func(string) {}); cf.Profiles["staging"].URL != "https://stg.example" {
		t.Error("profile replaced without consent")
	}
	io_, _, errb = testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--profile", "staging", "--yes", "--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	// TTY: answer "n" declines, "y" accepts.
	io_, _, errb = testLoginIO(srv.URL+"\n"+testKey+"\nn\n", true)
	writeRaw(t, twoProfiles)
	if rc := cmdLogin(context.Background(), []string{"--profile", "staging", "--key-stdin"}, cfgFlags{}, io_); rc != ExitUsage || !strings.Contains(errb.String(), "Replace") {
		t.Fatalf("decline: rc=%d %s", rc, errb)
	}
}

func TestLogin_FreshMachineUsesServerSuggestedProfile(t *testing.T) {
	loginHome(t)
	t.Setenv("SENTINEL_PROFILE", "")
	srv := selfServerWithProfile(t, "production")
	io_, out, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	cf, _ := loadConfigFile(func(string) {})
	if _, ok := cf.Profiles["production"]; !ok || cf.CurrentProfile != "production" || !strings.Contains(out.String(), "profile: production (current)") {
		t.Errorf("%+v\n%s", cf, out)
	}
}

func TestProfileCommands(t *testing.T) {
	loginHome(t)
	writeRaw(t, twoProfiles)
	run1 := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		rc := cmdProfile(args, &o, &e)
		return rc, o.String(), e.String()
	}
	rc, out, _ := run1("list")
	if rc != 0 || !strings.Contains(out, "* production") || !strings.Contains(out, "  staging") || strings.Contains(out, "prod_key") || strings.Contains(out, "stg_key") {
		t.Errorf("list:\n%s", out)
	}
	if rc, _, e := run1("use", "nope"); rc != ExitUsage || !strings.Contains(e, "production, staging") {
		t.Errorf("use unknown: %d %s", rc, e)
	}
	if rc, _, _ := run1("use", "staging"); rc != 0 {
		t.Fatal(rc)
	}
	if cf, _ := loadConfigFile(func(string) {}); cf.CurrentProfile != "staging" {
		t.Errorf("current=%s", cf.CurrentProfile)
	}
	if rc, out, _ := run1("remove", "staging"); rc != 0 || !strings.Contains(out, "current profile: production") {
		t.Errorf("remove: %d %s", rc, out)
	}
	if cf, _ := loadConfigFile(func(string) {}); len(cf.Profiles) != 1 || cf.CurrentProfile != "production" {
		t.Errorf("%+v", cf)
	}
}

func TestConfigCmd_ShowsProfileAndSourcesNeverKey(t *testing.T) {
	loginHome(t)
	writeRaw(t, twoProfiles)
	var out, errb bytes.Buffer
	cmdConfig(cfgFlags{Profile: "staging"}, &out, &errb)
	s := out.String()
	for _, want := range []string{"profile: staging (selected by flag)", "https://stg.example (from profile staging)", "key:     set (from profile staging)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "stg_key") {
		t.Error("key leaked")
	}
}

func TestKeyPrefix_StoredFromServerAndFallback(t *testing.T) {
	if got := displayPrefix("sent_agent_SECR", testKey); got != "sent_agent_SECR" {
		t.Errorf("stored: %q", got)
	}
	if got := displayPrefix("", testKey); got != "sent_agent_SEC…" || strings.Contains(got, "RET") {
		t.Errorf("fallback: %q", got)
	}
	// login stores the server-reported prefix, and `profile list` shows it (and never the key).
	loginHome(t)
	srv := selfServer(t, 200)
	io_, _, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--key-stdin"}, cfgFlags{}, io_); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	cf, _ := loadConfigFile(func(string) {})
	if cf.Profiles["default"].KeyPrefix != "sent_agent_SECR" {
		t.Errorf("stored prefix = %q", cf.Profiles["default"].KeyPrefix)
	}
	var out, e bytes.Buffer
	cmdProfile([]string{"list"}, &out, &e)
	if !strings.Contains(out.String(), "(sent_agent_SECR)") || strings.Contains(out.String(), "SECRETSECRET") {
		t.Errorf("list: %s", out.String())
	}
}

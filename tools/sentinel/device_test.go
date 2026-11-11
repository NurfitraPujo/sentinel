package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// sentinel-cli-browser-login: `sentinel login` (device flow), `sentinel logout`, and the identity lines of whoami/config,
// against an httptest dashboard. The key must never appear in any output.

const personalKey = "sent_agent_PERSONALPERSONALPERSONAL"

type fakeDashboard struct {
	t           *testing.T
	mu          sync.Mutex
	tokenSteps  []string // per poll: "pending" | "slow_down" | "denied" | "expired" | "ok"
	polls       int
	startBodies []map[string]string
	revokes     int
	revokeCode  int
	selfOwner   bool
	// verificationBase overrides the origin of the verification URIs the fake returns; empty = this server's own.
	verificationBase string
}

func (f *fakeDashboard) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/cli/device":
			if r.Header.Get("Authorization") != "" {
				f.t.Errorf("device start sent an Authorization header")
			}
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.startBodies = append(f.startBodies, b)
			base := firstNonEmpty(f.verificationBase, "http://"+r.Host)
			fmt.Fprintf(w, `{"device_code":"DEVICECODE-not-a-secret-to-print","user_code":"BCDF-GHJK","verification_uri":%q,"verification_uri_complete":%q,"expires_in":600,"interval":5}`, base+"/cli/approve", base+"/cli/approve?code=BCDF-GHJK")
		case "/api/cli/device/token":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["device_code"] != "DEVICECODE-not-a-secret-to-print" {
				f.t.Errorf("token poll with device_code %q", b["device_code"])
			}
			step := "pending"
			if f.polls < len(f.tokenSteps) {
				step = f.tokenSteps[f.polls]
			}
			f.polls++
			switch step {
			case "ok":
				fmt.Fprintf(w, `{"key":%q,"key_prefix":"sent_agent_","key_id":"k1","organization_id":"org-1","org_slug":"acme","url":"http://dash","cli_profile":"production","expires_at":"2027-01-05T00:00:00.000Z","owner":{"name":"Ada","email":"ada@example.test","role":"engineer"}}`, personalKey)
			case "slow_down":
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"slow_down","error_description":"Polling too fast","interval":10}`)
			case "denied":
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"access_denied","error_description":"denied"}`)
			case "expired":
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"expired_token","error_description":"expired"}`)
			default:
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"authorization_pending","error_description":"waiting"}`)
			}
		case "/api/agent/self":
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch {
			case key == personalKey && f.selfOwner:
				fmt.Fprint(w, `{"agentId":"a1","name":"Ada (CLI)","organizationId":"org-1","kind":"personal","owner":{"userId":"u1","name":"Ada","email":"ada@example.test","role":"engineer","canWrite":true},"key":{"id":"k1","prefix":"sent_agent_","createdAt":null,"expiresAt":"2027-01-05T00:00:00.000Z","lastUsedAt":null}}`)
			case key == testKey:
				fmt.Fprint(w, `{"agentId":"a2","name":"CI Bot","organizationId":"org-1","kind":"ai","owner":null,"key":{"id":"k2","prefix":"sent_agent_SECR","createdAt":null,"expiresAt":null,"lastUsedAt":null}}`)
			default:
				w.WriteHeader(401)
				fmt.Fprint(w, `{"message":"Invalid API key"}`)
			}
		case "/api/agent/key/revoke":
			f.revokes++
			code := f.revokeCode
			if code == 0 {
				code = 200
			}
			w.WriteHeader(code)
			if code == 200 {
				fmt.Fprint(w, `{"success":true,"revokedKeyId":"k1"}`)
			} else {
				fmt.Fprint(w, `{"message":"Forbidden: org agent keys are revoked by an org owner or admin in the dashboard"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// fastTicks makes the poll loop fire immediately and records each requested interval.
func fastTicks(t *testing.T) *[]int {
	t.Helper()
	var got []int
	old := tick
	tick = func(sec int) <-chan time.Time {
		got = append(got, sec)
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	t.Cleanup(func() { tick = old })
	return &got
}

// stubBrowser records the URL login tried to open, and fails like a headless machine would.
func stubBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	old := openBrowser
	openBrowser = func(url string) error { opened = append(opened, url); return errors.New("no browser here") }
	t.Cleanup(func() { openBrowser = old })
	return &opened
}

func noKeyIn(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		if strings.Contains(o, "PERSONALPERSONAL") || strings.Contains(o, "SECRETSECRET") || strings.Contains(o, "DEVICECODE") {
			t.Fatalf("a secret leaked into output:\n%s", o)
		}
	}
}

func TestDeviceLogin_PendingSlowDownSuccess_SavesProfileNeverPrintsKey(t *testing.T) {
	cfgPath := loginHome(t)
	intervals := fastTicks(t)
	opened := stubBrowser(t)
	f := &fakeDashboard{t: t, tokenSteps: []string{"pending", "slow_down", "pending", "ok"}, selfOwner: true}
	srv := f.server()

	io_, out, errb := testLoginIO("", false) // headless: not a terminal, nothing on stdin
	rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--device-name", "ada-laptop"}, cfgFlags{}, io_)
	if rc != ExitOK {
		t.Fatalf("rc=%d\n%s", rc, errb)
	}
	noKeyIn(t, out.String(), errb.String())

	// It said what to approve, tried the browser, and polled at 5 s, then 10 s after slow_down.
	approveURL := srv.URL + "/cli/approve?code=BCDF-GHJK"
	for _, want := range []string{approveURL, "BCDF-GHJK", "Only approve it if you started this login yourself", "could not open a browser"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb)
		}
	}
	if len(*opened) != 1 || (*opened)[0] != approveURL {
		t.Errorf("opened = %v", *opened)
	}
	if fmt.Sprint(*intervals) != "[5 5 10 10]" {
		t.Errorf("poll intervals = %v, want [5 5 10 10]", *intervals)
	}
	if b := f.startBodies[0]; b["client"] != "sentinel-cli" || b["device_name"] != "ada-laptop" || b["profile"] != "" || b["version"] == "" {
		t.Errorf("start body = %v", b)
	}

	// Fresh machine: the server-suggested profile, 0600, with the key, its prefix, and who it acts as.
	cf, err := loadConfigFile(func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cf.Profiles["production"]
	if !ok || cf.CurrentProfile != "production" || p.Key != personalKey || p.Owner != "ada@example.test" || p.Kind != "personal" || p.URL != srv.URL {
		t.Fatalf("profiles = %+v current=%q", cf.Profiles, cf.CurrentProfile)
	}
	if st, _ := os.Stat(cfgPath); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", st.Mode().Perm())
	}
	for _, want := range []string{"as: Ada <ada@example.test> (engineer, read-write)", "organization: acme", "profile: production (current)", "expires 2027-01-05"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDeviceLogin_NoBrowserFlagNeverTriesOne(t *testing.T) {
	loginHome(t)
	fastTicks(t)
	opened := stubBrowser(t)
	f := &fakeDashboard{t: t, tokenSteps: []string{"ok"}, selfOwner: true}
	srv := f.server()
	io_, _, errb := testLoginIO("", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--no-browser", "--profile", "work"}, cfgFlags{}, io_); rc != ExitOK {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if len(*opened) != 0 {
		t.Errorf("browser opened: %v", *opened)
	}
	if f.startBodies[0]["profile"] != "work" {
		t.Errorf("requested profile = %q", f.startBodies[0]["profile"])
	}
	if f.startBodies[0]["device_name"] == "" {
		t.Error("default device name (hostname) not sent")
	}
	if cf, _ := loadConfigFile(func(string) {}); cf.Profiles["work"].Key != personalKey {
		t.Errorf("%+v", cf.Profiles)
	}
}

func TestDeviceLogin_DeniedAndExpiredSaveNothing(t *testing.T) {
	for _, step := range []string{"denied", "expired"} {
		t.Run(step, func(t *testing.T) {
			cfgPath := loginHome(t)
			fastTicks(t)
			stubBrowser(t)
			f := &fakeDashboard{t: t, tokenSteps: []string{"pending", step}}
			srv := f.server()
			io_, out, errb := testLoginIO("", false)
			if rc := cmdLogin(context.Background(), []string{"--url", srv.URL}, cfgFlags{}, io_); rc != ExitAuth {
				t.Fatalf("rc=%d %s", rc, errb)
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Error("config written")
			}
			noKeyIn(t, out.String(), errb.String())
		})
	}
}

func TestDeviceLogin_AsksBeforeReplacingAProfileURL_BeforeAnyKeyIsMinted(t *testing.T) {
	loginHome(t)
	writeRaw(t, twoProfiles)
	fastTicks(t)
	stubBrowser(t)
	f := &fakeDashboard{t: t, tokenSteps: []string{"ok"}, selfOwner: true}
	srv := f.server()
	io_, _, errb := testLoginIO("", false)
	if rc := cmdLogin(context.Background(), []string{"--url", srv.URL, "--profile", "staging"}, cfgFlags{}, io_); rc != ExitUsage || !strings.Contains(errb.String(), "--yes") {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if len(f.startBodies) != 0 {
		t.Error("a device flow was started (a key could have been minted) before consent")
	}
}

func TestDeviceLogin_CancelledContextSavesNothing(t *testing.T) {
	cfgPath := loginHome(t)
	stubBrowser(t)
	old := tick
	tick = func(int) <-chan time.Time { return make(chan time.Time) } // never fires
	t.Cleanup(func() { tick = old })
	f := &fakeDashboard{t: t}
	srv := f.server()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	io_, _, errb := testLoginIO("", false)
	if rc := cmdLogin(ctx, []string{"--url", srv.URL}, cfgFlags{}, io_); rc != ExitUsage || !strings.Contains(errb.String(), "cancelled") {
		t.Fatalf("rc=%d %s", rc, errb)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("config written")
	}
}

func TestLogin_KeyStdinRejectsBrowserFlags(t *testing.T) {
	loginHome(t)
	io_, _, errb := testLoginIO(testKey+"\n", false)
	if rc := cmdLogin(context.Background(), []string{"--url", "http://x", "--key-stdin", "--no-browser"}, cfgFlags{}, io_); rc != ExitUsage {
		t.Fatalf("rc=%d %s", rc, errb)
	}
}

func writeProfiles(t *testing.T, url string) {
	t.Helper()
	writeRaw(t, fmt.Sprintf(`{"current_profile":"production","profiles":{
 "production":{"url":%q,"agent_key":%q,"key_prefix":"sent_agent_","kind":"personal","owner":"ada@example.test"},
 "ci":{"url":%q,"agent_key":%q,"kind":"ai"}}}`, url, personalKey, url, testKey))
}

func TestLogout_RevokesPersonalKeyAndRemovesProfile(t *testing.T) {
	loginHome(t)
	f := &fakeDashboard{t: t}
	srv := f.server()
	writeProfiles(t, srv.URL)
	var out, errb bytes.Buffer
	if rc := cmdLogout(context.Background(), nil, cfgFlags{}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	if f.revokes != 1 || !strings.Contains(out.String(), "revoked key sent_agent_") || !strings.Contains(out.String(), "current profile: ci") {
		t.Errorf("revokes=%d out=%s", f.revokes, out.String())
	}
	cf, _ := loadConfigFile(func(string) {})
	if _, ok := cf.Profiles["production"]; ok || cf.CurrentProfile != "ci" {
		t.Errorf("%+v", cf)
	}
	noKeyIn(t, out.String(), errb.String())
}

func TestLogout_OrgAgentKeyIsNotRevokedAndSaysSo(t *testing.T) {
	loginHome(t)
	f := &fakeDashboard{t: t, revokeCode: 403}
	srv := f.server()
	writeProfiles(t, srv.URL)
	var out, errb bytes.Buffer
	if rc := cmdLogout(context.Background(), []string{"--profile", "ci"}, cfgFlags{}, &out, &errb); rc != ExitOK {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "NOT revoked") || strings.Contains(out.String(), "revoked key") {
		t.Errorf("out=%s err=%s", out.String(), errb.String())
	}
	if cf, _ := loadConfigFile(func(string) {}); len(cf.Profiles) != 1 {
		t.Errorf("%+v", cf.Profiles)
	}
}

func TestLogout_NetworkErrorRemovesNothing(t *testing.T) {
	loginHome(t)
	writeProfiles(t, "http://127.0.0.1:1")
	var out, errb bytes.Buffer
	if rc := cmdLogout(context.Background(), nil, cfgFlags{}, &out, &errb); rc != ExitNetwork {
		t.Fatalf("rc=%d", rc)
	}
	if cf, _ := loadConfigFile(func(string) {}); len(cf.Profiles) != 2 {
		t.Errorf("%+v", cf.Profiles)
	}
	noKeyIn(t, out.String(), errb.String())
}

func TestLogout_AlreadyDeadKeyIsRemovedLocally(t *testing.T) {
	loginHome(t)
	f := &fakeDashboard{t: t, revokeCode: 401}
	srv := f.server()
	writeProfiles(t, srv.URL)
	var out, errb bytes.Buffer
	if rc := cmdLogout(context.Background(), nil, cfgFlags{}, &out, &errb); rc != ExitOK || !strings.Contains(errb.String(), "no longer accepts") {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
}

func TestWhoamiAndConfig_ShowPersonalOrOrgIdentity(t *testing.T) {
	loginHome(t)
	f := &fakeDashboard{t: t, selfOwner: true}
	srv := f.server()
	writeProfiles(t, srv.URL)
	for _, c := range []struct{ profile, want string }{
		{"production", "identity: personal key of Ada <ada@example.test> (engineer, read-write)"},
		{"ci", `identity: org agent "CI Bot" (ai)`},
	} {
		var out, errb bytes.Buffer
		if rc := run([]string{"--profile", c.profile, "whoami"}, &out, &errb); rc != ExitOK {
			t.Fatalf("%s: rc=%d %s", c.profile, rc, errb.String())
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: whoami missing %q:\n%s", c.profile, c.want, out.String())
		}
		noKeyIn(t, out.String(), errb.String())
	}
	var out, errb bytes.Buffer
	cmdConfig(cfgFlags{Profile: "production"}, &out, &errb)
	if !strings.Contains(out.String(), "identity: personal key of ada@example.test") {
		t.Errorf("config:\n%s", out.String())
	}
	out.Reset()
	cmdConfig(cfgFlags{Profile: "ci"}, &out, &errb)
	if !strings.Contains(out.String(), "identity: org agent key (ai)") {
		t.Errorf("config:\n%s", out.String())
	}
	noKeyIn(t, out.String())
}

// sentinel-cli-browser-login: the CLI opens the server-supplied verification URI only when it is http(s) on the SAME
// scheme and host as the server it was told to log in to. Anything else -- another host, javascript:, file:, a scheme
// downgrade -- is printed with a warning and never handed to the OS opener.
func TestDeviceLogin_OpensOnlySameOriginVerificationURI(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		wantOpen   bool
	}{
		{"same origin", "", true},
		{"foreign host", "https://sentinel-login.evil.example", false},
		{"scheme downgrade", "https://SAMEHOST", false}, // the fake server is http; https on the same host is a different origin
		{"non-http scheme", "file:///etc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loginHome(t)
			fastTicks(t)
			opened := stubBrowser(t)
			f := &fakeDashboard{t: t, tokenSteps: []string{"ok"}, selfOwner: true}
			srv := f.server()
			f.verificationBase = strings.Replace(tc.base, "SAMEHOST", strings.TrimPrefix(srv.URL, "http://"), 1)

			io_, out, errb := testLoginIO("", false)
			if rc := cmdLogin(context.Background(), []string{"--url", srv.URL}, cfgFlags{}, io_); rc != ExitOK {
				t.Fatalf("rc=%d\n%s", rc, errb)
			}
			noKeyIn(t, out.String(), errb.String())
			if got := len(*opened) == 1; got != tc.wantOpen {
				t.Fatalf("opened=%v, want open=%v", *opened, tc.wantOpen)
			}
			if !tc.wantOpen && !strings.Contains(errb.String(), "not opening it") {
				t.Errorf("no warning:\n%s", errb)
			}
		})
	}
}

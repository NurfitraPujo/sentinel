package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// loginIO is everything `sentinel login` touches outside the network, so tests can drive it
// without a terminal.
type loginIO struct {
	stdin  *bufio.Reader
	isTTY  bool
	stdout io.Writer
	stderr io.Writer
	// readSecret reads one line with terminal echo disabled. Only used when isTTY.
	readSecret func(r *bufio.Reader) (string, error)
}

func stdLoginIO(stdout, stderr io.Writer) *loginIO {
	return &loginIO{
		stdin:      bufio.NewReader(os.Stdin),
		isTTY:      stdinIsTTY(),
		stdout:     stdout,
		stderr:     stderr,
		readSecret: readSecretNoEcho,
	}
}

// stdinIsTTY is true only for a real terminal: a character device on which `stty` works.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	return cmd.Run() == nil
}

// readSecretNoEcho turns terminal echo off with `stty` (stdlib-only; no x/term), reads one line, and
// always restores the previous settings — including on SIGINT/SIGTERM.
func readSecretNoEcho(r *bufio.Reader) (string, error) {
	sttyOut := func(args ...string) (string, error) {
		cmd := exec.Command("stty", args...)
		cmd.Stdin = os.Stdin
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	saved, err := sttyOut("-g")
	if err != nil {
		return "", fmt.Errorf("cannot read terminal settings (stty): %w", err)
	}
	if _, err := sttyOut("-echo"); err != nil {
		return "", fmt.Errorf("cannot disable terminal echo (stty): %w", err)
	}
	restore := func() { _, _ = sttyOut(saved) }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() { close(done); signal.Stop(sig); restore(); fmt.Fprintln(os.Stderr) }()
	return readLine(r)
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// normalizeURL trims whitespace and trailing slashes and requires an http(s) base URL.
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q: expected something like https://sentinel.example.com", raw)
	}
	return raw, nil
}

// cmdLogin implements `sentinel login [--profile P] [--url U] [--device-name N] [--no-browser] [--key-stdin] [--use]
// [--yes]`.
//
// Without --key-stdin it runs the browser device flow (device.go): the dashboard mints a PERSONAL key once the user
// approves the printed code, so any org member can log in with no admin. --key-stdin keeps the paste flow for org agent
// keys / CI: one line from stdin, or a hidden prompt on a terminal. Either way the key is verified against
// GET /api/agent/self BEFORE anything is written, profile P is written/updated in config.json atomically (0600, dir
// 0700), and the key is never printed.
func cmdLogin(ctx context.Context, args []string, g cfgFlags, io_ *loginIO) int {
	const usageLine = "usage: sentinel login [--profile P] [--url URL] [--device-name NAME] [--no-browser] [--key-stdin] [--use] [--yes]"
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io_.stderr)
	urlFlag := fs.String("url", "", "Sentinel server base URL")
	profFlag := fs.String("profile", "", "profile to write (default: the selected profile)")
	keyStdin := fs.Bool("key-stdin", false, "paste an existing agent key instead of logging in through the browser (one line on stdin; a hidden prompt on a terminal)")
	deviceName := fs.String("device-name", "", "name shown on the approval page and in CLI sessions (default: this machine's hostname)")
	noBrowser := fs.Bool("no-browser", false, "do not try to open a browser; just print the URL and code")
	use := fs.Bool("use", false, "make this profile the current one")
	yes := fs.Bool("yes", false, "replace an existing profile's URL without asking")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(io_.stderr, usageLine)
		return ExitUsage
	}
	if *keyStdin && (*deviceName != "" || *noBrowser) {
		fmt.Fprintln(io_.stderr, "error: --device-name and --no-browser apply to browser login, not --key-stdin")
		return ExitUsage
	}

	path, err := configPath()
	if err != nil {
		fmt.Fprintln(io_.stderr, "error:", err)
		return ExitUsage
	}
	cf, err := loadConfigFile(func(m string) { fmt.Fprintln(io_.stderr, m) })
	if err != nil {
		fmt.Fprintln(io_.stderr, "error:", err)
		return ExitUsage
	}

	// Which profile? --profile (login's own, else global) > SENTINEL_PROFILE > current > default; on a
	// fresh machine with no profile at all, the server may suggest one after verification (below).
	profile := firstNonEmpty(*profFlag, g.Profile, os.Getenv("SENTINEL_PROFILE"))
	explicitProfile := profile != ""
	suggestable := false
	if profile == "" {
		if len(cf.Profiles) == 0 {
			profile, suggestable = defaultProfile, true
		} else {
			profile = cf.effectiveCurrent()
		}
	}
	if err := validateProfileName(profile); err != nil {
		fmt.Fprintln(io_.stderr, "error:", err)
		return ExitUsage
	}

	// The env override trap: warn about anything that will shadow what we are about to save.
	if os.Getenv("SENTINEL_AGENT_KEY") != "" && !explicitProfile {
		fmt.Fprintf(io_.stderr, "warning: SENTINEL_AGENT_KEY is set in your environment — commands will use the SENTINEL_URL/SENTINEL_AGENT_KEY pair instead of profile %q until you unset it (or pass --profile)\n", profile)
	} else if os.Getenv("SENTINEL_URL") != "" {
		fmt.Fprintf(io_.stderr, "warning: SENTINEL_URL is set in your environment; commands ignore it without SENTINEL_AGENT_KEY, but unset it to avoid confusion (profile %q is what will be used)\n", profile)
	}

	// URL: flag > existing profile's URL > env > prompt.
	candidate := firstNonEmpty(*urlFlag, g.URL)
	existing, profileExists := cf.Profiles[profile]
	if candidate == "" {
		candidate = existing.URL
	}
	if candidate == "" {
		candidate = os.Getenv("SENTINEL_URL")
	}
	if *urlFlag == "" && g.URL == "" && io_.isTTY {
		prompt := "Sentinel URL"
		if candidate != "" {
			prompt += " [" + candidate + "]"
		}
		fmt.Fprint(io_.stderr, prompt+": ")
		line, err := readLine(io_.stdin)
		if err != nil {
			fmt.Fprintln(io_.stderr, "\nerror: reading URL:", err)
			return ExitUsage
		}
		candidate = firstNonEmpty(line, candidate)
	}
	if candidate == "" {
		fmt.Fprintln(io_.stderr, "error: no URL — pass --url https://sentinel.example.com")
		return ExitUsage
	}
	baseURL, err := normalizeURL(candidate)
	if err != nil {
		fmt.Fprintln(io_.stderr, "error:", err)
		return ExitUsage
	}

	// confirmReplace asks before pointing an existing profile at a different URL.
	confirmReplace := func() bool {
		if !profileExists || existing.URL == "" || existing.URL == baseURL || *yes {
			return true
		}
		if !io_.isTTY {
			fmt.Fprintf(io_.stderr, "error: profile %q already points to %s; pass --yes to replace it with %s (nothing was saved)\n", profile, existing.URL, baseURL)
			return false
		}
		fmt.Fprintf(io_.stderr, "Profile %q currently points to %s. Replace with %s? [y/N]: ", profile, existing.URL, baseURL)
		ans, _ := readLine(io_.stdin)
		if a := strings.ToLower(ans); a != "y" && a != "yes" {
			fmt.Fprintln(io_.stderr, "nothing was saved")
			return false
		}
		return true
	}

	// Key: pasted (--key-stdin), or minted by the browser device flow.
	var key string
	var minted *deviceToken
	if *keyStdin {
		if io_.isTTY {
			fmt.Fprint(io_.stderr, "Agent key (input hidden): ")
			key, err = io_.readSecret(io_.stdin)
		} else {
			key, err = readLine(io_.stdin)
		}
		if err != nil {
			fmt.Fprintln(io_.stderr, "error: reading key:", err)
			return ExitUsage
		}
		if key == "" {
			fmt.Fprintln(io_.stderr, "error: empty agent key")
			return ExitUsage
		}
	} else {
		// Ask BEFORE a key is minted, so declining never strands a live key. (A fresh machine has no profile to
		// replace, so the server-suggested name below can never need this.)
		if !confirmReplace() {
			return ExitUsage
		}
		name := strings.TrimSpace(*deviceName)
		if name == "" {
			name = defaultDeviceName()
		}
		requested := profile
		if suggestable {
			requested = ""
		}
		var code int
		minted, code = runDeviceFlow(ctx, baseURL, name, requested, *noBrowser, io_)
		if code != ExitOK {
			return code
		}
		key = minted.Key
	}

	// Verify BEFORE writing.
	client := NewClient(baseURL, key)
	res, err := client.Do(ctx, "GET", "/api/agent/self", nil, nil)
	if err != nil {
		fmt.Fprintln(io_.stderr, "error:", strings.ReplaceAll(err.Error(), key, "***"))
		fmt.Fprintln(io_.stderr, "nothing was saved")
		return ExitNetwork
	}
	if code := exitCodeForStatus(res.Status); code != ExitOK {
		fmt.Fprintf(io_.stderr, "error: server returned %d: %s\nnothing was saved\n", res.Status, errorMessage(res.Body))
		return code
	}
	var self selfIdentity
	if err := json.Unmarshal(res.Body, &self); err != nil {
		fmt.Fprintln(io_.stderr, "error: unexpected response from /api/agent/self (is this a Sentinel URL?):", err)
		return ExitNetwork
	}
	suggestion := self.CLIProfile
	if minted != nil && minted.CLIProfile != "" {
		suggestion = minted.CLIProfile
	}
	if suggestable && suggestion != "" && validateProfileName(suggestion) == nil {
		profile = suggestion
		existing, profileExists = cf.Profiles[profile]
	}

	// Replacing an existing profile's URL needs consent (asked before minting in the browser flow).
	if minted == nil && !confirmReplace() {
		return ExitUsage
	}

	first := len(cf.Profiles) == 0
	prefix := displayPrefix(self.Key.Prefix, key)
	stored := Profile{URL: baseURL, Key: key, KeyPrefix: prefix, Kind: self.Kind}
	if self.Owner != nil {
		stored.Owner = self.Owner.Email
	}
	cf.Profiles[profile] = stored
	if first || *use || cf.CurrentProfile == "" && len(cf.Profiles) == 1 {
		cf.CurrentProfile = profile
	} else if cf.CurrentProfile == "" && cf.Legacy {
		cf.CurrentProfile = defaultProfile // keep what the legacy file meant
	}
	if err := saveConfigFile(cf); err != nil {
		fmt.Fprintln(io_.stderr, "error:", err)
		return ExitNetwork
	}
	cur := ""
	if cf.CurrentProfile == profile {
		cur = " (current)"
	}
	fmt.Fprintf(io_.stdout, "Logged in to %s\n", baseURL)
	if self.Owner != nil {
		access := "read-only"
		if self.Owner.CanWrite {
			access = "read-write"
		}
		fmt.Fprintf(io_.stdout, "as: %s (%s, %s) — a personal key: it can do what you can do in the dashboard, nothing more\n", self.Owner.display(), self.Owner.Role, access)
	} else {
		fmt.Fprintf(io_.stdout, "agent: %s\n", self.Name)
	}
	org := self.OrganizationID
	if minted != nil && minted.OrgSlug != "" {
		org = minted.OrgSlug
	}
	expires := ""
	if self.Key.ExpiresAt != nil && *self.Key.ExpiresAt != "" {
		expires = " (expires " + (*self.Key.ExpiresAt)[:min(10, len(*self.Key.ExpiresAt))] + ")"
	}
	fmt.Fprintf(io_.stdout, "organization: %s\nprofile: %s%s\nkey: %s%s\nsaved: %s (mode 0600)\n", org, profile, cur, prefix, expires, path)
	if cf.CurrentProfile != profile {
		fmt.Fprintf(io_.stdout, "note: %q is not the current profile; use `sentinel --profile %s ...` or `sentinel profile use %s`\n", profile, profile, profile)
	}
	fmt.Fprintln(io_.stdout, "next: sentinel whoami   |   sentinel agent install")
	return ExitOK
}

// selfIdentity is the part of GET /api/agent/self login and whoami read.
type selfIdentity struct {
	Name           string     `json:"name"`
	OrganizationID string     `json:"organizationId"`
	CLIProfile     string     `json:"cliProfile"`
	Kind           string     `json:"kind"`
	Owner          *selfOwner `json:"owner"`
	Key            struct {
		Prefix    string  `json:"prefix"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"key"`
}

// selfOwner is the person a personal key acts for (null for an org agent).
type selfOwner struct {
	Name     *string `json:"name"`
	Email    string  `json:"email"`
	Role     string  `json:"role"`
	CanWrite bool    `json:"canWrite"`
}

func (o *selfOwner) display() string {
	if o.Name != nil && strings.TrimSpace(*o.Name) != "" {
		return fmt.Sprintf("%s <%s>", strings.TrimSpace(*o.Name), o.Email)
	}
	return o.Email
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeJSONAtomic marshals v to path via a 0600 temp file in the same directory + rename, creating the
// directory 0700.
func writeJSONAtomic(path string, v interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}

// cmdConfig implements `sentinel config`: the profile in use and where the url and key each come from.
// Prints the URL's value but only the SOURCE of the key.
func cmdConfig(g cfgFlags, stdout, stderr io.Writer) int {
	r, err := resolveConfigSources(g, func(m string) { fmt.Fprintln(stderr, m) })
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}
	writeConfigSummary(stdout, r)
	path := mustConfigPath()
	state := "missing"
	if _, err := os.Stat(path); err == nil {
		state = "present"
	}
	fmt.Fprintf(stdout, "file:    %s (%s)\n", path, state)
	if r.URL == "" || r.Key == "" {
		fmt.Fprintln(stdout, "run `sentinel login` to configure")
	}
	return ExitOK
}

// writeConfigSummary prints profile/url/key provenance; never the key.
func writeConfigSummary(w io.Writer, r Resolved) {
	prof := r.Profile
	if prof == "" {
		prof = "- (url+key come from " + r.URLSource + ")"
	} else {
		prof += " (selected by " + r.ProfileSource + ")"
	}
	fmt.Fprintf(w, "profile: %s\n", prof)
	fmt.Fprintf(w, "url:     %s (from %s)\n", firstNonEmpty(r.URL, "-"), r.URLSource)
	fmt.Fprintf(w, "key:     %s (from %s)\n", map[bool]string{true: "set", false: "not set"}[r.Key != ""], r.KeySource)
	switch {
	case r.Owner != "":
		fmt.Fprintf(w, "identity: personal key of %s (sentinel login)\n", r.Owner)
	case r.Kind != "":
		fmt.Fprintf(w, "identity: org agent key (%s)\n", r.Kind)
	}
}

// cmdProfile implements `sentinel profile list|use|remove`.
func cmdProfile(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: sentinel profile list | use <name> | remove <name>")
		return ExitUsage
	}
	if len(args) == 0 {
		return usage()
	}
	cf, err := loadConfigFile(func(m string) { fmt.Fprintln(stderr, m) })
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage()
		}
		if len(cf.Profiles) == 0 {
			fmt.Fprintln(stdout, "no profiles — run `sentinel login --profile <name>`")
			return ExitOK
		}
		cur := cf.effectiveCurrent()
		for _, n := range cf.names() {
			p := cf.Profiles[n]
			mark := " "
			if n == cur {
				mark = "*"
			}
			key := "no key"
			if p.Key != "" {
				key = displayPrefix(p.KeyPrefix, p.Key)
			}
			fmt.Fprintf(stdout, "%s %-20s %s  (%s)\n", mark, n, firstNonEmpty(p.URL, "-"), key)
		}
		return ExitOK
	case "use", "remove":
		if len(args) != 2 {
			return usage()
		}
		name := args[1]
		if _, ok := cf.Profiles[name]; !ok {
			fmt.Fprintf(stderr, "error: unknown profile %q; profiles that exist: %s\n", name, listOrNone(cf.names()))
			return ExitUsage
		}
		if args[0] == "use" {
			cf.CurrentProfile = name
			fmt.Fprintf(stdout, "current profile: %s\n", name)
		} else {
			wasCurrent := cf.effectiveCurrent() == name
			delete(cf.Profiles, name)
			fmt.Fprintf(stdout, "removed profile %s\n", name)
			if wasCurrent {
				cf.CurrentProfile = ""
				if names := cf.names(); len(names) > 0 {
					cf.CurrentProfile = names[0]
					fmt.Fprintf(stdout, "current profile: %s\n", names[0])
				}
			}
		}
		if err := saveConfigFile(cf); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return ExitNetwork
		}
		return ExitOK
	}
	return usage()
}

// cmdLogout implements `sentinel logout [--profile P]`: revokes the profile's key on the server (POST
// /api/agent/key/revoke -- the key can only revoke ITSELF) and removes the profile. A personal key is revoked; an org agent
// key is refused by the server (an admin retires those) and stays valid, which is said plainly. A key the server already
// rejects (401) is just removed locally. On a network error nothing is removed: the key may still be live, and dropping
// the only local copy would leave a live key nobody can revoke from this machine.
func cmdLogout(ctx context.Context, args []string, g cfgFlags, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profFlag := fs.String("profile", "", "profile to log out of (default: the selected profile)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: sentinel logout [--profile P]")
		return ExitUsage
	}
	cf, err := loadConfigFile(func(m string) { fmt.Fprintln(stderr, m) })
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}
	name := firstNonEmpty(*profFlag, g.Profile, os.Getenv("SENTINEL_PROFILE"), cf.effectiveCurrent())
	prof, ok := cf.Profiles[name]
	if !ok {
		fmt.Fprintf(stderr, "error: unknown profile %q; profiles that exist: %s\n", name, listOrNone(cf.names()))
		return ExitUsage
	}

	revoked := false
	if prof.URL != "" && prof.Key != "" {
		res, err := NewClient(prof.URL, prof.Key).Do(ctx, "POST", "/api/agent/key/revoke", nil, nil)
		if err != nil {
			fmt.Fprintln(stderr, "error: could not reach the server to revoke the key:", strings.ReplaceAll(err.Error(), prof.Key, "***"))
			fmt.Fprintf(stderr, "nothing was removed; retry, or drop it locally only with `sentinel profile remove %s`\n", name)
			return ExitNetwork
		}
		switch {
		case res.Status >= 200 && res.Status < 300:
			revoked = true
		case res.Status == 401:
			fmt.Fprintln(stderr, "note: the server no longer accepts this key (expired or already revoked)")
		case res.Status == 403:
			fmt.Fprintf(stderr, "warning: %s\nthe key was NOT revoked and stays valid; ask an org owner/admin to revoke it if it should stop working\n", errorMessage(res.Body))
		default:
			fmt.Fprintf(stderr, "error: server returned %d: %s\nnothing was removed\n", res.Status, errorMessage(res.Body))
			return exitCodeForStatus(res.Status)
		}
	}

	wasCurrent := cf.effectiveCurrent() == name
	delete(cf.Profiles, name)
	if wasCurrent {
		cf.CurrentProfile = ""
		if names := cf.names(); len(names) > 0 {
			cf.CurrentProfile = names[0]
		}
	}
	if err := saveConfigFile(cf); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitNetwork
	}
	if revoked {
		fmt.Fprintf(stdout, "revoked key %s on %s\n", displayPrefix(prof.KeyPrefix, prof.Key), prof.URL)
	}
	fmt.Fprintf(stdout, "removed profile %s\n", name)
	if wasCurrent && cf.CurrentProfile != "" {
		fmt.Fprintf(stdout, "current profile: %s\n", cf.CurrentProfile)
	}
	return ExitOK
}

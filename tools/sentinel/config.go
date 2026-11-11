package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Config is the resolved {URL, Key} pair a command runs against; see resolveConfig for how it is
// chosen (flags, an env credential PAIR, or a named profile in the config file). The key is never
// logged or printed anywhere in this program — see keyPrefix in output.go for the only derived form
// that is safe to print (whoami).
type Config struct {
	URL string `json:"url"`
	Key string `json:"agent_key"`
	// KeyPrefix is the display prefix the server reported for Key at login (GET /api/agent/self).
	KeyPrefix string `json:"key_prefix,omitempty"`
	// Kind and Owner are what GET /api/agent/self reported at login: kind 'personal' + the owner's email for a
	// `sentinel login` key, or the org agent's kind ('ai'/'bot') with no owner. Display only (`sentinel config`); the
	// server re-decides what the key may do on every request.
	Kind  string `json:"kind,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// Profile is one named {url, agent_key} entry in the config file.
type Profile = Config

// ConfigFile is the on-disk shape: {"current_profile": "...", "profiles": {name: {url, agent_key}}}.
// The pre-profile flat {"url","agent_key"} file still loads, as a profile named "default"; the first
// write migrates it to this shape.
type ConfigFile struct {
	CurrentProfile string             `json:"current_profile,omitempty"`
	Profiles       map[string]Profile `json:"profiles,omitempty"`
	Legacy         bool               `json:"-"` // loaded from the flat shape
}

const defaultProfile = "default"

var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validateProfileName(n string) error {
	if !profileNameRE.MatchString(n) {
		return fmt.Errorf("invalid profile name %q: use [a-z0-9][a-z0-9_-]{0,31}", n)
	}
	return nil
}

func (c *ConfigFile) names() []string {
	out := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// effectiveCurrent is the profile used when nothing selects one explicitly.
func (c *ConfigFile) effectiveCurrent() string {
	if c.CurrentProfile != "" {
		return c.CurrentProfile
	}
	return defaultProfile
}

// configPath resolves $XDG_CONFIG_HOME/sentinel/config.json, falling back to
// ~/.config/sentinel/config.json per the XDG base directory spec.
func configPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "sentinel", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", "sentinel", "config.json"), nil
}

// stateDir resolves $XDG_STATE_HOME/sentinel, falling back to ~/.local/state/sentinel.
func stateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "sentinel"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "sentinel"), nil
}

// loadConfigFile reads the config file if present. A missing file is not an error — flags/env
// may supply everything. Warns (to stderr, via warn) if the file's permissions are more
// permissive than 0600: the key inside it is a bearer credential, so a world/group-readable
// config file is a real exposure, not decoration.
func loadConfigFile(warn func(string)) (*ConfigFile, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	cf := &ConfigFile{Profiles: map[string]Profile{}}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cf, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// Permission bits are not meaningful on Windows; skip the check there.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			warn(fmt.Sprintf("warning: config file %s is readable by group/other (mode %04o) — it contains a bearer credential; run `chmod 600 %s`", path, mode, path))
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw struct {
		ConfigFile
		URL string `json:"url"`
		Key string `json:"agent_key"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cf.CurrentProfile = raw.CurrentProfile
	if len(raw.Profiles) > 0 {
		cf.Profiles = raw.Profiles
	} else if raw.URL != "" || raw.Key != "" {
		cf.Profiles[defaultProfile] = Profile{URL: raw.URL, Key: raw.Key}
		cf.Legacy = true
	}
	return cf, nil
}

// saveConfigFile writes cf in the profiles shape, atomically, 0600 (dir 0700).
func saveConfigFile(cf *ConfigFile) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, cf)
}

// cfgFlags are the global command-line selectors.
type cfgFlags struct{ URL, Key, Profile string }

// Resolved is the outcome of resolveConfigSources: the values plus where each came from.
type Resolved struct {
	Config
	KeyPrefix     string // server-reported prefix, when the profile stored one
	Profile       string // profile whose values were used ("" when an env pair or flags supplied both)
	ProfileSource string // flag | env | file | default
	URLSource     string // flag | env pair | profile <name> | unset
	KeySource     string
}

// Source labels.
const (
	sourceUnset = "unset"
	sourceFlag  = "flag"
	sourceEnv   = "env pair"
)

func profileSource(name string) string { return "profile " + name }

// resolveConfig is the strict form: url and key must both resolve.
func resolveConfig(f cfgFlags, warn func(string)) (Resolved, error) {
	r, err := resolveConfigSources(f, warn)
	if err != nil {
		return Resolved{}, err
	}
	if r.URL == "" {
		return Resolved{}, fmt.Errorf("no server URL configured for profile %q: run `sentinel login`, or pass -url", r.Profile)
	}
	if r.Key == "" {
		return Resolved{}, fmt.Errorf("no agent key configured for profile %q: run `sentinel login`, or pass -key", r.Profile)
	}
	return r, nil
}

// resolveConfigSources applies the precedence rules (they exist so a stale SENTINEL_URL in a shell can
// never be silently paired with a different environment's key):
//
//  1. -url / -key flags win, per field.
//  2. The profile is chosen: --profile > SENTINEL_PROFILE > current_profile in the file > "default".
//     A profile that is named but does not exist is an error (exit 2).
//  3. URL and key are NEVER taken from different sources. Env credentials are used only as the PAIR
//     SENTINEL_URL + SENTINEL_AGENT_KEY (a missing SENTINEL_URL is an error) — unless a profile was
//     selected EXPLICITLY (--profile / SENTINEL_PROFILE), which beats env credentials.
//  4. SENTINEL_URL without SENTINEL_AGENT_KEY is ignored, with a warning.
func resolveConfigSources(f cfgFlags, warn func(string)) (Resolved, error) {
	cf, err := loadConfigFile(warn)
	if err != nil {
		return Resolved{}, err
	}

	explicit, explicitSrc := f.Profile, sourceFlag
	if explicit == "" {
		explicit, explicitSrc = os.Getenv("SENTINEL_PROFILE"), "env"
	}
	name, nameSrc := explicit, explicitSrc
	if name == "" {
		if cf.CurrentProfile != "" {
			name, nameSrc = cf.CurrentProfile, "file"
		} else {
			name, nameSrc = defaultProfile, "default"
		}
	}
	if err := validateProfileName(name); err != nil {
		return Resolved{}, err
	}
	prof, exists := cf.Profiles[name]
	if !exists && nameSrc != "default" {
		return Resolved{}, fmt.Errorf("unknown profile %q (selected by %s); profiles that exist: %s", name, nameSrc, listOrNone(cf.names()))
	}

	envURL, envKey := os.Getenv("SENTINEL_URL"), os.Getenv("SENTINEL_AGENT_KEY")
	r := Resolved{Profile: name, ProfileSource: nameSrc, URLSource: sourceUnset, KeySource: sourceUnset}

	if envKey != "" && explicit == "" {
		if envURL == "" && f.URL == "" {
			return Resolved{}, fmt.Errorf("SENTINEL_AGENT_KEY is set but SENTINEL_URL is not: env credentials are used only as a pair (set SENTINEL_URL too, unset SENTINEL_AGENT_KEY, or pass --profile)")
		}
		r.Profile, r.ProfileSource = "", ""
		if envURL != "" {
			r.URL, r.URLSource = envURL, sourceEnv
		}
		r.Key, r.KeySource = envKey, sourceEnv
	} else {
		if exists {
			r.URL, r.Key, r.KeyPrefix = prof.URL, prof.Key, prof.KeyPrefix
			r.Kind, r.Owner = prof.Kind, prof.Owner
			if prof.URL != "" {
				r.URLSource = profileSource(name)
			}
			if prof.Key != "" {
				r.KeySource = profileSource(name)
			}
		}
		if envURL != "" && envKey == "" {
			warn(fmt.Sprintf("warning: SENTINEL_URL (%s) is set in your environment but SENTINEL_AGENT_KEY is not — ignoring it and using profile %q (%s); unset SENTINEL_URL, or set SENTINEL_AGENT_KEY too to use the environment pair", envURL, name, firstNonEmpty(prof.URL, "no url")))
		}
	}

	if f.URL != "" {
		r.URL, r.URLSource = f.URL, sourceFlag
	}
	if f.Key != "" {
		r.Key, r.KeySource = f.Key, sourceFlag
		r.Kind, r.Owner = "", "" // what the profile recorded is about ITS key, not this one
	}
	return r, nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "(none — run `sentinel login`)"
	}
	return strings.Join(names, ", ")
}

func mustConfigPath() string {
	p, err := configPath()
	if err != nil {
		return "$XDG_CONFIG_HOME/sentinel/config.json"
	}
	return p
}

// keyPrefix returns a short, safe-to-print prefix of the key for identification purposes (e.g.
// `whoami`). Never returns enough of the key to be useful to an attacker, and this is the ONLY
// place in the program that is allowed to derive a printable form of the key.
func keyPrefix(key string) string {
	const n = 14
	if len(key) <= n {
		return "***"
	}
	return key[:n] + "…"
}

// stateKeyTag is the 8-char tag that names the events-follow cursor file. Kept separate from
// keyPrefix (which is for display) so changing what is displayed never orphans a saved cursor.
func stateKeyTag(key string) string {
	const n = 8
	if len(key) <= n {
		return "***"
	}
	return key[:n] + "..."
}

// displayPrefix prefers the key prefix the server reported at login (stored in the profile) and falls
// back to a short derived prefix for profiles written before it was stored. Never more than that.
func displayPrefix(stored, key string) string {
	if stored != "" {
		if len(stored) > 16 {
			stored = stored[:16]
		}
		return stored
	}
	return keyPrefix(key)
}

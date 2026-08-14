package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the resolved {URL, Key} pair a command runs against, after applying precedence:
// command-line flags > SENTINEL_URL/SENTINEL_AGENT_KEY environment variables > config file
// ($XDG_CONFIG_HOME/sentinel/config.json, or ~/.config/sentinel/config.json if XDG_CONFIG_HOME is
// unset). The key is never logged or printed anywhere in this program — see keyPrefix in
// output.go for the only derived form that is safe to print (whoami).
type Config struct {
	URL string `json:"url"`
	Key string `json:"agent_key"`
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
func loadConfigFile(warn func(string)) (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("stat %s: %w", path, err)
	}

	// Permission bits are not meaningful on Windows; skip the check there.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			warn(fmt.Sprintf("warning: config file %s is readable by group/other (mode %04o) — it contains a bearer credential; run `chmod 600 %s`", path, mode, path))
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// resolveConfig applies the documented precedence: flags > env > config file. Either flag value
// being empty means "not set on the command line" — flag.String defaults are always "".
func resolveConfig(flagURL, flagKey string, warn func(string)) (Config, error) {
	fileCfg, err := loadConfigFile(warn)
	if err != nil {
		return Config{}, err
	}

	cfg := fileCfg
	if envURL := os.Getenv("SENTINEL_URL"); envURL != "" {
		cfg.URL = envURL
	}
	if envKey := os.Getenv("SENTINEL_AGENT_KEY"); envKey != "" {
		cfg.Key = envKey
	}
	if flagURL != "" {
		cfg.URL = flagURL
	}
	if flagKey != "" {
		cfg.Key = flagKey
	}

	if cfg.URL == "" {
		return Config{}, fmt.Errorf("no server URL configured: pass -url, set SENTINEL_URL, or add \"url\" to %s", mustConfigPath())
	}
	if cfg.Key == "" {
		return Config{}, fmt.Errorf("no agent key configured: pass -key, set SENTINEL_AGENT_KEY, or add \"agent_key\" to %s", mustConfigPath())
	}
	return cfg, nil
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
	const n = 8
	if len(key) <= n {
		return "***"
	}
	return key[:n] + "..."
}

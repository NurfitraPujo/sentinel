package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The skill and guide are embedded so `sentinel agent install` / `sentinel guide` work with no
// checkout. Canonical sources and the byte-identical copies are managed by
// scripts/sync-cli-embeds.sh (CI fails on drift).
var (
	//go:embed assets/SKILL.md
	embeddedSkill []byte
	//go:embed assets/GUIDE.md
	embeddedGuide []byte
)

const (
	skillName    = "sentinel-agent"
	markerPrefix = "<!-- installed by sentinel "
)

// agentTarget is one coding agent's GLOBAL (user-level) skill location.
type agentTarget struct {
	flag     string // CLI flag name and display name
	homeDir  string // relative to $HOME; its existence means the tool is installed
	skillDir string // relative to $HOME; parent of <skillName>/SKILL.md
}

// Paths:
//   - claude: ~/.claude/skills/<name>/SKILL.md — Claude Code's documented personal skills dir.
//   - agy:    ~/.gemini/config/skills/<name>/SKILL.md — verified against the installed agy binary
//     (its migration/help text names `~/.gemini/config/skills/<name>/SKILL.md` as the global skills path).
//   - codex:  ~/.agents/skills/<name>/SKILL.md — Codex's documented user-level skills dir. UNVERIFIED on
//     this tooling: no codex binary was available to confirm it.
var agentTargets = []agentTarget{
	{flag: "claude", homeDir: ".claude", skillDir: filepath.Join(".claude", "skills")},
	{flag: "codex", homeDir: ".codex", skillDir: filepath.Join(".agents", "skills")},
	{flag: "agy", homeDir: ".gemini", skillDir: filepath.Join(".gemini", "config", "skills")},
}

// renderSkill inserts the provenance marker right after the YAML frontmatter (frontmatter must stay
// on line 1 for skill loaders).
func renderSkill(ver string) []byte {
	marker := fmt.Sprintf("%s%s; do not edit — re-run `sentinel agent install` to update, `sentinel agent install --uninstall` to remove -->\n", markerPrefix, ver)
	s := string(embeddedSkill)
	const fence = "\n---\n"
	if strings.HasPrefix(s, "---\n") {
		if i := strings.Index(s[3:], fence); i >= 0 {
			cut := 3 + i + len(fence)
			return []byte(s[:cut] + marker + s[cut:])
		}
	}
	return []byte(marker + s)
}

func isMarked(content []byte) bool {
	for _, l := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(l, markerPrefix) {
			return true
		}
	}
	return false
}

func cmdGuide(stdout io.Writer) int {
	_, _ = stdout.Write(embeddedGuide)
	return ExitOK
}

// cmdAgent dispatches `sentinel agent <subcommand>`; only `install` exists.
func cmdAgent(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprintln(stderr, "usage: sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]")
		return ExitUsage
	}
	return cmdAgentInstall(args[1:], stdout, stderr)
}

func cmdAgentInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	chosen := map[string]*bool{}
	for _, t := range agentTargets {
		chosen[t.flag] = fs.Bool(t.flag, false, "install for "+t.flag)
	}
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	force := fs.Bool("force", false, "overwrite an existing skill file that sentinel did not install")
	uninstall := fs.Bool("uninstall", false, "remove skill files previously installed by sentinel")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]")
		return ExitUsage
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "error: resolving home directory:", err)
		return ExitUsage
	}

	var targets []agentTarget
	explicit := false
	for _, t := range agentTargets {
		if *chosen[t.flag] {
			explicit = true
			targets = append(targets, t)
		}
	}
	if !explicit {
		for _, t := range agentTargets {
			if st, err := os.Stat(filepath.Join(home, t.homeDir)); err == nil && st.IsDir() {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintln(stderr, "error: none of ~/.claude, ~/.codex, ~/.gemini exist; pass --claude, --codex and/or --agy to install anyway")
			return ExitUsage
		}
	}

	ver := buildVersion()
	want := renderSkill(ver)
	rc := ExitOK
	for _, t := range targets {
		dir := filepath.Join(home, t.skillDir, skillName)
		file := filepath.Join(dir, "SKILL.md")
		var code int
		if *uninstall {
			code = uninstallOne(t.flag, dir, file, *dry, stdout, stderr)
		} else {
			code = installOne(t.flag, dir, file, want, *dry, *force, stdout, stderr)
		}
		if code != ExitOK {
			rc = code
		}
	}
	return rc
}

func installOne(name, dir, file string, want []byte, dry, force bool, stdout, stderr io.Writer) int {
	have, err := os.ReadFile(file)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "%s: error reading %s: %v\n", name, file, err)
		return ExitNetwork
	}
	switch {
	case exists && bytes.Equal(have, want):
		fmt.Fprintf(stdout, "%s: up to date  %s\n", name, file)
		return ExitOK
	case exists && !isMarked(have) && !force:
		fmt.Fprintf(stderr, "%s: REFUSING to overwrite %s — it was not installed by sentinel (no provenance marker).\n", name, file)
		fmt.Fprintf(stderr, "    would change: %d bytes / %d lines -> %d bytes / %d lines; re-run with --force to replace it\n",
			len(have), bytes.Count(have, []byte("\n")), len(want), bytes.Count(want, []byte("\n")))
		return ExitConflict
	}
	verb := "wrote"
	if exists {
		verb = "updated"
	}
	if dry {
		fmt.Fprintf(stdout, "%s: would write (%s)  %s\n", name, map[bool]string{true: "update", false: "create"}[exists], file)
		return ExitOK
	}
	if err := writeFileAtomic(dir, file, want); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", name, err)
		return ExitNetwork
	}
	fmt.Fprintf(stdout, "%s: %s  %s\n", name, verb, file)
	return ExitOK
}

func uninstallOne(name, dir, file string, dry bool, stdout, stderr io.Writer) int {
	have, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "%s: not installed  %s\n", name, file)
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: error reading %s: %v\n", name, file, err)
		return ExitNetwork
	}
	if !isMarked(have) {
		fmt.Fprintf(stderr, "%s: leaving %s alone — not installed by sentinel (no provenance marker)\n", name, file)
		return ExitConflict
	}
	if dry {
		fmt.Fprintf(stdout, "%s: would remove  %s\n", name, file)
		return ExitOK
	}
	if err := os.Remove(file); err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", name, err)
		return ExitNetwork
	}
	_ = os.Remove(dir) // only succeeds when empty
	fmt.Fprintf(stdout, "%s: removed  %s\n", name, file)
	return ExitOK
}

func writeFileAtomic(dir, file string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".SKILL-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

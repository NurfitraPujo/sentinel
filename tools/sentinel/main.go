// Command sentinel is a provider-agnostic CLI over the Sentinel dashboard's agent API
// (/api/agent/*, apps/dashboard-web/src/routes/api/agent/**). It is a triage tool for AI coding
// agents (or humans scripting around them): list/inspect issues, claim/release/resolve them, post
// comments/questions/progress notes, link related issues, tail the org's activity feed, and batch
// several mutations into one HTTP round trip.
//
// It is a standalone Go module (see go.mod) with zero dependencies beyond the standard library
// and zero imports from any other module in this repository, deliberately: this is meant to be
// `go install`-able on its own
// (go install github.com/NurfitraPujo/sentinel/tools/sentinel@latest, or the dashboard's /install.sh), by an agent that has never cloned the Sentinel monorepo.
//
// # Configuration
//
// The server URL and agent key come from ONE place — never mixed across sources (a stale SENTINEL_URL
// in a shell once got paired with another environment's key). Highest priority first:
//
//  1. -url / -key command-line flags (per field)
//  2. an explicit profile: --profile P, or SENTINEL_PROFILE (beats env credentials, like the AWS CLI)
//  3. the env PAIR SENTINEL_URL + SENTINEL_AGENT_KEY (SENTINEL_URL alone is ignored with a warning)
//  4. a profile from $XDG_CONFIG_HOME/sentinel/config.json (or ~/.config/sentinel/config.json), written by
//     `sentinel login`: {"current_profile": "production", "profiles": {"production": {"url": "...",
//     "agent_key": "sent_agent_..."}}}; the profile is current_profile, else "default". The old flat
//     {"url", "agent_key"} file still loads, as profile "default".
//
// The key is never logged or printed; see keyPrefix in config.go for the only derived form this
// program will ever emit.
//
// # Usage
//
//	sentinel [--profile P] [-url URL] [-key KEY] [-format json|table] <command> [args...]
//
//	sentinel issues list [--type user_report|system_error] [--claimed true|false] [--project ID] [--waiting true]
//	sentinel issues get <issueId>                       ("issue" = "issues", "show" = "get")
//	sentinel issues occurrences <issueId> [--limit N] [--before TS] [--with-spans]
//	sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]
//	sentinel spans pull <traceId|occurrenceId> [--wait] [--timeout 3m]
//	sentinel claim <issueId>
//	sentinel claim-next [--type T] [--project ID] [--environment E] [--source S] [--sort firstSeen|lastSeen]
//	sentinel release <issueId>
//	sentinel status <issueId> <unresolved|resolved|ignored> [--resolved-in VERSION]
//	sentinel comment <issueId> --body <md> [--parent <commentId>] [--attachment <id> ...]
//	sentinel comment edit <issueId> <commentId> --body <md>
//	sentinel comment delete <issueId> <commentId>
//	sentinel comments <issueId> [--after <ts>]
//	sentinel question <issueId> --body <md> --waiting-on <reporter|team>
//	sentinel progress <issueId> --body <md>
//	sentinel severity <issueId> <low|medium|high|critical>
//	sentinel link <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>
//	sentinel unlink <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>
//	sentinel projects
//	sentinel members list [--role <role>]
//	sentinel members add <email> [--role <role>]
//	sentinel members remove <memberId>
//	sentinel whoami
//	sentinel version | --version
//	sentinel login [--url U] [--profile P] [--device-name N] [--no-browser]   (browser approval; a personal key)
//	sentinel login [--url U] [--profile P] --key-stdin                       (paste an org agent key; CI)
//	sentinel logout [--profile P]
//	sentinel config
//	sentinel profile list | use <name> | remove <name>
//	sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]
//	sentinel guide
//	sentinel help [command]          (or: sentinel <command> --help)
//	sentinel key rotate
//	sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me] [--follow] [--interval SEC]
//	sentinel batch -f ops.json [--stop-on-error=false]
//	sentinel upload <file> --issue <id> [--comment <text>]
//	sentinel upload <issueId> <file>   (deprecated two-positional form, issueId ignored)
//
// Every id argument is checked to be a UUID before any request (ids.go), and -h/--help anywhere prints the command's
// usage instead of running it (help.go).
//
// Exit codes: 0 ok, 1 network/server error, 2 usage error, 3 auth failure (401/403), 4 not found
// (404), 5 conflict (409), 6 validation error (400/422). On any non-zero exit the server's error
// message (when there was a server response) is printed to stderr. `spans pull --wait` exits 4 when the pull
// finishes with no spans and 1 when it times out.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// tick is a seam for tests: production waits intervalSeconds between polls; tests replace this to
// fire immediately (bounded by iteration count) rather than sleeping for real.
var tick = func(intervalSeconds int) <-chan time.Time {
	return time.After(time.Duration(intervalSeconds) * time.Second)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sentinel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "Sentinel server base URL (overrides SENTINEL_URL and the config file)")
	keyFlag := fs.String("key", "", "Sentinel agent key (overrides SENTINEL_AGENT_KEY and the config file)")
	profileFlag := fs.String("profile", "", "config profile to use (overrides SENTINEL_PROFILE and current_profile)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	format := fs.String("format", "json", `output format: "json" (default) or "table" (list-shaped commands only)`)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: sentinel [-url URL] [-key KEY] [-format json|table] <command> [args...]")
		fmt.Fprintln(stderr, "commands: login, logout, profile, config, version, guide, agent, issues (issue), claim, claim-next, release, status, comment, comments, question, progress, link, unlink, projects, members, whoami, key, events, batch, upload, spans, help")
		fmt.Fprintln(stderr, "run `sentinel <command> --help` for a command's usage")
	}

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, buildVersion())
		return ExitOK
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return ExitUsage
	}

	name, cmdArgs := rest[0], rest[1:]

	// Help is answered here, before any command runs (help.go): a command must never receive "--help" as an argument.
	if name == "help" {
		return printHelp(stdout, cmdArgs)
	}
	if helpRequested(cmdArgs) {
		if u, ok := usageFor(name, cmdArgs); ok {
			fmt.Fprintln(stdout, u)
			return ExitOK
		}
	}

	gflags := cfgFlags{URL: *urlFlag, Key: *keyFlag, Profile: *profileFlag}

	// Commands that must work BEFORE a url/key is configured (or that never touch the server).
	switch name {
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return ExitOK
	case "guide":
		return cmdGuide(stdout)
	case "agent":
		return cmdAgent(cmdArgs, stdout, stderr)
	case "profile":
		return cmdProfile(cmdArgs, stdout, stderr)
	case "config":
		return cmdConfig(gflags, stdout, stderr)
	case "login":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return cmdLogin(ctx, cmdArgs, gflags, stdLoginIO(stdout, stderr))
	case "logout":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return cmdLogout(ctx, cmdArgs, gflags, stdout, stderr)
	}

	fn, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n", name)
		fs.Usage()
		return ExitUsage
	}

	cfg, err := resolveConfig(gflags, func(msg string) { fmt.Fprintln(stderr, msg) })
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{
		client: NewClient(cfg.URL, cfg.Key),
		cfg:    cfg,
		format: *format,
		stdout: stdout,
		stderr: stderr,
	}
	return fn(ctx, e, cmdArgs)
}

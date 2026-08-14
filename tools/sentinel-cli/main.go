// Command sentinel is a provider-agnostic CLI over the Sentinel dashboard's agent API
// (/api/agent/*, apps/dashboard-web/src/routes/api/agent/**). It is a triage tool for AI coding
// agents (or humans scripting around them): list/inspect issues, claim/release/resolve them, post
// comments/questions/progress notes, link related issues, tail the org's activity feed, and batch
// several mutations into one HTTP round trip.
//
// It is a standalone Go module (see go.mod) with zero dependencies beyond the standard library
// and zero imports from any other module in this repository, deliberately: this is meant to be
// `go install`-able on its own, by an agent that has never cloned the Sentinel monorepo.
//
// # Configuration
//
// The server URL and agent key are resolved in this order, highest priority first:
//
//  1. -url / -key command-line flags
//  2. SENTINEL_URL / SENTINEL_AGENT_KEY environment variables
//  3. $XDG_CONFIG_HOME/sentinel/config.json (or ~/.config/sentinel/config.json):
//     {"url": "https://sentinel.example.com", "agent_key": "sk_..."}
//
// The key is never logged or printed; see keyPrefix in config.go for the only derived form this
// program will ever emit.
//
// # Usage
//
//	sentinel [-url URL] [-key KEY] [-format json|table] <command> [args...]
//
//	sentinel issues list [--type user_report|system_error] [--claimed true|false] [--project ID] [--waiting true]
//	sentinel issues get <issueId>
//	sentinel issues occurrences <issueId> [--limit N] [--before TS]
//	sentinel claim <issueId>
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
//	sentinel whoami
//	sentinel key rotate
//	sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me] [--follow] [--interval SEC]
//	sentinel batch -f ops.json [--stop-on-error=false]
//	sentinel upload <file> --issue <id> [--comment <text>]
//	sentinel upload <issueId> <file>   (deprecated two-positional form, issueId ignored)
//
// Exit codes: 0 ok, 1 network/server error, 2 usage error, 3 auth failure (401/403), 4 not found
// (404), 5 conflict (409), 6 validation error (400/422). On any non-zero exit the server's error
// message (when there was a server response) is printed to stderr.
package main

import (
	"context"
	"flag"
	"fmt"
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

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("sentinel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "Sentinel server base URL (overrides SENTINEL_URL and the config file)")
	keyFlag := fs.String("key", "", "Sentinel agent key (overrides SENTINEL_AGENT_KEY and the config file)")
	format := fs.String("format", "json", `output format: "json" (default) or "table" (list-shaped commands only)`)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: sentinel [-url URL] [-key KEY] [-format json|table] <command> [args...]")
		fmt.Fprintln(stderr, "commands: issues, claim, release, status, comment, comments, question, progress, link, unlink, projects, whoami, key, events, batch, upload")
	}

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return ExitUsage
	}

	name, cmdArgs := rest[0], rest[1:]
	fn, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n", name)
		fs.Usage()
		return ExitUsage
	}

	cfg, err := resolveConfig(*urlFlag, *keyFlag, func(msg string) { fmt.Fprintln(stderr, msg) })
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{
		client: NewClient(cfg.URL, cfg.Key),
		format: *format,
		stdout: stdout,
		stderr: stderr,
	}
	return fn(ctx, e, cmdArgs)
}

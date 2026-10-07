package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// commandUsage is each command's usage, printed by `sentinel <command> --help` / `-h` and `sentinel help <command>`.
// Help is decided HERE, centrally and before any command runs, because a command without a flag set of its own used
// to take "--help" as its positional id: `sentinel issues get --help` sent GET /api/agent/issues/--help (a server 500
// before the uuid route matcher). Keys are the command words as typed; an alias shares its target's entry.
var commandUsage = map[string]string{
	"issues": `usage: sentinel issues <list|get|show|occurrences|activity> ...   ("issue" is the same command)
  sentinel issues list [--type user_report|system_error] [--claimed true|false] [--project ID] [--waiting true]
                       [--since TS] [--sort firstSeen|lastSeen] [--limit N] [--cursor C]
  sentinel issues get <issueId>                      (alias: show)
  sentinel issues occurrences <issueId> [--limit N] [--before TS] [--with-spans]
  sentinel issues activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]`,
	"issues list":        "usage: sentinel issues list [--type user_report|system_error] [--claimed true|false] [--project ID] [--waiting true] [--since TS] [--sort firstSeen|lastSeen] [--limit N] [--cursor C]",
	"issues get":         "usage: sentinel issues get <issueId>   (alias: sentinel issue show <issueId>)",
	"issues occurrences": "usage: sentinel issues occurrences <issueId> [--limit N] [--before TS] [--with-spans]",
	"issues activity": `usage: sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]
  The issue's history, newest first: claims, status changes, comments, progress, regressions, occurrence bursts
  (a run of consecutive bursts is one line). --limit entries per page (server: default 50, max 200); --before the
  cursor a previous page printed; --all follows every page; --json prints the API response instead of the table.`,
	"claim":      "usage: sentinel claim <issueId>",
	"claim-next": "usage: sentinel claim-next [--type T] [--project ID] [--environment E] [--source S] [--sort firstSeen|lastSeen]",
	"release":    "usage: sentinel release <issueId>",
	"status":     "usage: sentinel status <issueId> <unresolved|resolved|ignored> [--resolved-in VERSION]",
	"comment": `usage: sentinel comment <issueId> --body <md> [--parent <commentId>] [--attachment <id> ...]
  sentinel comment edit <issueId> <commentId> --body <md>
  sentinel comment delete <issueId> <commentId>`,
	"comment edit":   "usage: sentinel comment edit <issueId> <commentId> --body <md>",
	"comment delete": "usage: sentinel comment delete <issueId> <commentId>",
	"comments":       "usage: sentinel comments <issueId> [--after <ts>]",
	"question":       "usage: sentinel question <issueId> --body <md> --waiting-on <reporter|team>",
	"progress":       "usage: sentinel progress <issueId> --body <md>",
	"severity":       "usage: sentinel severity <issueId> <low|medium|high|critical>",
	"link":           "usage: sentinel link <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>",
	"unlink":         "usage: sentinel unlink <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>",
	"projects":       "usage: sentinel projects",
	"members": `usage: sentinel members list [--role <role>]
  sentinel members add <email> [--role <role>]
  sentinel members remove <memberId>`,
	"whoami": "usage: sentinel whoami",
	"events": "usage: sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me] [--follow] [--interval SEC]\n  (one issue's history: sentinel issue activity <issueId>)",
	"batch":  "usage: sentinel batch -f ops.json [--stop-on-error=false]",
	"upload": `usage: sentinel upload <file> --issue <id> [--comment "text"]`,
	"key":    "usage: sentinel key rotate",
	"spans":  "usage: sentinel spans pull <traceId|occurrenceId> [--wait] [--timeout 3m]",
	"login": `usage: sentinel login [--url U] [--profile P] [--device-name N] [--no-browser]   (browser approval; a personal key)
  sentinel login [--url U] [--profile P] --key-stdin                       (paste an org agent key; CI)`,
	"logout":  "usage: sentinel logout [--profile P]",
	"config":  "usage: sentinel config",
	"profile": "usage: sentinel profile list | use <name> | remove <name>",
	"agent":   "usage: sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]",
	"guide":   "usage: sentinel guide",
	"version": "usage: sentinel version | --version",
}

// commandAliases maps an alias word to the command whose usage it shares.
var commandAliases = map[string]string{"issue": "issues", "show": "get"}

func canonicalWord(w string) string {
	if a, ok := commandAliases[w]; ok {
		return a
	}
	return w
}

// isHelpFlag reports whether a token asks for help.
func isHelpFlag(a string) bool {
	return a == "-h" || a == "-help" || a == "--help"
}

// helpRequested reports whether a command's arguments ask for help: a -h/-help/--help anywhere, or a bare "help" as
// the first word. A flag value that happens to be "--help" (`--body --help`) also counts -- printing help is the safe
// reading of an ambiguous line, never sending it.
func helpRequested(args []string) bool {
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	for _, a := range args {
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

// usageFor finds the most specific usage for `name args...`: "<name> <sub>" when the first argument is a known
// subcommand, else "<name>". ok is false for a command without an entry.
func usageFor(name string, args []string) (string, bool) {
	name = canonicalWord(name)
	if len(args) > 0 {
		if u, ok := commandUsage[name+" "+canonicalWord(args[0])]; ok {
			return u, true
		}
	}
	u, ok := commandUsage[name]
	return u, ok
}

// printHelp writes `sentinel help [command [sub]]`: one command's usage, or the list of commands.
func printHelp(w io.Writer, args []string) int {
	if len(args) > 0 {
		if u, ok := usageFor(args[0], args[1:]); ok {
			fmt.Fprintln(w, u)
			return ExitOK
		}
		fmt.Fprintf(w, "unknown command %q\n", args[0])
	}
	names := make([]string, 0, len(commandUsage))
	for k := range commandUsage {
		if !strings.Contains(k, " ") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	fmt.Fprintln(w, "usage: sentinel [--profile P] [-url URL] [-key KEY] [-format json|table] <command> [args...]")
	fmt.Fprintln(w, "commands: "+strings.Join(names, ", "))
	fmt.Fprintln(w, "run `sentinel <command> --help` for a command's usage; `sentinel guide` for the full agent guide")
	if len(args) > 0 {
		return ExitUsage
	}
	return ExitOK
}

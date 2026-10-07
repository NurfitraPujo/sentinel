package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Every id the agent API takes in a path is a UUID (issue, comment, occurrence, attachment). The server answers 404
// for anything else, but checking here gives a clear message without a round trip -- and, above all, never sends a
// flag-looking token as an id: `sentinel claim --help` once became GET .../issues/--help. There are no short-id or
// prefix forms in the API, so there is nothing else to accept. A member id may also be a user id, so `members remove`
// only refuses flag-looking values.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const exampleUUID = "3f2a6c1e-8d4b-4c2a-9e1f-0a5b7c9d2e4f"

// checkID returns nil when value is a UUID, else an error naming what was wrong. kind is "issue id", "comment id", ...
func checkID(kind, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("expected %s, got the flag %q (the %s comes first, flags after it; see --help)", withArticle(kind), value, kind)
	}
	if !uuidRE.MatchString(value) {
		return fmt.Errorf("invalid %s %q: want a UUID like %s", kind, value, exampleUUID)
	}
	return nil
}

func withArticle(kind string) string {
	if kind != "" && strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}

// checkIDs validates (kind, value) pairs in order; on the first bad one it prints the error and returns ExitUsage
// and false.
func (e *env) checkIDs(pairs ...string) (int, bool) {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := checkID(pairs[i], pairs[i+1]); err != nil {
			fmt.Fprintln(e.stderr, "error:", err)
			return ExitUsage, false
		}
	}
	return ExitOK, true
}

// checkNotFlag refuses a flag-looking value where a free-form id is expected (members remove).
func (e *env) checkNotFlag(kind, value string) (int, bool) {
	if strings.HasPrefix(value, "-") {
		fmt.Fprintf(e.stderr, "error: expected %s, got the flag %q (see --help)\n", withArticle(kind), value)
		return ExitUsage, false
	}
	return ExitOK, true
}

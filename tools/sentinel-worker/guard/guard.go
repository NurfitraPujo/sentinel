// Package guard implements sentinel-worker's untrusted-input handling and published-output gate
// (plan §4.6): delimiting untrusted content in prompts, and checking every model-authored string
// before it leaves the worker (length caps, secret-value rejection, verbatim-tool-result caps).
// N8a defines the seam only — no repo tools or LLM output exist yet to gate (that's N8b/N8c/N8d);
// this package is exercised for real starting N8c.
package guard

import "strings"

// Delimit wraps untrusted content (issue titles, messages, stacktraces, comment/report bodies —
// all attacker-controlled per §4.6) in a fenced, labelled block for inclusion in a prompt, paired
// with a standing system rule (owned by the prompt templates, not this function) that fenced
// content is data, never instructions.
func Delimit(label, content string) string {
	return "```untrusted:" + label + "\n" + content + "\n```"
}

// Verdict is the result of checking one candidate published field.
type Verdict struct {
	Allowed bool
	Reason  string // set when !Allowed
}

// Config carries the gate's tunables (plan §5): WORKER_GATE_MAX_VERBATIM (default 0.25) and any
// configured secret values to reject against (redactor's value list, shared with the git-token
// leak protections in gitprovider, N8c).
type Config struct {
	MaxLen       int
	MaxVerbatim  float64 // fraction, e.g. 0.25
	SecretValues []string
}

// Check applies the plan §4.6 published-field gate: (a) length cap, (b) reject if it contains a
// configured secret value, (c) reject if more than MaxVerbatim of it is a verbatim substring of
// any of toolResults (this job's read_file/search_code outputs) — checked here as "does any single
// tool result contain this much of the candidate verbatim", a conservative proxy for the plan's
// full corpus check that N8c's repoctx integration will refine with real tool-result tracking.
func Check(candidate string, cfg Config, toolResults []string) Verdict {
	if cfg.MaxLen > 0 && len(candidate) > cfg.MaxLen {
		return Verdict{Allowed: false, Reason: "exceeds max length"}
	}
	for _, secret := range cfg.SecretValues {
		if secret != "" && strings.Contains(candidate, secret) {
			return Verdict{Allowed: false, Reason: "contains a configured secret value"}
		}
	}
	if cfg.MaxVerbatim > 0 && len(candidate) > 0 {
		for _, tr := range toolResults {
			overlap := longestCommonSubstringLen(candidate, tr)
			if float64(overlap)/float64(len(candidate)) > cfg.MaxVerbatim {
				return Verdict{Allowed: false, Reason: "exceeds verbatim tool-result threshold"}
			}
		}
	}
	return Verdict{Allowed: true}
}

// longestCommonSubstringLen returns the length of the longest common substring of a and b. Kept
// deliberately simple (O(len(a)*len(b)) DP) — job-scoped inputs are small (tool-result byte caps
// are enforced upstream per plan §4.1), so this never runs against unbounded text.
func longestCommonSubstringLen(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	best := 0
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
				if curr[j] > best {
					best = curr[j]
				}
			} else {
				curr[j] = 0
			}
		}
		prev, curr = curr, prev
	}
	return best
}

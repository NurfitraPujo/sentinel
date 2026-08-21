// jobs/fix_pr.go implements the FIX engine's PR flow (plan §4.4 step 5, N8f "fix-pr-resume-caps"):
// push the fix branch (askpass-authed, never the default branch), open a harness-templated PR,
// then post the plan §2.3-shaped batch (issues.progress + a comment carrying the PR URL, NO status
// op — C7 — and the claim is KEPT). It also defines the journal payload sweep.go's hasOpenFix
// generic non-terminal-record check and PollFixPRStatus's live PR-status poll (driven from
// main.go's Sweep wiring) to find the open PR.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// fixBranchPrefix is the ONLY namespace a FIX push target may live in (plan §4.4 step 5,
// CLAUDE.md hard rule: "assert the target is sentinel-fix/*"). AssertFixBranchSafe is the
// load-bearing check a mutation test proves: delete the call site in PushFixBranch, or weaken
// this constant to match an arbitrary branch, and TestPushFixBranch_RefusesDefaultBranch /
// TestPushFixBranch_RefusesNonFixPrefixedBranch must go red.
const fixBranchPrefix = "sentinel-fix/"

// AssertFixBranchSafe rejects any push target that is not a well-formed sentinel-fix/* branch,
// and separately rejects a branch equal to defaultBranch even in the (currently unreachable,
// defence-in-depth) case that it was somehow named into the sentinel-fix/ namespace anyway —
// CLAUDE.md: "NEVER push a default branch" is an independent invariant from the prefix check, not
// implied by it once FixBranchName's own derivation is trusted less than "always".
func AssertFixBranchSafe(branch, defaultBranch string) error {
	if branch == "" {
		return fmt.Errorf("jobs: fix pr: push target branch is empty")
	}
	if defaultBranch != "" && branch == defaultBranch {
		return fmt.Errorf("jobs: fix pr: refusing to push the default branch %q", branch)
	}
	if !strings.HasPrefix(branch, fixBranchPrefix) {
		return fmt.Errorf("jobs: fix pr: refusing to push non-FIX branch %q (must start with %q)", branch, fixBranchPrefix)
	}
	return nil
}

// PushFixBranchInput is PushFixBranch's argument bundle.
type PushFixBranchInput struct {
	RepoDir       string
	Branch        string
	DefaultBranch string
	Cred          gitprovider.GitCredential
	Redactor      *gitprovider.Redactor

	// CloneURL is the repo connection's own known-good clone URL (settings.ProjectSettings /
	// FixRepoConfig.CloneURL — never read back out of the repo's own, attacker-writable
	// .git/config) that this push is meant to reach. It is REQUIRED: PushFixBranch derives the
	// askpass host-pin from it, not from argv, because `git push -u origin <branch>` carries no
	// URL in its own args for deriveExpectedHost to find (finding 1). Without a pin here, a
	// repo-local `url.<attacker>.insteadOf` rewrite of origin (or a plain `git remote set-url`)
	// written by the untrusted FIX executor before this push runs would silently redirect the
	// authenticated request -- and the git credential with it -- to an attacker-controlled host.
	CloneURL string
}

// PushFixBranch pushes in.Branch to origin (askpass-authed, plan §4.5) after AssertFixBranchSafe.
// This is the ONLY place in this package that ever runs `git push` — the Fix Executor itself
// never pushes (plan §4.4 trust boundary: "Push happens from the WORKER after validation ...
// not by the executor").
//
// The askpass host pin (finding 1) is derived from in.CloneURL -- the repo connection's own
// known-good clone URL -- NOT from argv (there is none to derive from in a `push -u origin
// <branch>` invocation) and NOT from re-reading origin's URL out of .git/config (which is exactly
// what an attacker with executor-level write access to the workspace could have rewritten,
// e.g. via a repo-local `url.<attacker>.insteadOf` or `git remote set-url`, BEFORE this push
// runs). Pinning to the caller-supplied CloneURL means that rewrite still authenticates against
// the real host, or not at all -- never against the attacker's.
func PushFixBranch(ctx context.Context, in PushFixBranchInput) error {
	if err := AssertFixBranchSafe(in.Branch, in.DefaultBranch); err != nil {
		return err
	}
	if in.CloneURL == "" {
		return fmt.Errorf("jobs: fix pr: push %s: CloneURL is required to pin the askpass host", in.Branch)
	}
	expectedHost, err := expectedPushHost(in.CloneURL)
	if err != nil {
		return fmt.Errorf("jobs: fix pr: push %s: %w", in.Branch, err)
	}
	if err := gitprovider.RunGitWithHost(ctx, in.RepoDir, in.Cred, in.Redactor, expectedHost, "push", "-u", "origin", in.Branch); err != nil {
		return fmt.Errorf("jobs: fix pr: push %s: %w", in.Branch, err)
	}
	return nil
}

// expectedPushHost parses cloneURL (production always supplies plan §4.5's "remotes always
// tokenless" http(s) clone URL, e.g. "https://github.com/owner/repo.git") into the host:port
// SENTINEL_ASKPASS_HOST pin PushFixBranch passes to RunGitWithHost. A malformed http(s) URL (a
// parse error, or an http(s) scheme with no host) is refused outright — an empty pin there would
// defeat the point of pinning (see gitauth.go's deriveExpectedHost doc: an EMPTY pin disables the
// askpass check entirely). A cloneURL with a non-http(s) scheme, or no scheme at all (a plain
// local filesystem path, the shape test fixtures use for a local bare-repo remote), returns ""
// deliberately: that transport never carries HTTPS Basic auth over the wire in the first place, so
// there is no credential-bearing request for a host pin to protect.
func expectedPushHost(cloneURL string) (string, error) {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return "", fmt.Errorf("parsing clone URL for askpass host pin: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil
	}
	if u.Host == "" {
		return "", fmt.Errorf("clone URL %q has an http(s) scheme but no host; refusing to push with a disabled askpass pin", cloneURL)
	}
	return u.Host, nil
}

// prBodyTemplate is the plan §4.4 step-5 harness-templated PR body: fixed prose plus the Sentinel
// issue URL, with the Fix Brief in its OWN fenced block. FixBrief is the only field in this
// template ever sourced from model output; fencing it means a prompt-injection payload inside a
// fixBrief renders as inert markdown code to a human reviewer, never as body prose the harness
// itself appears to have written (CLAUDE.md: "never raw model prose or issue text outside the
// fence").
const prBodyTemplate = `This pull request was opened automatically by Sentinel's Agent Worker in response to a reported issue. Please review carefully before merging — this change was authored by an automated coding agent.

Sentinel issue: %s

## Diagnosis (Fix Brief)

%s
`

// longestBacktickRun returns the length of the longest run of consecutive '`' characters in s. Used
// to size a CommonMark-safe fence: a fence of N backticks is only closed by a line with >= N
// backticks, so a fence strictly longer than any run inside the content can never be prematurely
// closed by content that happens to contain its own ``` sequences (CLAUDE.md / plan §4.4: the Fix
// Brief must render as "a fenced block — never raw model prose or issue text outside the fence").
func longestBacktickRun(s string) int {
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	return longest
}

// fenceFixBrief renders content inside a backtick fence guaranteed to be strictly longer than any
// backtick run content itself contains, so content can never escape the fence early (a bare ``` in
// a fixBrief that quotes suspected code is common and must not un-fence trailing text into raw PR
// body prose).
func fenceFixBrief(content string) string {
	n := longestBacktickRun(content) + 1
	if n < 3 {
		n = 3
	}
	fence := strings.Repeat("`", n)
	return fence + "\n" + content + "\n" + fence
}

// titleCharsetPattern is the conservative allowlist errorClassForTitle restricts a PR title's
// error-class segment to (circuit-config-sec finding 7): letters, digits, and a small set of
// punctuation common in real error-class names (dots/colons/slashes/underscores/hyphens, plus
// space and basic sentence punctuation). Everything else -- control characters, and markdown-
// significant runes that could break out of the title's plain-text context or render as
// unintended formatting/links in a GitHub PR title/notification (“ ` “, `*`, `_` doubled up as
// emphasis, `[`, `]`, `(`, `)` forming a markdown link, `<`, `>` opening HTML/autolinks, `#`
// looking like a heading or issue reference, `\`, `|`, `"`, `@` risking an accidental mention) --
// is dropped outright rather than passed through.
var titleCharsetPattern = regexp.MustCompile(`[^A-Za-z0-9 ,.:/_+=~-]`)

// errorClassForTitle sanitizes an error-class string for inclusion in a PR title: collapses all
// whitespace runs (including embedded newlines a stacktrace-derived errorClass might carry) to a
// single space, restricts the result to titleCharsetPattern's conservative charset, and caps
// length, so a pathological or attacker-influenced errorClass cannot blow up, break the
// single-line PR title, or inject markdown/HTML the title's rendering context would interpret.
//
// circuit-config-sec finding 7: errorClass is attacker-controlled (it flows from event data an
// external error report can shape) and, before this fix, only had whitespace collapsed -- it was
// interpolated into the PR title without ever going through guard.Check or WrapUntrusted, unlike
// every other model-authored/untrusted field this package publishes. A charset restriction (rather
// than routing it through guard.Check, which is sized for multi-KB prose fields, not an ~80-byte
// title fragment) is the proportionate fix here: it cannot inject formatting or control sequences
// into the title no matter what bytes errorClass carries.
func errorClassForTitle(errorClass string) string {
	s := strings.Join(strings.Fields(errorClass), " ")
	s = titleCharsetPattern.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ") // collapse any double spaces left by a dropped char
	const maxLen = 80
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	if s == "" {
		s = "unknown-error"
	}
	return s
}

// BuildFixPRSpec renders the plan §4.4 step-5 harness-templated PRSpec: title
// "fix: <error class> (sentinel <short id>)", body = the fixed template above with the issue URL
// and the GATED Fix Brief interpolated. fixBrief is run through guard.CheckWithConfig(guard.FieldFixBrief,
// ...) BEFORE it is interpolated into anything — the only guard call between an Advisor's raw
// fixBrief text and what ends up in a PR body (CLAUDE.md: "gated through guard.Check").
// toolOutputs/secrets are threaded straight through to the gate as-is; see guard.CheckWithConfig's
// own doc for their meaning. maxVerbatim is WORKER_GATE_MAX_VERBATIM (plan §5 finding 3); <=0 uses
// guard.DefaultMaxVerbatim, same convention as jobs.ActContext.maxVerbatim.
func BuildFixPRSpec(issueID, issueURL, errorClass, fixBrief, headBranch, baseBranch string, toolOutputs, secrets []string, maxVerbatim float64) (gitprovider.PRSpec, error) {
	if maxVerbatim <= 0 {
		maxVerbatim = guard.DefaultMaxVerbatim
	}
	cfg := guard.Config{SecretValues: secrets, MaxVerbatim: maxVerbatim}
	if err := guard.CheckWithConfig(guard.FieldFixBrief, fixBrief, toolOutputs, cfg); err != nil {
		return gitprovider.PRSpec{}, err
	}
	title := fmt.Sprintf("fix: %s (sentinel %s)", errorClassForTitle(errorClass), first8Hex(issueID))
	issueLine := issueID
	if issueURL != "" {
		issueLine = issueURL
	}
	return gitprovider.PRSpec{
		Title: title,
		Body:  fmt.Sprintf(prBodyTemplate, issueLine, fenceFixBrief(fixBrief)),
		Head:  headBranch,
		Base:  baseBranch,
	}, nil
}

// CreateFixPR pushes the fix branch (via PushFixBranch, so AssertFixBranchSafe always runs first)
// then opens the PR via provider.CreatePR. This is the only call site in this package invoking
// Provider.CreatePR — nothing else in the FIX engine opens a pull request.
func CreateFixPR(ctx context.Context, provider gitprovider.Provider, repo gitprovider.RepoRef, push PushFixBranchInput, spec gitprovider.PRSpec) (gitprovider.PR, error) {
	if err := PushFixBranch(ctx, push); err != nil {
		return gitprovider.PR{}, err
	}
	pr, err := provider.CreatePR(ctx, repo, spec)
	if err != nil {
		return gitprovider.PR{}, fmt.Errorf("jobs: fix pr: create PR: %w", err)
	}
	return pr, nil
}

// PostFixPRBatch compiles and sends the plan §4.4 step-5 batch: issues.progress + a comment
// carrying the PR URL. Deliberately NO status op — C7: in_progress does not exist as an issue
// status; claim + progress ARE the in-flight signal — and NO issues.claim.release: the claim
// stays held while the PR is out for review (the sweep's PR-status poll, wired via
// ResolveOpenFixPR below, is what eventually resolves/releases it).
//
// The issues.progress op is keyed "message_md" (finding 1): the server (agent-ops.ts:408) throws
// a 400 requiring exactly that param name -- NOT "body_md", which is what issues.comment/
// issues.claim.release use. Before this fix every PostFixPRBatch call 400'd on its progress op on
// every single PR (mirror client.PostProgress's own "message_md" convention, sentinel/client.go).
//
// checkBatchResults (the SAME per-op classifier RealActor.Act/sweep.go's releaseWithHandback use)
// walks results[] after the call: an envelope-level failure or ANY op that did not classify as
// success/droppable-conflict is now surfaced as an error rather than silently discarded -- before
// this fix, callers (fix.go's executeValidatePublish) never inspected the batch response body at
// all, so a 400'd progress op was swallowed and the PR-opened notification silently never landed.
func PostFixPRBatch(ctx context.Context, client Sender, jobID, issueID string, pr gitprovider.PR) (*sentinel.Result, error) {
	b := newOpBuilder(jobID)
	body := fmt.Sprintf("Opened a fix pull request: %s", pr.URL)
	b.add("issues.progress", issueID, map[string]interface{}{"message_md": body})
	b.add("issues.comment", issueID, map[string]interface{}{"body_md": body})
	res, err := client.PostBatch(ctx, sentinel.BatchRequest{Operations: b.ops, StopOnError: false})
	if err != nil {
		return nil, fmt.Errorf("jobs: fix pr: posting PR-opened batch for job %s: %w", jobID, err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return res, fmt.Errorf("jobs: fix pr: posting PR-opened batch for job %s: status %d: %s", jobID, res.Status, sentinel.ErrorMessage(res.Body))
	}
	if err := checkBatchResults(Compiled{Ops: b.ops}, res); err != nil {
		return res, fmt.Errorf("jobs: fix pr: posting PR-opened batch for job %s: %w", jobID, err)
	}
	return res, nil
}

// PostFixPRClosedBatch compiles and sends the plan §4.3 "declined/closed => comment + release"
// batch: a hand-back comment noting the fix PR closed without merging, then issues.claim.release
// (fixed op order — comment before release, matching releaseWithHandback's own convention). Used
// by main.go's fix-PR-status hook once PollFixPRStatus reports FixPRStatusClosed.
func PostFixPRClosedBatch(ctx context.Context, client Sender, jobID, issueID, prURL string) (*sentinel.Result, error) {
	b := newOpBuilder(jobID)
	body := fmt.Sprintf("🤖 The fix pull request (%s) was closed without merging. Releasing this issue: a human should take another look.", prURL)
	b.add("issues.comment", issueID, map[string]interface{}{"body_md": body})
	if err := b.addRelease(issueID); err != nil {
		return nil, err
	}
	res, err := client.PostBatch(ctx, sentinel.BatchRequest{Operations: b.ops, StopOnError: false})
	if err != nil {
		return nil, fmt.Errorf("jobs: fix pr: posting PR-closed batch for job %s: %w", jobID, err)
	}
	return res, nil
}

// FixKind is the journal Kind every FIX-originated record uses (an exported alias of sweep.go's
// openFixKind) — main.go's boot-time recovery pass needs it, from outside this package, to tell an
// in-flight FIX record apart from a TRIAGE/FOLLOW-UP one after state.Journal.RecoveryScan (finding
// 4), the same way hasOpenFix already does from inside this package.
const FixKind = openFixKind

// FixRunningPayload is state.Record.Payload's shape at the plan §4.4 step-3b in-flight FIX marker
// (finding 4, N8f functional-minors): Input is the COMPLETE FixJobInput a fresh RunFix was called
// with, so a boot-time resume never needs to re-derive ErrorClass/FixBrief/Occurrences/IssueURL
// from a live GetIssue call or re-consult any Advisor — mirroring plan §2.2's "the LLM is never
// re-invoked for a job that already produced a decision" replay-verbatim discipline for TRIAGE/
// FOLLOW-UP jobs. BaseCommit is carried alongside for observability/debugging even though
// ResumeFix itself re-derives the base commit it actually needs from the saved resume-state
// artifacts (fix_resume.go), not from this payload.
type FixRunningPayload struct {
	Input      FixJobInput `json:"input"`
	BaseCommit string      `json:"baseCommit"`
	// Resumed distinguishes a crash-RESUME's own in-flight marker from a FRESH attempt's (finding
	// 5): FixCaps.SeedToday must count only fresh (Resumed==false) records toward
	// WORKER_MAX_FIX_JOBS_PER_DAY/WORKER_MAX_FIX_ATTEMPTS -- a resumed attempt continuing the SAME
	// job after a worker restart is not "one more job" (CLAUDE.md: "a crash-resume of the same job
	// does NOT count again"). Before this field existed, journalFixRunning was called again for
	// every resume (via executeValidatePublish's shared tail), and SeedToday counted every such
	// record unconditionally -- a job that crashed and resumed twice in one day silently consumed
	// three attempt-budget slots instead of one.
	Resumed bool `json:"resumed"`
}

// journalFixRunning appends the plan §4.4 step-3b in-flight FIX marker: Kind=FixKind,
// State=state.StateFixRunning (non-terminal), Payload=FixRunningPayload. Called by
// FixRunner.executeValidatePublish at the start of every attempt (fresh via RunFix or resumed via
// ResumeFix) once a workspace/baseCommit exists — this is what makes a FIX job crash mid-attempt
// visible to, and resumable by, main.go's boot-time recovery scan (finding 4: before this, nothing
// distinguished an in-flight FIX job from any other non-terminal journal state, so
// state.Journal.RecoveryScan's callers either silently ignored it or would have mis-driven it
// through loop.Runner.Resume, which only understands TRIAGE/FOLLOW-UP kinds).
func journalFixRunning(j *state.Journal, in FixJobInput, baseCommit string, resumed bool) error {
	payload, err := json.Marshal(FixRunningPayload{Input: in, BaseCommit: baseCommit, Resumed: resumed})
	if err != nil {
		return fmt.Errorf("jobs: fix pr: marshaling FixRunningPayload for job %s: %w", in.JobID, err)
	}
	return j.Append(state.Record{
		JobID:      in.JobID,
		IssueID:    in.IssueID,
		Kind:       FixKind,
		TriggerSeq: in.TriggerSeq,
		State:      state.StateFixRunning,
		Payload:    payload,
	})
}

// journalFixTerminal appends a terminal record (state.StateFailed or state.StateSkipped -- never
// state.StateDone, which a FIX job only reaches via the pr-open/merge-handoff path journaled by
// JournalFixPROpen/journalFixPRClosed) for in.JobID -- the fix-lifecycle remediation round 2
// finding-1 BLOCKER fix. Every RunFix/ResumeFix exit path that does NOT open a PR (executor error,
// validation fail, empty diff, commit fail, PR-spec-build fail, cap hits, workspace prep fail,
// no-repo-connection propose-only) used to return via releaseWithComment WITHOUT ever appending a
// terminal record for this jobID: journalFixRunning (above) had already written a non-terminal
// state.StateFixRunning record for it (or, for the pre-workspace gates below, no record at all),
// and nothing closed it out. state.Journal.RecoveryScan surfaces the LATEST record per jobID that
// is non-terminal -- so a FIX job that failed validation, or exhausted its attempt cap, kept
// looking exactly like a crashed in-flight attempt on every subsequent boot, and
// resumeInFlightJob (main.go) drove it straight back into FixRunner.ResumeFix forever, re-running
// (and re-failing) the identical dead job on every restart. Called from releaseWithComment, the
// shared tail of every one of those exit paths, so this is wired from every real caller, not just
// a helper nothing invokes.
func journalFixTerminal(j *state.Journal, in FixJobInput, st state.JobState) error {
	payload, err := json.Marshal(FixRunningPayload{Input: in})
	if err != nil {
		return fmt.Errorf("jobs: fix pr: marshaling terminal FIX payload for job %s: %w", in.JobID, err)
	}
	return j.Append(state.Record{
		JobID:      in.JobID,
		IssueID:    in.IssueID,
		Kind:       FixKind,
		TriggerSeq: in.TriggerSeq,
		State:      st,
		Payload:    payload,
	})
}

// DecodeFixRunningPayload decodes raw (a journaled FixRunningPayload, e.g.
// state.InFlightJob.Payload from a record with State==state.StateFixRunning) back into a
// FixRunningPayload — the read side of journalFixRunning, for main.go's boot-time recovery pass to
// reconstruct the FixJobInput a crashed attempt needs to call FixRunner.ResumeFix with.
func DecodeFixRunningPayload(raw json.RawMessage) (FixRunningPayload, error) {
	var p FixRunningPayload
	if len(raw) == 0 {
		return p, fmt.Errorf("jobs: fix pr: DecodeFixRunningPayload: empty payload")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("jobs: fix pr: DecodeFixRunningPayload: %w", err)
	}
	return p, nil
}

// FixPRPayload is state.Record.Payload's shape once a FIX job's PR has been opened — the record
// sweep.go's generic hasOpenFix check (any non-terminal Kind==openFixKind record) already finds
// once this is journaled with State: state.StateActed, and what a PR-status poller (the sweep's
// FixPRStatusHook, plan §4.3) decodes via ResolveOpenFixPR to know which PR to poll.
type FixPRPayload struct {
	Provider gitprovider.RepoRef `json:"repo"` // Provider/Owner/Repo carried through RepoRef.Provider too
	PRID     string              `json:"prId"`
	PRURL    string              `json:"prUrl"`
}

// JournalFixPROpen appends the plan §4.4 step-5 in-flight marker: Kind=openFixKind ("fix"),
// State=StateActed (non-terminal — sweep.go's hasOpenFix treats any non-terminal fix-kind record
// as an open FIX), Payload=FixPRPayload. Calling this is what makes hasOpenFix/ReconcileReaped and
// PollFixPRStatus (the live PR-status poll, driven from main.go's Sweep wiring) find this
// issue's open PR — nothing else in the codebase journals a
// Kind==openFixKind record.
func JournalFixPROpen(j *state.Journal, jobID, issueID string, triggerSeq int64, payload FixPRPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jobs: fix pr: marshaling FixPRPayload for job %s: %w", jobID, err)
	}
	return j.Append(state.Record{
		JobID:      jobID,
		IssueID:    issueID,
		Kind:       openFixKind,
		TriggerSeq: triggerSeq,
		State:      state.StateActed,
		Payload:    data,
	})
}

// ResolveOpenFixPR decodes issueID's latest non-terminal openFixKind journal record (if any) back
// into a FixPRPayload — the read side of JournalFixPROpen, for a PR-status poller (sweep.go's
// FixPRStatusHook) to learn which provider/repo/PR id to call gitprovider.PRStatus against.
// found=false (with a nil error) means no open FIX-PR record exists for issueID, matching
// hasOpenFix's own "always false until journaled" convention rather than treating "none found" as
// an error.
func ResolveOpenFixPR(j *state.Journal, issueID string) (payload FixPRPayload, found bool, err error) {
	payload, _, found, err = resolveOpenFixJob(j, issueID)
	return payload, found, err
}

// resolveOpenFixJob is ResolveOpenFixPR plus the jobID the open record lives under, so a caller
// that needs to CLOSE that same record (journalFixPRClosed below) can append to the identical
// JobID rather than minting an unrelated one LatestByJobID would never associate with it.
func resolveOpenFixJob(j *state.Journal, issueID string) (payload FixPRPayload, jobID string, found bool, err error) {
	latest, err := j.LatestByJobID()
	if err != nil {
		return FixPRPayload{}, "", false, fmt.Errorf("jobs: fix pr: resolving open PR for issue %s: %w", issueID, err)
	}
	for jid, r := range latest {
		// r.State == state.StateActed specifically (not merely non-terminal) -- JournalFixPROpen is
		// the ONLY place that ever writes that exact State for a Kind==openFixKind record. Without
		// this, a Kind==openFixKind/State==StateFixRunning in-flight marker (journalFixRunning, plan
		// §4.4 step 3b's finding-4 resume trigger) would ALSO satisfy the loose "non-terminal +
		// unmarshals into FixPRPayload" check below: FixRunningPayload's JSON has none of
		// FixPRPayload's fields, so json.Unmarshal succeeds anyway with an all-zero FixPRPayload
		// (Go's decoder does not require every field present) -- a false "open PR" found before any
		// PR was ever opened.
		if r.IssueID != issueID || r.Kind != openFixKind || r.State != state.StateActed || len(r.Payload) == 0 {
			continue
		}
		var p FixPRPayload
		if jsonErr := json.Unmarshal(r.Payload, &p); jsonErr != nil {
			continue // not a FixPRPayload record (e.g. an earlier in-flight stage) -- keep scanning
		}
		return p, jid, true, nil
	}
	return FixPRPayload{}, "", false, nil
}

// journalFixPRClosed appends a terminal (StateDone) record onto the SAME jobID an open fix-PR was
// journaled under, so hasOpenFix's non-terminal scan stops finding it once the PR resolves
// (merged or declined/closed) — without this, a resolved PR would keep looking "open" to
// ReconcileReaped/the sweep forever.
func journalFixPRClosed(j *state.Journal, jobID, issueID string, triggerSeq int64, payload FixPRPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jobs: fix pr: marshaling FixPRPayload for job %s: %w", jobID, err)
	}
	return j.Append(state.Record{
		JobID:      jobID,
		IssueID:    issueID,
		Kind:       openFixKind,
		TriggerSeq: triggerSeq,
		State:      state.StateDone,
		Payload:    data,
	})
}

// FixPRStatusChecker is the narrow gitprovider surface PollFixPRStatus needs — satisfied by any
// gitprovider.Provider, but kept separate so main.go's resolver can return exactly this without
// pulling the rest of Provider (CreatePR, Auth, ...) into the hook's contract.
type FixPRStatusChecker interface {
	PRStatus(ctx context.Context, repo gitprovider.RepoRef, id string) (gitprovider.PRState, error)
}

// FixPRProviderResolver resolves the FixPRStatusChecker to poll repo's PR status with — main.go's
// implementation looks up the matching git credential in settings.Store, the same way
// settingsRepoResolver does for clones (plan §4.5). jobs cannot import settings/loop itself
// (loop already imports jobs), hence the resolver is injected rather than built here.
type FixPRProviderResolver func(repo gitprovider.RepoRef) (FixPRStatusChecker, error)

// FixPRStatusOutcome is what PollFixPRStatus found for one issue's open fix-PR poll.
type FixPRStatusOutcome int

const (
	// FixPRStatusNone: no open fix-PR record for this issue, the PR is still open, or the poll
	// itself failed (see the returned error) — callers should take no action.
	FixPRStatusNone FixPRStatusOutcome = iota
	// FixPRStatusMerged: the fix-PR merged. Plan §4.3: "merged => FOLLOW-UP proposes resolve."
	FixPRStatusMerged
	// FixPRStatusClosed: the fix-PR was declined/closed without merging. Plan §4.3:
	// "declined/closed => comment + release."
	FixPRStatusClosed
)

// PollFixPRStatus is the read+classify half of the plan §4.3 PR-status poll: it resolves issueID's
// open fix-PR (if any), asks resolveProvider's FixPRStatusChecker for its current state, and on a
// terminal outcome (merged/declined) journals the close so hasOpenFix stops reporting it open. It
// deliberately does NOT enqueue the FOLLOW-UP resolve job or post the release comment/release
// itself — those need loop.Enqueuer and the sentinel client, which this package cannot import
// (loop already imports jobs) — see main.go's fixPRStatusHook for those side effects, driven off
// this function's return value.
func PollFixPRStatus(ctx context.Context, j *state.Journal, triggerSeq int64, resolveProvider FixPRProviderResolver, issueID string) (FixPRStatusOutcome, FixPRPayload, error) {
	payload, jobID, found, err := resolveOpenFixJob(j, issueID)
	if err != nil || !found {
		return FixPRStatusNone, FixPRPayload{}, err
	}
	if resolveProvider == nil {
		return FixPRStatusNone, payload, fmt.Errorf("jobs: fix pr: poll status for issue %s: nil provider resolver", issueID)
	}
	checker, err := resolveProvider(payload.Provider)
	if err != nil {
		return FixPRStatusNone, payload, fmt.Errorf("jobs: fix pr: resolving provider for issue %s: %w", issueID, err)
	}
	prState, err := checker.PRStatus(ctx, payload.Provider, payload.PRID)
	if err != nil {
		return FixPRStatusNone, payload, fmt.Errorf("jobs: fix pr: polling PR status for issue %s: %w", issueID, err)
	}
	switch prState {
	case gitprovider.PRStateMerged:
		if err := journalFixPRClosed(j, jobID, issueID, triggerSeq, payload); err != nil {
			return FixPRStatusNone, payload, err
		}
		return FixPRStatusMerged, payload, nil
	case gitprovider.PRStateDeclined:
		if err := journalFixPRClosed(j, jobID, issueID, triggerSeq, payload); err != nil {
			return FixPRStatusNone, payload, err
		}
		return FixPRStatusClosed, payload, nil
	default: // PRStateOpen or an unrecognized/future state: still in flight, no action.
		return FixPRStatusNone, payload, nil
	}
}

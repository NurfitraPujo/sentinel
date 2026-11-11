---
description: "Analyze implementation for untested side effects and generate ripple-report.md"
---

# Ripple Scan

## Why This Exists

Tests verify that code does what it should. But they rarely catch what code does *besides* what it should. After implementation, changes ripple outward — affecting data flows, breaking implicit contracts, shifting timing assumptions, or introducing resource pressure that no test was designed to detect.

This command is **NOT a general code review**. It answers one specific question: **"What did this implementation break or put at risk that wasn't broken before?"** Every finding must be causally linked to a specific change made during implementation. Pre-existing issues that weren't affected by the changes are out of scope.

## User Input

```text
$ARGUMENTS
```

Parse the user's input for an optional severity threshold:

| Keyword | Behavior |
|---------|----------|
| _(default)_ | Report all findings (critical, warning, info) |
| `critical` | Only report critical-severity findings |
| `--diff` | Incremental scan — only files changed since the last scan |

Examples:
- `/speckit.ripple.scan` — full scan, all severities
- `/speckit.ripple.scan critical` — critical issues only
- `/speckit.ripple.scan --diff` — incremental scan on changed files

## Workflow

### Step 1: Load Context

Run the prerequisites check from the repository root:

```bash
.specify/scripts/bash/check-prerequisites.sh --json --paths-only
```

Parse `FEATURE_DIR` from the output. Then load the following artifacts from that directory:

- **Required**: `tasks.md`, `spec.md`, `plan.md`
- **Optional**: `blueprint.md`, `data-model.md`, `contracts/` directory, `checklist.md`
- **Optional**: `ripple-report.md` (previous scan — used for delta comparison)

If `tasks.md` is missing, abort with: "Run `/speckit.tasks` first."

### Step 2: Establish Baseline and Identify Changes

**This is the most critical step.** Ripple analysis is delta-anchored — it compares "before" vs. "after" to find side effects introduced by the implementation. Without a clear baseline, the analysis degenerates into a general code review.

#### 2a: Determine the Baseline

Identify the branch point (where the feature branch diverged from the base branch):

```bash
git merge-base HEAD main
```

This commit represents the state of the codebase **before** implementation. All analysis must be relative to this point.

#### 2b: Extract the Change Set

Get the precise list of files changed by the implementation:

```bash
git diff --name-status $(git merge-base HEAD main)..HEAD
```

This produces the **change set** — the files that were Added (A), Modified (M), or Deleted (D) by the implementation. These are the **cause** of potential ripple effects.

Also cross-reference with `tasks.md` to understand the **intent** behind each change.

**`--diff` (incremental) mode**: If the `--diff` flag is set and a previous `ripple-report.md` exists:

1. Read the `**Scanned**:` timestamp from the existing report header
2. Narrow the change set to files modified **after** that timestamp using:
   ```bash
   git log --since="{scanned_datetime}" --name-only --pretty=format: HEAD -- {files_in_change_set}
   ```
3. Only analyze files that appear in both the original change set (vs. merge-base) AND were modified after the last scan
4. Carry over all existing findings from the previous report — do not re-analyze them unless their files appear in the narrowed set
5. If no files were modified since the last scan, report: "No changes since last scan. Run without `--diff` for a full re-scan."

#### 2c: Identify the Blast Radius

For each file in the change set, identify files that **depend on it** but were **not part of the change set**:

- Files that import, reference, or call into the changed file
- Files that share state, configuration, or data structures with the changed file
- Files that consume the output (API responses, events, messages) of the changed code

These dependents are where ripple effects manifest. Read them to understand what assumptions they make about the changed code.

#### 2d: Read the Diffs

For each changed file, read the actual diff to understand **what specifically changed**:

```bash
git diff $(git merge-base HEAD main)..HEAD -- {file_path}
```

Understanding the specific lines changed is essential. A finding must trace back to a specific diff hunk, not just "this file was modified."

### Step 3: Analyze Across 9 Categories

For each change in the diff, trace its impact on the blast radius files. The analysis follows a strict causal chain:

```
Specific change (diff hunk) → Affected dependent (blast radius file) → Side effect (what breaks or becomes risky)
```

**Causation test**: Before reporting a finding, ask: *"If this implementation had not been made, would this problem exist?"* If the answer is YES, it is a pre-existing issue — do NOT report it. Only report issues that were **introduced or worsened** by the current changes.

Apply all 9 categories — skip a category only if it is genuinely irrelevant to the change.

---

#### Category 1: Data Flow

Trace how the change altered the way data enters, transforms, and exits the code.

**Look for changes that:**
- Altered the shape, type, or encoding of data that downstream consumers still expect in the old format
- Added/removed fields in a serialization path without updating the deserialization counterpart
- Introduced implicit type coercion or precision loss that didn't exist before
- Changed what happens to invalid/missing data (e.g., previously rejected, now silently default-filled)
- Broke an assumption a dependent module had about data ordering, nullability, or completeness
- Exposed sensitive data (credentials, PII, tokens) to a new output path — logs, responses, or external services — that didn't receive it before

---

#### Category 2: State & Lifecycle

Examine how the change introduced new state mutations or altered object lifetimes.

**Look for changes that:**
- Introduced new shared/global state mutation that outlives the operation's intended scope
- Added resource acquisition (handles, connections, locks) without corresponding release
- Created a new initialization order dependency that didn't exist before
- Removed or reordered lifecycle hooks/teardown logic that dependents relied on
- Invalidated caches or memoization by changing the data they were built from

---

#### Category 3: Interface Contract

Check whether the change altered — explicitly or implicitly — the contract that other modules depend on.

**Look for changes that:**
- Modified a method/function signature in a way that compiles but shifts semantics for callers
- Changed the meaning of a return value (same type, different interpretation now)
- Altered preconditions/postconditions that callers were relying on without updating them
- Broke an implicit contract (e.g., "this always returned sorted results" but now it doesn't)
- Changed event/callback emission — different order, different payloads, or stopped emitting entirely
- Weakened an access control contract — operations that previously required authorization now don't, or permission checks that were bypassed or downgraded

---

#### Category 4: Resource & Performance

Assess whether the change altered resource consumption or performance characteristics compared to before.

**Look for changes that:**
- Increased loop/recursion depth or changed scaling behavior with input size
- Added allocations inside hot paths that didn't exist before (per-request, per-item, per-tick)
- Moved I/O operations into a tighter loop or increased call frequency
- Changed algorithmic complexity (e.g., was O(n), now O(n^2) due to a newly nested lookup)
- Broke a batch/bulk operation into individual calls, or vice versa, affecting throughput

---

#### Category 5: Concurrency

Analyze whether the change introduced new thread-safety, async, or parallel execution risks within a single process.

**Look for changes that:**
- Exposed shared mutable state to concurrent access that was previously single-threaded or protected
- Broke an atomicity assumption (e.g., a read-then-write that was safe before but now races)
- Introduced new lock acquisition that conflicts with existing lock ordering
- Made async operations depend on completion order without enforcing it
- Moved a callback or handler to run on a different thread/context than before

---

#### Category 6: Distributed Coordination

Evaluate whether the change introduced new cross-process, cross-service, or cross-node risks.

**Look for changes that:**
- Added new network calls that assume success or instant response where there were none before
- Removed or omitted idempotency on operations that can now be retried (message redelivery, API retry)
- Introduced new ordering assumptions across service boundaries that aren't enforced
- Created a consistency gap — updated one side of a service boundary without the other
- Changed behavior when a downstream dependency is unreachable (previously handled, now not)
- Extended a transaction boundary to span multiple services without compensation/rollback logic

---

#### Category 7: Configuration & Environment

Check whether the change introduced new configuration or deployment requirements.

**Look for changes that:**
- Added new environment variables, config keys, or feature flags without documenting or defaulting them
- Introduced environment-specific behavior (dev vs. staging vs. production) that isn't accounted for
- Changed dependency versions that require coordinated deployment with other services
- Added new files or modules not included in build/package configuration
- Created new migration or deployment ordering requirements not captured anywhere
- Relaxed security-related configuration (access controls, trust boundaries, encryption settings, rate limits) that was stricter before

---

#### Category 8: Error Propagation

Trace how the change altered error flows compared to the previous behavior.

**Look for changes that:**
- Introduced new failure modes without corresponding error handling in callers
- Changed error types or codes that upstream catch/match logic depends on
- Added catch blocks that swallow errors silently where errors previously propagated
- Created new partial failure states where the operation can half-complete, leaving inconsistent state
- Added retry logic on operations that aren't idempotent
- Changed error messages or codes that other components parse programmatically
- Altered error responses to include implementation internals (debug details, query information, system paths) that weren't exposed before

---

#### Category 9: Observability

Assess whether the change degraded the ability to monitor, debug, or diagnose issues compared to before.

**Look for changes that:**
- Removed or downgraded log statements for operations that previously had them
- Added new code paths with no logging, metrics, or tracing where parallel paths had them
- Failed to propagate correlation/trace IDs through newly introduced call chains
- Changed metric labels or dimensions, breaking existing dashboards/alerts
- Didn't update health check or readiness probe logic to reflect newly added dependencies
- Lost debug information (stack traces, context) by wrapping errors differently than before

---

### Step 4: Assign Severity

For each finding, assign a severity:

| Severity | Criteria |
|----------|----------|
| **CRITICAL** | Could cause data loss, security breach, or system outage in production |
| **WARNING** | Likely to cause bugs, degraded performance, or operational issues |
| **INFO** | Potential concern worth reviewing — may be intentional or low-risk |

### Step 5: Generate ripple-report.md

Create `specs/{feature}/ripple-report.md` with the following structure:

````markdown
# Ripple Report: {Feature Name}

**Branch**: `{branch}` | **Scanned**: {datetime}
**Baseline**: `{merge-base commit short hash}` (branch point from {base branch})
**Change Set**: {N} files changed | **Blast Radius**: {M} dependents checked
**Findings**: {critical} critical, {warning} warning, {info} info

## Summary

{2-3 sentence overview of the most significant findings}

## Findings

### CRITICAL

#### R-{NNN}: {Brief title}

- **Category**: {category name}
- **Cause**: {What specific change (file + diff hunk) introduced this side effect}
- **Affected**: `{path/to/affected/file}` (line ~{N}) — the code that is now at risk
- **Blast Radius**: `{other_affected_1}`, `{other_affected_2}`
- **Before**: {How this behaved before the change}
- **After**: {How this behaves now — and why that's a problem}
- **Why Tests Miss It**: {Why existing or typical tests won't catch this}
- **Recommendation**: {Concrete action to mitigate}
- **Status**: OPEN

---

### WARNING

#### R-{NNN}: {Brief title}

{same structure as above}

---

### INFO

#### R-{NNN}: {Brief title}

{same structure as above}

---

## Coverage Gap Matrix

| Category | Critical | Warning | Info | Not Applicable |
|----------|----------|---------|------|----------------|
| Data Flow | {count} | {count} | {count} | |
| State & Lifecycle | {count} | {count} | {count} | |
| Interface Contract | {count} | {count} | {count} | |
| Resource & Performance | {count} | {count} | {count} | |
| Concurrency | {count} | {count} | {count} | |
| Distributed Coordination | | | | N/A — single-process |
| Configuration & Environment | {count} | {count} | {count} | |
| Error Propagation | {count} | {count} | {count} | |
| Observability | {count} | {count} | {count} | |

> Mark a category as "N/A" only with a brief justification (e.g., no distributed components involved).

## Next Steps

- [ ] Address CRITICAL findings before merging
- [ ] Review WARNING findings with the team
- [ ] Run `/speckit.ripple.check` after fixes to verify resolution
````

### Step 6: Report

Output a summary:
- Path to the generated `ripple-report.md`
- Finding counts by severity
- Top 3 highest-risk findings highlighted
- Suggested next step (e.g., "Address 2 CRITICAL findings, then run `/speckit.ripple.check`")

## Rules

- **Delta-anchored (MOST IMPORTANT)**: Every finding MUST be causally linked to a specific change in the implementation diff. Ask: *"If this implementation had not been made, would this problem exist?"* If YES → do NOT report it. Pre-existing issues, general code smells, and hypothetical future problems are out of scope. This is not a code review.
- **Before/After required**: Each finding must describe what the behavior was BEFORE the change and what it is AFTER. If you cannot articulate both states, the finding is not a side effect — it's a general observation.
- **Evidence-based**: Every finding must reference both the causing diff hunk AND the affected code (file, line, function). No vague warnings.
- **No false confidence**: If a category cannot be fully analyzed (e.g., no access to runtime behavior), state the limitation explicitly in the report.
- **Project-agnostic categories**: Apply categories based on what the code actually does, not what domain it belongs to. A CLI tool can have concurrency issues; an embedded system can have interface contract problems.
- **Respect existing tests**: If a test already covers a specific side effect, do not report it. The goal is to find what tests *miss*, not duplicate their coverage.
- **Severity honesty**: CRITICAL means "will likely break production." Do not inflate severity for attention.
- **Incremental-friendly**: When a previous `ripple-report.md` exists, note which findings are new vs. carried over. Never silently drop previous findings — mark them as RESOLVED if the code has changed to address them.
- **Read before judging**: Before flagging a side effect, read the actual implementation of the affected code AND its dependents. Do not assume behavior from names or signatures alone.
- **Blast radius completeness**: For each changed file, trace at least one level of dependents (files that import/reference it). For CRITICAL findings, trace two levels.
- **Scale-aware analysis**: For large change sets (16+ files), prioritize depth over breadth — focus blast radius tracing on the most structurally significant changes (shared modules, interfaces, configuration) rather than trying to trace every file equally. When `critical` filter is active, skip WARNING/INFO analysis entirely to conserve context.
- **Language of the report**: Follow the language used in existing spec/plan/tasks documents.
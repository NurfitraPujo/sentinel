# Ripple

> Detect side effects that tests can't catch after implementation.

A [Spec Kit](https://github.com/github/spec-kit) community extension that analyzes your implementation for hidden ripple effects — the kind of side effects that pass all tests but break things in production.

## The Problem

Tests verify **intended behavior**. But every code change creates ripple effects that tests aren't designed to catch:

- A function signature looks the same, but its return value *means* something different now
- A new dependency introduces a subtle ordering constraint
- A refactored module now holds onto resources longer than before
- An async operation that used to complete in time no longer does under load

These issues hide in the gap between "all tests pass" and "production is stable."

## What Ripple Does

Ripple is **not a general code review**. It compares your implementation against the baseline (branch point) and asks: *"What did this change break or put at risk that wasn't broken before?"*

Every finding is **delta-anchored** — causally linked to a specific change in your diff, with a clear before/after description. Pre-existing issues are out of scope.

It analyzes across **9 categories**:

| Category | What It Catches |
|----------|----------------|
| **Data Flow** | Input/output shape mismatches, silent data loss, serialization gaps |
| **State & Lifecycle** | Global state pollution, resource leaks, initialization order issues |
| **Interface Contract** | Semantic signature changes, broken implicit contracts |
| **Resource & Performance** | Complexity regressions, hot-path allocations, I/O amplification |
| **Concurrency** | Race conditions, lock ordering, atomicity assumptions |
| **Distributed Coordination** | Idempotency gaps, ordering assumptions, partition tolerance |
| **Configuration & Environment** | Missing config keys, environment-specific gaps, deploy ordering |
| **Error Propagation** | Unhandled failure modes, silent swallowing, partial failure states |
| **Observability** | Lost trace context, logging gaps, broken metrics |

Categories are **domain-agnostic** — they apply whether you're building a web API, CLI tool, mobile app, embedded system, data pipeline, or anything else. Security concerns (access control bypass, sensitive data exposure, privilege escalation) are covered as a cross-cutting lens within relevant categories rather than as a separate category. Domain-specific details are inferred from the actual codebase — Ripple adapts its analysis to the project's technology stack automatically.

## How Ripple Differs from Code Review

| | Code Review (`review`, `staff-review`) | Ripple |
|---|---|---|
| **Analyzes** | The changed code itself | The impact on code that *wasn't* changed |
| **Asks** | "Is this code good?" | "What did this change break elsewhere?" |
| **Baseline** | None — evaluates code as-is | git merge-base — compares before/after |
| **Pre-existing issues** | Reports them | Excludes them (causation test) |
| **Best used** | Together — review catches code quality, Ripple catches ripple effects |

## Installation

```bash
# From GitHub
git clone https://github.com/chordpli/spec-kit-ripple.git
specify extension add --dev spec-kit-ripple/

# Verify
specify extension list
```

## Commands

### `/speckit.ripple.scan`

Analyze implementation for untested side effects.

```bash
/speckit.ripple.scan              # Full scan, all severities
/speckit.ripple.scan critical     # Critical findings only
/speckit.ripple.scan --diff       # Incremental scan on changed files
```

**Produces**: `specs/{feature}/ripple-report.md`

### `/speckit.ripple.resolve`

Interactively walk through findings and decide how to fix each one.

```bash
/speckit.ripple.resolve             # Resolve all open findings, CRITICAL first
/speckit.ripple.resolve critical    # Resolve critical findings only
/speckit.ripple.resolve R-001 R-003 # Resolve specific findings
/speckit.ripple.resolve --dry-run   # Preview options without recording decisions
```

For each finding, Ripple presents:
1. The cause (what changed) and the side effect (what's at risk)
2. 2-4 concrete resolution options with tradeoffs (minimal fix, structural fix, skip)
3. A recommended option with reasoning

You pick an option, describe your own approach, or skip. Decisions are recorded in `ripple-report.md`, and fix plans are saved to `specs/{feature}/ripple-fixes.md` — ready for `/speckit.implement` to consume.

### `/speckit.ripple.check`

Re-verify findings after fixes have been applied.

```bash
/speckit.ripple.check             # Re-check all open findings
/speckit.ripple.check critical    # Re-check critical findings only
/speckit.ripple.check R-001 R-005 # Re-check specific findings
```

**Updates**: existing `ripple-report.md` with resolution status

Crucially, check also detects **fix-induced side effects** — new problems created by the fixes themselves. If a fix introduced a new risk, check will catch it and add it as a new finding.

## Hook

Ripple hooks into `after_implement` — after running `/speckit.implement`, you'll be prompted:

```
Scan for untested side effects? (y/n)
```

## Workflow

```
/speckit.specify  →  /speckit.plan  →  /speckit.tasks  →  /speckit.implement
                                                                    ↓
                                                     ┌──  /speckit.ripple.scan
                                                     │              ↓
                                                     │    /speckit.ripple.resolve
                                                     │              ↓
                                                     │    Implement fixes
                                                     │              ↓
                                                     │    /speckit.ripple.check
                                                     │              ↓
                                                     └── New findings? → loop back
                                                                    ↓ No
                                                          Merge with confidence
```

## Artifacts

| File | Created by | Purpose |
|------|-----------|---------|
| `specs/{feature}/ripple-report.md` | `scan`, updated by `resolve` and `check` | Side effect findings, resolution history, check history |
| `specs/{feature}/ripple-fixes.md` | `resolve` | Implementation guidance for each fix — bridge to `/speckit.implement` |

## Severity Levels

| Level | Meaning |
|-------|---------|
| **CRITICAL** | Could cause data loss, security breach, or system outage in production |
| **WARNING** | Likely to cause bugs, degraded performance, or operational issues |
| **INFO** | Potential concern worth reviewing — may be intentional or low-risk |

## Usage Tips

| PR Size | Recommendation |
|---------|---------------|
| Small (1-5 files) | `/speckit.ripple.scan` — full scan |
| Medium (6-15 files) | `/speckit.ripple.scan` — full scan, consider `critical` filter if findings are noisy |
| Large (16+ files) | `/speckit.ripple.scan critical` — start with critical only, then expand if needed |
| Re-scan after fixes | `/speckit.ripple.scan --diff` — incremental scan on changed files only |

Ripple reads the full diff plus blast radius files. Larger change sets consume more context. Use filters to keep scans focused.

## Requirements

- Spec Kit >= 0.2.0
- Existing spec artifacts (`spec.md`, `plan.md`, `tasks.md`)
- Implemented code on disk

## License

MIT
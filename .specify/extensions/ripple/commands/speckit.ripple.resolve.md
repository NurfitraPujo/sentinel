---
description: "Interactively resolve ripple findings one by one — review cause, choose a fix strategy, and apply"
---

# Ripple Resolve

## Why This Exists

`/speckit.ripple.scan` surfaces side effects. But a list of problems is not a list of solutions. Each finding may have multiple valid resolution strategies with different tradeoffs — quick patch vs. structural fix, defensive guard vs. root cause elimination, accept risk vs. redesign.

This command walks through each finding interactively, presents resolution options with tradeoffs, and records the decision. It turns "here's what's broken" into "here's what we decided to do about it."

## User Input

```text
$ARGUMENTS
```

Parse the user's input for optional filters:

| Keyword | Behavior |
|---------|----------|
| _(default)_ | Resolve all OPEN findings, CRITICAL first |
| `critical` | Resolve only CRITICAL findings |
| `R-{NNN}` | Resolve specific finding(s) by ID |
| `--dry-run` | Show resolution options without applying changes |

Examples:
- `/speckit.ripple.resolve` — walk through all open findings
- `/speckit.ripple.resolve critical` — critical findings only
- `/speckit.ripple.resolve R-001 R-003` — resolve specific findings
- `/speckit.ripple.resolve --dry-run` — preview options without recording decisions

## Workflow

### Step 1: Load Context

Run the prerequisites check from the repository root:

```bash
.specify/scripts/bash/check-prerequisites.sh --json --paths-only
```

Parse `FEATURE_DIR` from the output. Then load:

- **Required**: `ripple-report.md` (from a previous scan)
- **Required**: `tasks.md`, `spec.md`
- **Optional**: `plan.md`, `blueprint.md`

If `ripple-report.md` is missing, abort with: "Run `/speckit.ripple.scan` first."

### Step 2: Build Resolution Queue

1. Parse `ripple-report.md` and extract all findings with status `OPEN`
2. Apply user's filter if provided (severity or specific IDs)
3. Order the queue: CRITICAL → WARNING → INFO (within each severity, by finding ID)
4. Count total findings in scope

If no OPEN findings match the filter, report: "No open findings to resolve." and suggest running `/speckit.ripple.scan` if the report is outdated.

Report the queue:

```
Found {N} open findings to resolve: {count} CRITICAL, {count} WARNING, {count} INFO
Starting with R-{NNN}...
```

### Step 3: Sequential Resolution Loop

Present **EXACTLY ONE finding at a time**. Never reveal future findings in advance.

For each finding:

#### 3a: Present the Finding

Display a concise summary:

```markdown
## R-{NNN}: {Title} [{SEVERITY}]

**Category**: {category}

**What changed**: {Cause — the specific diff that introduced this}

**Before**: {behavior before the change}

**After**: {behavior after — and why it's a problem}

**Why tests miss it**: {explanation}
```

#### 3b: Analyze and Present Resolution Options

Read the affected code (the file at the referenced path) and its dependents. Based on the actual codebase state, generate 2–4 resolution options. Each option must be:

- **Concrete** — specific enough to implement, not vague advice
- **Distinct** — each option represents a genuinely different strategy, not variations of the same fix
- **Honest about tradeoffs** — effort, risk, scope of change, what it does NOT solve

Present your **recommended option** prominently with reasoning, then show all options:

```markdown
**Recommended: Option {X}** — {1-2 sentence reasoning why this is the best balance of effort, safety, and correctness}

| Option | Strategy | Tradeoff |
|--------|----------|----------|
| A | {description} | {effort / risk / limitation} |
| B | {description} | {effort / risk / limitation} |
| C | {description} | {effort / risk / limitation} |
| Skip | Accept risk — document as known limitation | No code change; risk remains |

Reply with the option letter (e.g., "A"), accept the recommendation ("yes"), or describe your own approach.
```

**Option design rules:**
- Always include at least one **minimal fix** (smallest change that addresses the immediate risk)
- Always include at least one **structural fix** (addresses the root cause, may touch more files)
- Always include **Skip** — the user may intentionally accept the risk
- If the finding is INFO severity, bias toward lightweight options
- Never include options that require changes outside the project's control (e.g., "wait for library update")

#### 3c: Process the User's Response

| User says | Action |
|-----------|--------|
| Option letter ("A", "B", etc.) | Record that option as the chosen resolution |
| "yes" / "recommended" | Use the recommended option |
| "skip" | Mark as ACCEPTED_RISK with user's acknowledgment |
| Free-form text | Interpret as a custom resolution strategy — confirm understanding before recording |
| "stop" / "done" | End the loop early; remaining findings stay OPEN |

If the response is ambiguous, ask for a quick clarification (does not count as a new finding).

#### 3d: Record the Resolution Decision

After each accepted answer, immediately update `ripple-report.md`:

1. Update the finding's status:

```markdown
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: {Option X}: {description} — chosen on {date}
```

2. If the user chose "Skip":

```markdown
- **Status**: ACCEPTED_RISK
- **Resolution Strategy**: Risk accepted — {user's reasoning if provided} ({date})
```

3. Save `ripple-report.md` after each decision (atomic write — don't batch).

#### 3e: Generate Implementation Guidance

After recording the decision, output a brief implementation note for the chosen strategy:

```markdown
### Implementation for R-{NNN}

**Files to modify**:
- `{path/to/file}` — {what to change}
- `{path/to/file}` — {what to change}

**Key steps**:
1. {step}
2. {step}
3. {step}

**Verification**: {how to confirm the fix works}
```

#### 3f: Persist Guidance for Implementation

After each resolution decision, append the implementation guidance to `specs/{feature}/ripple-fixes.md`. This file serves as a bridge to `/speckit.implement` — it aggregates all fix plans into a single artifact that the implement command can consume.

```markdown
## R-{NNN}: {Title} [{SEVERITY}]

**Strategy**: {Option X} — {description}

**Files to modify**:
- `{path/to/file}` — {what to change}

**Key steps**:
1. {step}
2. {step}

**Verification**: {how to confirm the fix works}

---
```

- Create `ripple-fixes.md` on the first resolution; append subsequent fixes
- If the file already exists from a prior session, append below the existing content with a session header: `# Ripple Fixes — Session {date}`
- After all resolutions, inform the user: "Fix plans saved to `specs/{feature}/ripple-fixes.md`. Run `/speckit.implement` to apply, or implement manually."

Then proceed to the next finding: "Next: R-{NNN} — {title} [{severity}]"

### Step 4: Update Report Summary

After all findings are processed (or the user stops early):

1. Update the ripple-report.md header counts to reflect new statuses
2. **Update Resolution History** — append a row to the existing table, never create a duplicate section:

   - **If `## Resolution History` section already exists**: Find the existing table and append a new row at the bottom. Do NOT create a second `## Resolution History` heading.
   - **If `## Resolution History` section does not exist**: Create it once at the bottom of the report (before `## Check History` if that exists) with the table header and the first row.
   - Add a `### Session detail ({date})` sub-section below the table with per-finding option choices. If prior session details exist, leave them intact and append the new one after them.

```markdown
## Resolution History

| Date | Scope | Resolved | Accepted Risk | Skipped | Still Open |
|------|-------|----------|---------------|---------|------------|
| {prior rows unchanged} | | | | | |
| {datetime} | {all/critical/R-NNN} | {count} | {count} | {count} | {count} |
```

**Deduplication rule**: Before writing, scan the report for existing `## Resolution History` headings. If more than one exists (from a prior bug), merge all rows into a single table under one heading and remove the duplicate.

3. Update the Next Steps section to reflect remaining work

### Step 5: Report

Output a completion summary:

- Findings addressed in this session
- Decisions made: {N} resolution planned, {N} risk accepted, {N} still open
- If all CRITICAL findings have a resolution: "All critical findings have resolution strategies. Implement fixes, then run `/speckit.ripple.check` to verify."
- If CRITICAL findings remain OPEN: "{N} critical findings still need resolution."
- Suggest next step: implement the fixes, then `/speckit.ripple.check`

## Rules

- **One at a time**: Present exactly one finding per turn. Never batch findings or reveal the queue.
- **Read before suggesting**: Always read the current state of the affected file before generating options. Options must be based on actual code, not the report's description alone.
- **Options must be implementable**: Every option (except Skip) must be specific enough that a developer (or `/speckit.implement`) can act on it without further design decisions.
- **Respect user decisions**: If the user chooses an option you didn't recommend, do not argue. Record it and move on.
- **No auto-fixing**: This command records decisions and provides guidance. It does NOT modify source code. Code changes happen in a separate implementation step.
- **Atomic saves**: Write `ripple-report.md` after each decision, not at the end. This prevents loss if the session is interrupted.
- **Severity-appropriate depth**: CRITICAL findings get detailed multi-option analysis. INFO findings can have simpler 2-option choices (fix / skip).
- **Never exceed the report**: Only resolve findings that exist in `ripple-report.md`. Do not surface new issues — that's what `/speckit.ripple.scan` is for.
- **Language of the session**: Follow the language used in the existing `ripple-report.md`.

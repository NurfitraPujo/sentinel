---
description: "Re-verify ripple report items and update resolution status"
---

# Ripple Check

## Why This Exists

After `/speckit.ripple.scan` generates a report, the developer addresses findings — fixing code, adding safeguards, or accepting risks. This command re-verifies each finding against the current codebase to confirm whether issues have actually been resolved, or if new side effects have been introduced by the fixes themselves.

## User Input

```text
$ARGUMENTS
```

Parse the user's input for optional filters:

| Keyword | Behavior |
|---------|----------|
| _(default)_ | Re-check all OPEN findings |
| `critical` | Re-check only CRITICAL findings |
| `R-{NNN}` | Re-check a specific finding by ID |

Examples:
- `/speckit.ripple.check` — re-check all open findings
- `/speckit.ripple.check critical` — re-check critical findings only
- `/speckit.ripple.check R-001 R-005` — re-check specific findings

## Workflow

### Step 1: Load Context

Run the prerequisites check from the repository root:

```bash
.specify/scripts/bash/check-prerequisites.sh --json --paths-only
```

Parse `FEATURE_DIR` from the output. Then load:

- **Required**: `ripple-report.md` (from a previous scan)
- **Required**: `tasks.md`, `spec.md`
- **Optional**: `blueprint.md`, `plan.md`

If `ripple-report.md` is missing, abort with: "Run `/speckit.ripple.scan` first."

### Step 2: Parse Existing Findings

Parse `ripple-report.md` and extract all findings with status `OPEN`. For each finding, capture:

- Finding ID (R-{NNN})
- Category
- Severity
- File path and line reference
- Description of the side effect
- Recommendation

Apply user's filter if provided (severity or specific IDs).

### Step 3: Re-verify Each Finding

For each OPEN finding in scope:

1. **Read the current file** at the referenced path and line
2. **Check if the code has changed** since the scan — compare against the description and evidence in the finding
3. **Evaluate resolution**:

| Verdict | Condition | New Status |
|---------|-----------|------------|
| **RESOLVED** | The side effect is no longer present — code was fixed or safeguard was added | RESOLVED |
| **MITIGATED** | Risk reduced but not eliminated — partial fix or documented acceptance | MITIGATED |
| **OPEN** | The side effect still exists unchanged | OPEN |
| **WORSENED** | The fix attempt introduced additional problems or the original issue expanded | WORSENED |
| **STALE** | The referenced file/line no longer exists (deleted or heavily refactored) | STALE |

4. **For WORSENED findings**: Describe what changed and what new risk was introduced
5. **For STALE findings**: Attempt to locate the code in its new location; if found, re-evaluate; if not found, mark as STALE with a note

### Step 4: Scan for Fix-Induced Side Effects

The fixes themselves can introduce new ripple effects. For each finding that changed status (RESOLVED or MITIGATED):

1. Identify what code was changed to address the finding
2. Apply the same 9-category analysis from `/speckit.ripple.scan` to the fix — but scoped narrowly to the changed lines
3. If new side effects are found, add them as new findings (R-{NNN+}) with a note: "Introduced while resolving R-{original}"

### Step 5: Update ripple-report.md

Update the existing `ripple-report.md` in place:

1. **Update finding statuses** — change `OPEN` to `RESOLVED`, `MITIGATED`, `WORSENED`, or `STALE` as determined
2. **Add resolution notes** — for each status change, append:

```markdown
- **Status**: RESOLVED
- **Resolution**: {What was done to fix it} ({date})
```

3. **Append new findings** — if fix-induced side effects were found, add them to the appropriate severity section with new IDs
4. **Update header counts** — recalculate findings summary
5. **Update the Coverage Gap Matrix** — reflect resolved items and any new findings
6. **Update Check History** — append a row to the existing table, never create a duplicate section:

   - **If `## Check History` section already exists**: Find the existing table and append a new row at the bottom. Do NOT create a second `## Check History` heading.
   - **If `## Check History` section does not exist**: Create it once at the bottom of the report with the table header and the first row.
   - If a `### Check detail` or `### Implementation detail` sub-section exists from a prior check, leave it intact. Add a new sub-section for this check session below it.

```markdown
## Check History

| Date | Scope | Resolved | Mitigated | Worsened | Stale | New | Still Open |
|------|-------|----------|-----------|----------|-------|-----|------------|
| {prior rows unchanged} | | | | | | | |
| {datetime} | {all/critical/R-NNN} | {count} | {count} | {count} | {count} | {count} | {count} |
```

**Deduplication rule**: Before writing, scan the report for existing `## Check History` headings. If more than one exists (from a prior bug), merge all rows into a single table under one heading and remove the duplicate.

### Step 6: Report

Output a summary:
- Total findings checked
- Status changes: {N} resolved, {N} mitigated, {N} worsened, {N} stale, {N} still open
- New findings introduced by fixes (if any)
- If all CRITICAL findings are resolved: "All critical findings resolved. Safe to proceed."
- If CRITICAL findings remain: "N critical findings still open. Address before merging."
- If WORSENED findings exist: highlight them prominently

## Rules

- **Never delete findings**: Findings change status but are never removed from the report. This preserves audit history.
- **Evidence for every verdict**: Each status change must reference what specifically changed in the code. No status changes based on assumptions.
- **Fix-induced analysis is mandatory**: Skipping Step 4 defeats the purpose. Fixes that introduce new problems are common — always check.
- **WORSENED is serious**: If a fix made things worse, flag it prominently. Do not bury it in the report.
- **Preserve finding IDs**: Never renumber existing findings. New findings get the next available ID.
- **Read before judging**: Always read the current state of the file before changing a finding's status. Do not rely on git diff alone — the full context matters.
- **Scope honesty**: If the check was filtered (e.g., `critical` only), state clearly in the report that unchecked findings retain their previous status.
- **Language of the report**: Follow the language used in the existing `ripple-report.md`.
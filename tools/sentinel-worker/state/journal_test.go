package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestJournal_LoadMissingIsEmptyNotError(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("missing journal must not be an error, got: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %d", len(records))
	}
}

func TestJournal_AppendAndLoadPreservesOrder(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	states := []JobState{StateQueued, StateClaimed, StateAdvised, StateActing, StateActed, StateDone}
	for _, s := range states {
		if err := j.Append(Record{JobID: "job-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 10, State: s}); err != nil {
			t.Fatalf("Append(%s): %v", s, err)
		}
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != len(states) {
		t.Fatalf("expected %d records, got %d", len(states), len(records))
	}
	for i, s := range states {
		if records[i].State != s {
			t.Errorf("record %d: state = %s, want %s", i, records[i].State, s)
		}
	}
}

// TestJournal_DedupeViaLatestByJobID proves the plan §2.2 dedupe rule: a jobId whose LATEST record
// is terminal is what callers check against to drop a re-delivered event.
func TestJournal_DedupeViaLatestByJobID(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	must(t, j.Append(Record{JobID: "job-1", IssueID: "i1", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "job-1", IssueID: "i1", Kind: "triage", State: StateDone}))

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-1"]
	if !ok {
		t.Fatalf("expected job-1 in latest map")
	}
	if !rec.State.IsTerminal() {
		t.Fatalf("expected job-1's latest state to be terminal, got %s", rec.State)
	}
}

func TestJobID_StableAcrossCalls(t *testing.T) {
	a := JobID("triage", "issue-1", 5)
	b := JobID("triage", "issue-1", 5)
	if a != b {
		t.Fatalf("JobID must be deterministic: %s != %s", a, b)
	}
	c := JobID("triage", "issue-1", 6)
	if a == c {
		t.Fatalf("different triggerSeq must produce a different jobId")
	}
}

// TestJournal_CompactDropsOldTerminalKeepsRecent proves the plan §2.2 compaction rule: terminal
// records older than the cutoff are dropped, everything else survives.
func TestJournal_CompactDropsOldTerminalKeepsRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)

	old := time.Now().Add(-10 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	must(t, j.Append(Record{JobID: "old-done", IssueID: "i1", Kind: "triage", State: StateDone, At: old}))
	must(t, j.Append(Record{JobID: "recent-done", IssueID: "i2", Kind: "triage", State: StateDone, At: recent}))
	must(t, j.Append(Record{JobID: "still-open", IssueID: "i3", Kind: "triage", State: StateAdvised, At: old}))

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	if _, err := j.Compact(cutoff); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID after compact: %v", err)
	}
	if _, ok := latest["old-done"]; ok {
		t.Errorf("expected old-done to be compacted away")
	}
	if _, ok := latest["recent-done"]; !ok {
		t.Errorf("expected recent-done to survive compaction")
	}
	if _, ok := latest["still-open"]; !ok {
		t.Errorf("expected still-open (non-terminal) to survive compaction regardless of age")
	}
}

// TestJournal_CompactPreservesInFlightJobFullSequence proves the plan §2.2 crash-replay guarantee
// survives compaction: a job sitting mid-flight (advised -> acting, never reaching a terminal
// state) must keep its ENTIRE record sequence — including the advised record's decision payload —
// after compaction runs, not just its latest ("acting") record. Recovery replays the journaled
// batch from that payload verbatim; losing it would force a re-invocation of the Advisor/LLM.
func TestJournal_CompactPreservesInFlightJobFullSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)

	old := time.Now().Add(-10 * 24 * time.Hour)
	decisionPayload := []byte(`{"kind":"triage","summary":"stub decision"}`)

	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateQueued, At: old}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateClaimed, At: old}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateAdvised, At: old, Payload: decisionPayload}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateActing, At: old}))

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	if _, err := j.Compact(cutoff); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load after compact: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected all 4 in-flight records to survive compaction, got %d: %+v", len(records), records)
	}
	var sawAdvisedPayload bool
	for _, r := range records {
		if r.State == StateAdvised {
			if string(r.Payload) != string(decisionPayload) {
				t.Errorf("advised record payload = %s, want %s", r.Payload, decisionPayload)
			}
			sawAdvisedPayload = true
		}
	}
	if !sawAdvisedPayload {
		t.Fatalf("the journaled decision for in-flight job j1 did not survive compaction — replay would have to re-invoke the Advisor")
	}
}

// TestJournal_RoundTripAllStates writes one record per JobState the plan §2.2 defines and proves
// every one survives Append+Load unchanged. queued|superseded|claimed|advised|questioned|acting|
// acted|failed|skipped|done — the full lifecycle vocabulary, including the two "loser" transitions
// (superseded, skipped) that only the dispatcher/sweep produce.
func TestJournal_RoundTripAllStates(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	all := []JobState{
		StateQueued, StateSuperseded, StateClaimed, StateAdvised, StateQuestioned,
		StateActing, StateActed, StateFailed, StateSkipped, StateDone,
	}
	for i, s := range all {
		must(t, j.Append(Record{
			JobID: JobID("kind", "issue", int64(i)), IssueID: "issue", Kind: "kind",
			TriggerSeq: int64(i), State: s,
		}))
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != len(all) {
		t.Fatalf("expected %d records, got %d", len(all), len(records))
	}
	for i, s := range all {
		if records[i].State != s {
			t.Errorf("record %d: state = %s, want %s", i, records[i].State, s)
		}
	}
}

// TestJournal_IsDuplicate proves the dedupe helper: terminal latest state => duplicate, anything
// else (including "no record at all") => not a duplicate.
func TestJournal_IsDuplicate(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	must(t, j.Append(Record{JobID: "in-flight", IssueID: "i1", Kind: "triage", State: StateAdvised}))
	must(t, j.Append(Record{JobID: "finished", IssueID: "i2", Kind: "triage", State: StateDone}))

	cases := []struct {
		jobID string
		want  bool
	}{
		{"in-flight", false},
		{"finished", true},
		{"never-seen", false},
	}
	for _, c := range cases {
		got, err := j.IsDuplicate(c.jobID)
		if err != nil {
			t.Fatalf("IsDuplicate(%s): %v", c.jobID, err)
		}
		if got != c.want {
			t.Errorf("IsDuplicate(%s) = %v, want %v", c.jobID, got, c.want)
		}
	}
}

// TestJournal_HasOpenQuestion proves the query loop/dispatch.go's question_answered OR-arm needs
// (plan §3): a job whose LATEST record is StateQuestioned counts as an open question for its
// issue; a job that has moved past questioned (e.g. to advised, once the FOLLOW-UP resumed it), or
// one belonging to a different issue, does not.
func TestJournal_HasOpenQuestion(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	must(t, j.Append(Record{JobID: "q1", IssueID: "i1", Kind: "followup", State: StateQuestioned}))
	must(t, j.Append(Record{JobID: "q2", IssueID: "i2", Kind: "followup", State: StateQuestioned}))
	must(t, j.Append(Record{JobID: "q2", IssueID: "i2", Kind: "followup", State: StateAdvised, Payload: []byte(`{}`)}))

	got, err := j.HasOpenQuestion("i1")
	if err != nil {
		t.Fatalf("HasOpenQuestion(i1): %v", err)
	}
	if !got {
		t.Fatalf("HasOpenQuestion(i1) = false, want true (job q1's latest record is questioned)")
	}

	got, err = j.HasOpenQuestion("i2")
	if err != nil {
		t.Fatalf("HasOpenQuestion(i2): %v", err)
	}
	if got {
		t.Fatalf("HasOpenQuestion(i2) = true, want false (job q2 moved past questioned to advised)")
	}

	got, err = j.HasOpenQuestion("i3")
	if err != nil {
		t.Fatalf("HasOpenQuestion(i3): %v", err)
	}
	if got {
		t.Fatalf("HasOpenQuestion(i3) = true, want false (no records at all for this issue)")
	}
}

// TestJournal_DecisionForJob proves the resume-from-acting seam (plan §2.2, loop/runner.go's
// resumeFromAdvised): a job's most recent StateAdvised payload is retrievable independent of any
// later record (e.g. "acting"), which carries its own, different payload (batchBodyHash) rather
// than the decision.
func TestJournal_DecisionForJob(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	decision := []byte(`{"kind":"triage","raw":"eyJzdHViIjp0cnVlfQ=="}`)
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateAdvised, Payload: decision}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateActing, Payload: []byte(`{"batchBodyHash":"abc"}`)}))

	got, err := j.DecisionForJob("j1")
	if err != nil {
		t.Fatalf("DecisionForJob: %v", err)
	}
	if string(got) != string(decision) {
		t.Fatalf("DecisionForJob = %s, want the advised payload %s (not the acting record's payload)", got, decision)
	}

	got, err = j.DecisionForJob("never-seen")
	if err != nil {
		t.Fatalf("DecisionForJob(never-seen): %v", err)
	}
	if got != nil {
		t.Fatalf("DecisionForJob(never-seen) = %s, want nil", got)
	}
}

// TestJournal_RecoveryScanReturnsInFlightPayloads proves the REPLAY contract (CONTEXT.md: Advisors
// are never consulted during replay): RecoveryScan must return every non-terminal job together with
// its latest journaled payload, and must NOT return terminal jobs — there is deliberately no
// "re-decide" path in this API, only a payload handoff.
func TestJournal_RecoveryScanReturnsInFlightPayloads(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	decision := []byte(`{"disposition":"fixable"}`)

	must(t, j.Append(Record{JobID: "advised-job", IssueID: "i1", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "advised-job", IssueID: "i1", Kind: "triage", State: StateAdvised, Payload: decision}))
	// acting-job mirrors loop/runner.go's ACTUAL sequence: "advised" carries the decision payload,
	// and the subsequent "acting" record does NOT repeat it (runner.go's StateActing Append has no
	// Payload field set) — RecoveryScan must still hand back the decision for this job so the
	// caller never has to re-consult the Advisor.
	must(t, j.Append(Record{JobID: "acting-job", IssueID: "i2", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "acting-job", IssueID: "i2", Kind: "triage", State: StateAdvised, Payload: decision}))
	must(t, j.Append(Record{JobID: "acting-job", IssueID: "i2", Kind: "triage", State: StateActing}))
	must(t, j.Append(Record{JobID: "finished-job", IssueID: "i3", Kind: "triage", State: StateDone}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 2 {
		t.Fatalf("expected 2 in-flight jobs, got %d: %+v", len(inFlight), inFlight)
	}
	byID := map[string]InFlightJob{}
	for _, job := range inFlight {
		byID[job.JobID] = job
	}
	if _, ok := byID["finished-job"]; ok {
		t.Fatalf("terminal job must not appear in RecoveryScan results")
	}
	advised, ok := byID["advised-job"]
	if !ok {
		t.Fatalf("expected advised-job in RecoveryScan results")
	}
	if advised.State != StateAdvised || string(advised.DecisionPayload) != string(decision) {
		t.Errorf("advised-job: state=%s decisionPayload=%s, want advised/%s", advised.State, advised.DecisionPayload, decision)
	}
	acting, ok := byID["acting-job"]
	if !ok || string(acting.DecisionPayload) != string(decision) {
		t.Fatalf("expected acting-job to carry its journaled DECISION payload for verbatim replay, got %+v", acting)
	}
}

// TestJournal_RecoveryScanKeepsDecisionDistinctFromActingPayload is the red-first regression proof
// for the "last non-empty payload wins" bug: once a job's "acting" record carries its OWN payload
// (batchBodyHash, per plan §2.2), a naive single-Payload RecoveryScan implementation lets that
// value silently overwrite the journaled "advised" decision, leaving the caller with nothing to
// replay and forcing it to re-consult the Advisor — exactly what CONTEXT.md's Replay contract
// forbids ("Advisors are never consulted during replay"). RecoveryScan must keep the two payloads
// in separate fields so the decision survives regardless of what a later record carries.
func TestJournal_RecoveryScanKeepsDecisionDistinctFromActingPayload(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	decision := []byte(`{"disposition":"fixable","batchBody":{"ops":[]}}`)
	actingPayload := []byte(`{"batchBodyHash":"deadbeef"}`)

	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateAdvised, Payload: decision}))
	must(t, j.Append(Record{JobID: "j1", IssueID: "i1", Kind: "triage", State: StateActing, Payload: actingPayload}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected 1 in-flight job, got %d: %+v", len(inFlight), inFlight)
	}
	job := inFlight[0]
	if string(job.DecisionPayload) != string(decision) {
		t.Fatalf("DecisionPayload = %s, want journaled decision %s (must not be overwritten by the acting record's own payload)", job.DecisionPayload, decision)
	}
	if string(job.Payload) != string(actingPayload) {
		t.Fatalf("Payload = %s, want the latest record's own payload %s", job.Payload, actingPayload)
	}
}

// TestJournal_CorruptLineToleratedNotFatal proves plan corruption tolerance: a torn/corrupt line
// (e.g. a process killed mid-Append) is skipped and counted, never fatal to Load — recovery must
// still see every valid record before and after the bad line.
func TestJournal_CorruptLineToleratedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)
	must(t, j.Append(Record{JobID: "before", IssueID: "i1", Kind: "triage", State: StateQueued}))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening journal to inject corruption: %v", err)
	}
	if _, err := f.WriteString("{not valid json, torn write\n"); err != nil {
		t.Fatalf("injecting corrupt line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	must(t, j.Append(Record{JobID: "after", IssueID: "i2", Kind: "triage", State: StateQueued}))

	records, corrupt, err := j.Load()
	if err != nil {
		t.Fatalf("Load must tolerate a corrupt line, got error: %v", err)
	}
	if corrupt != 1 {
		t.Errorf("expected 1 corrupt line counted, got %d", corrupt)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 valid records around the corrupt line, got %d: %+v", len(records), records)
	}
	if records[0].JobID != "before" || records[1].JobID != "after" {
		t.Errorf("unexpected records: %+v", records)
	}
}

// TestJournal_AppendHealsTornTrailingLine proves Append's self-healing guarantee: a crash mid-Append
// can leave record bytes on disk with no terminating '\n'. Without healing, the NEXT Append would
// glue itself onto that torn line, corrupting the next record too. This test injects exactly that
// torn-write shape (no trailing newline) and asserts the next real Append still survives as its own
// parseable record, and the torn line is counted as exactly one corrupt line — never more, never a
// crash.
func TestJournal_AppendHealsTornTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)
	must(t, j.Append(Record{JobID: "before", IssueID: "i1", Kind: "triage", State: StateQueued}))

	// Simulate a crash mid-Append: record bytes land, the terminating '\n' does not.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening journal to inject torn write: %v", err)
	}
	if _, err := f.WriteString(`{"jobId":"torn","issueId":"i2","kind":"triage","state":"queued"`); err != nil {
		t.Fatalf("injecting torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	must(t, j.Append(Record{JobID: "after", IssueID: "i3", Kind: "triage", State: StateQueued}))

	records, corrupt, err := j.Load()
	if err != nil {
		t.Fatalf("Load must tolerate the torn line, got error: %v", err)
	}
	if corrupt != 1 {
		t.Errorf("expected exactly 1 corrupt line counted for the torn write, got %d", corrupt)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 valid records (before, after) surviving the torn write, got %d: %+v", len(records), records)
	}
	if records[0].JobID != "before" {
		t.Errorf("record[0].JobID = %q, want %q", records[0].JobID, "before")
	}
	if records[1].JobID != "after" {
		t.Errorf("record[1].JobID = %q, want %q — the post-crash record must NOT be destroyed by the torn line before it", records[1].JobID, "after")
	}
}

// TestJournal_RecoveryScanPropagatesCorruptCount proves the validator's B3 finding is fixed:
// RecoveryScan must surface the corrupt-line count Load computes, not discard it.
func TestJournal_RecoveryScanPropagatesCorruptCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)
	must(t, j.Append(Record{JobID: "good", IssueID: "i1", Kind: "triage", State: StateQueued}))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening for corruption: %v", err)
	}
	if _, err := f.Write([]byte("{not valid json\n")); err != nil {
		t.Fatalf("writing corrupt line: %v", err)
	}
	must(t, f.Close())

	_, corrupt, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if corrupt != 1 {
		t.Fatalf("expected RecoveryScan to report 1 corrupt line, got %d", corrupt)
	}
}

// TestJournal_CompactPropagatesCorruptCount proves Compact reports the corrupt lines it is about
// to permanently erase, rather than silently dropping them with no signal to the caller.
func TestJournal_CompactPropagatesCorruptCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.journal")
	j := OpenJournal(path)
	must(t, j.Append(Record{JobID: "good", IssueID: "i1", Kind: "triage", State: StateQueued}))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening for corruption: %v", err)
	}
	if _, err := f.Write([]byte("{not valid json\n")); err != nil {
		t.Fatalf("writing corrupt line: %v", err)
	}
	must(t, f.Close())

	corrupt, err := j.Compact(time.Now().Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if corrupt != 1 {
		t.Fatalf("expected Compact to report 1 corrupt line about to be erased, got %d", corrupt)
	}
}

// TestJournal_RecoveryScanOrdersByJournalAppearance proves RecoveryScan returns in-flight jobs in
// journal (first-appearance) order, not Go's nondeterministic map-iteration order — the validator's
// finding that same-issue jobs could replay out of order after a crash. Two jobs for the same issue
// are appended interleaved with several other jobs so a map-order implementation would frequently
// (not always) misorder them; run repeatedly this reliably catches it via t.Run subtests below is
// unnecessary because a fixed slice built from first-appearance index is deterministic every run.
func TestJournal_RecoveryScanOrdersByJournalAppearance(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))

	// Journal order: triage-1 (queued), other-a, other-b, followup-1 (queued), other-c, other-d.
	// triage-1 appears first, so it must come first in RecoveryScan's result.
	must(t, j.Append(Record{JobID: "triage-1", IssueID: "same-issue", Kind: "triage", State: StateActing}))
	must(t, j.Append(Record{JobID: "other-a", IssueID: "ia", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "other-b", IssueID: "ib", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "followup-1", IssueID: "same-issue", Kind: "followup", State: StateQueued}))
	must(t, j.Append(Record{JobID: "other-c", IssueID: "ic", Kind: "triage", State: StateQueued}))
	must(t, j.Append(Record{JobID: "other-d", IssueID: "id", Kind: "triage", State: StateQueued}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	wantOrder := []string{"triage-1", "other-a", "other-b", "followup-1", "other-c", "other-d"}
	if len(inFlight) != len(wantOrder) {
		t.Fatalf("expected %d in-flight jobs, got %d: %+v", len(wantOrder), len(inFlight), inFlight)
	}
	for i, want := range wantOrder {
		if inFlight[i].JobID != want {
			t.Fatalf("inFlight[%d].JobID = %q, want %q (journal-appearance order) — got order: %v",
				i, inFlight[i].JobID, want, jobIDsOf(inFlight))
		}
	}
}

// TestJournal_ConcurrentAppendAndLatestByJobID races Append against LatestByJobID (the finding-5
// in-memory index both read/write, guarded by Journal.mu) to prove the index never causes a data
// race or a lost update visible to a subsequent read. Run with -race.
func TestJournal_ConcurrentAppendAndLatestByJobID(t *testing.T) {
	j := OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))

	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				jobID := fmt.Sprintf("job-%d", w)
				if err := j.Append(Record{JobID: jobID, IssueID: "i", Kind: "triage", State: StateQueued}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}

	// Concurrent readers exercising LatestByJobID while writers are still appending.
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := j.LatestByJobID(); err != nil {
					t.Errorf("LatestByJobID: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWg.Wait()

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	if len(latest) != writers {
		t.Fatalf("expected %d distinct jobIds in the index, got %d: %+v", writers, len(latest), latest)
	}
	for w := 0; w < writers; w++ {
		jobID := fmt.Sprintf("job-%d", w)
		if _, ok := latest[jobID]; !ok {
			t.Fatalf("expected %s in LatestByJobID after concurrent appends, got %+v", jobID, latest)
		}
	}
}

func jobIDsOf(jobs []InFlightJob) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.JobID
	}
	return ids
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

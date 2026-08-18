package state

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JobState is one of the job-journal's state-transition labels (plan §2.2). Terminal states are
// done|failed|skipped|superseded — a re-delivered event whose jobId already has ANY terminal
// record is dropped (dedupe).
type JobState string

const (
	StateQueued     JobState = "queued"
	StateSuperseded JobState = "superseded"
	StateClaimed    JobState = "claimed"
	StateAdvised    JobState = "advised"
	StateQuestioned JobState = "questioned"
	StateActing     JobState = "acting"
	StateActed      JobState = "acted"
	StateDone       JobState = "done"
	StateFailed     JobState = "failed"
	StateSkipped    JobState = "skipped"
)

// Terminal states end a job's lifecycle: no further transitions are ever appended for the jobId,
// and a re-delivered event that maps to it is dropped as a duplicate (plan §2.2 "Dedupe").
var terminalStates = map[JobState]bool{
	StateSuperseded: true,
	StateDone:       true,
	StateFailed:     true,
	StateSkipped:    true,
}

// IsTerminal reports whether s is one of the journal's terminal states.
func (s JobState) IsTerminal() bool { return terminalStates[s] }

// Record is one line of jobs.journal: `{jobId, issueId, kind, triggerSeq, state, at, payload?}`
// (plan §2.2). Payload carries the Advisor's decision JSON + compiled batch body once state
// reaches "advised", so recovery can replay a journaled batch verbatim without ever re-invoking
// the LLM.
type Record struct {
	JobID      string          `json:"jobId"`
	IssueID    string          `json:"issueId"`
	Kind       string          `json:"kind"`
	TriggerSeq int64           `json:"triggerSeq"`
	State      JobState        `json:"state"`
	At         time.Time       `json:"at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// JobID derives the plan §2.2 stable job identifier: hash(kind + issueId + triggerSeq). Stable
// across restarts and across bootstrap re-enqueues (bootstrap synthesizes its own triggerSeq
// convention per plan §2.1, but the derivation itself is uniform).
func JobID(kind, issueID string, triggerSeq int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", kind, issueID, triggerSeq)))
	return hex.EncodeToString(sum[:])[:32]
}

// Journal is the append-only NDJSON job journal ($WORKER_STATE_DIR/jobs.journal). All writes are
// appended under a mutex serializing this process's own writers (single Agent Worker process per
// state volume, plan §2.2 "do not scale >1 replica per state volume").
type Journal struct {
	path string
	mu   sync.Mutex
	// index is the latest-record-per-jobId view, built once at open and kept current by every
	// Append (and rebuilt by Compact after it drops jobs). It is what LatestByJobID/IsDuplicate
	// read, so the per-event dedupe check on the hot path is O(1) instead of re-reading and
	// re-parsing the entire journal file (finding: dedupe was O(journal) per event). Load/
	// RecoveryScan/Compact still re-read the file directly — they need the full ordered record
	// sequence (or, for RecoveryScan, journal-appearance order), not just the latest-per-job view,
	// and they are maintenance-pass operations, not per-event hot path. Guarded by mu. A nil index
	// means "not yet built" (e.g. OpenJournal's initial Load failed for a reason other than a
	// missing file) — callers fall back to a full Load in that case, so correctness never depends
	// on the index having been built successfully.
	index map[string]Record
}

// OpenJournal returns a Journal bound to path and, best-effort, replays the existing file once to
// build the in-memory latest-per-jobId index Append/LatestByJobID maintain from then on. A missing
// file (fresh state volume) yields an empty index, not an error. If the initial replay fails for any
// other reason, index is left nil and LatestByJobID transparently falls back to a full Load per call
// until the next successful Append or Compact repairs it.
func OpenJournal(path string) *Journal {
	j := &Journal{path: path}
	if records, _, err := j.Load(); err == nil {
		idx := make(map[string]Record, len(records))
		for _, r := range records {
			idx[r.JobID] = r
		}
		j.index = idx
	}
	return j
}

// Load replays every record in the journal file in order. A missing file yields an empty slice,
// not an error — a fresh state volume has no journal yet.
//
// Corrupt-line tolerance: a line that fails to json.Unmarshal (e.g. a torn trailing write from a
// crash mid-Append, or any other on-disk corruption) is SKIPPED, not fatal — the journal is the
// load-bearing durability layer (plan §2.2) and a single bad line must never crash recovery. The
// second return value counts how many lines were skipped so the caller can log it loudly.
func (j *Journal) Load() ([]Record, int, error) {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var records []Record
	corrupt := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			corrupt++
			continue
		}
		records = append(records, r)
	}
	if err := scanner.Err(); err != nil {
		return records, corrupt, fmt.Errorf("reading %s: %w", j.path, err)
	}
	return records, corrupt, nil
}

// LatestByJobID returns the most recent record per jobId — the view the dedupe check and
// crash-recovery replay both need. It reads the in-memory index built at open and maintained by
// every Append (O(1) per lookup, O(jobs) to copy out), not the file — see Journal.index's doc
// comment. If the index was never successfully built, it falls back to a full Load and populates
// the index from that, so a transient failure at open self-heals on the next call.
func (j *Journal) LatestByJobID() (map[string]Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.index == nil {
		records, _, err := j.Load()
		if err != nil {
			return nil, err
		}
		// The corrupt-line count is intentionally discarded here: LatestByJobID's callers
		// (IsDuplicate, dedupe checks) don't perform a full maintenance pass, and
		// RecoveryScan/Compact — the two callers that DO run as part of a maintenance pass —
		// surface the count themselves.
		idx := make(map[string]Record, len(records))
		for _, r := range records {
			idx[r.JobID] = r
		}
		j.index = idx
	}

	latest := make(map[string]Record, len(j.index))
	for k, v := range j.index {
		latest[k] = v
	}
	return latest, nil
}

// IsDuplicate reports whether jobID already has a terminal record in the journal (plan §2.2
// "Dedupe": a re-delivered event whose jobId has any terminal record is dropped).
func (j *Journal) IsDuplicate(jobID string) (bool, error) {
	latest, err := j.LatestByJobID()
	if err != nil {
		return false, err
	}
	r, ok := latest[jobID]
	if !ok {
		return false, nil
	}
	return r.State.IsTerminal(), nil
}

// HasOpenQuestion reports whether issueID has any job whose LATEST journal record is
// StateQuestioned — a blocking question we asked that has not yet been resolved forward (the
// question_answered dispatch OR-arm, plan §3: "FOLLOW-UP if assignedTo == me OR journal shows my
// open question"). "Latest" matters: once the FOLLOW-UP job that consumes the answer advances
// past questioned (to advised/acting/done/...), the question is no longer open.
func (j *Journal) HasOpenQuestion(issueID string) (bool, error) {
	latest, err := j.LatestByJobID()
	if err != nil {
		return false, err
	}
	for _, r := range latest {
		if r.IssueID == issueID && r.State == StateQuestioned {
			return true, nil
		}
	}
	return false, nil
}

// DecisionForJob returns the payload journaled at jobID's most recent StateAdvised record — the
// Advisor's decision the job must replay verbatim on resume (plan §2.2: "the LLM is NEVER
// re-invoked for a job that already produced a decision"). Used by Runner.Run to resume a job
// found sitting at StateActing, whose own record carries a batchBodyHash, not the decision itself
// (see InFlightJob's doc comment for why the two are journaled separately). Returns a nil payload,
// no error, if the job has no advised record (e.g. it was never re-decided, which should not
// happen for a job that reached acting, but callers must not panic on it).
func (j *Journal) DecisionForJob(jobID string) (json.RawMessage, error) {
	records, _, err := j.Load()
	if err != nil {
		return nil, err
	}
	var payload json.RawMessage
	for _, r := range records {
		if r.JobID == jobID && r.State == StateAdvised && len(r.Payload) > 0 {
			payload = r.Payload
		}
	}
	return payload, nil
}

// InFlightJob is one job the Recovery scan found sitting in a non-terminal state, along with its
// journaled payloads. Per CONTEXT.md's Replay contract, recovery never re-derives a decision: it
// hands back exactly what was journaled, and the caller (loop/runner.go) re-executes it verbatim —
// there is deliberately no "re-decide" path in this API.
//
// The plan (§2.2) journals a payload at more than one state for the SAME job — "advised" carries
// the Advisor's decision JSON + compiled batch body, and a later "acting" record carries its own
// payload (batchBodyHash), written just before the batch POST. These are two distinct records, not
// a single evolving one, so InFlightJob keeps them in two distinct fields rather than collapsing
// them: doing so ("last non-empty payload wins") would let a job that reached "acting" silently
// overwrite its journaled decision with the acting payload, leaving the caller with no decision to
// replay and forcing it to re-consult the Advisor — exactly what the Replay contract forbids.
type InFlightJob struct {
	JobID   string
	IssueID string
	Kind    string
	State   JobState
	// Payload is the latest record's OWN payload (may be empty — e.g. a bare "acting" record with
	// no payload field set, or "claimed"/"questioned"/"queued", none of which carry one).
	Payload json.RawMessage
	// DecisionPayload is the payload from this job's most recent "advised" record — the Advisor's
	// decision JSON and compiled batch body. It is populated independently of Payload so a later
	// record (e.g. "acting") can never cause the decision to be lost; recovery from advised, acting,
	// or any other non-terminal state downstream of "advised" always finds it here.
	DecisionPayload json.RawMessage
	// TriggerSeq is the latest record's own TriggerSeq — the originating event's feed sequence
	// number, needed by the caller (loop/runner.go's Resume) to reconstruct a minimal Event to
	// drive the job through JobID-consistent Append calls without re-deriving a new jobId.
	TriggerSeq int64
}

// RecoveryScan replays the journal and returns every job whose latest record is NOT terminal —
// the work a restart must resume (plan §2.2, CONTEXT.md's "Recovery": "scan the journal, then
// replay or resume each in-flight job"). Each entry carries both its latest record's own payload
// and, separately, the decision payload journaled at "advised" (if any), so the caller can replay
// verbatim without ever re-invoking an Advisor — see InFlightJob's doc comment for why the two are
// kept distinct.
// RecoveryScan replays the journal and returns every job whose latest record is NOT terminal —
// the work a restart must resume — plus the number of corrupt lines skipped while doing so (the
// same count Load produces), so callers no longer have to discard it. The returned slice is
// ordered by each job's FIRST appearance in the journal, not map-iteration order: CONTEXT.md's
// Recovery contract ("replay or resume each in-flight job") and plan §3's per-issue serial queues
// both assume replay preserves journal order, and a crash can legitimately leave two non-terminal
// records for the same issue (e.g. a TRIAGE at "acting" with a FOLLOW-UP still "queued" behind
// it) — replaying those out of order would publish the follow-up before the triage it depends on.
func (j *Journal) RecoveryScan() ([]InFlightJob, int, error) {
	records, corrupt, err := j.Load()
	if err != nil {
		return nil, corrupt, err
	}
	latest := make(map[string]Record, len(records))
	decisionPayload := make(map[string]json.RawMessage, len(records))
	order := make([]string, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		latest[r.JobID] = r
		if !seen[r.JobID] {
			seen[r.JobID] = true
			order = append(order, r.JobID)
		}
		if r.State == StateAdvised && len(r.Payload) > 0 {
			decisionPayload[r.JobID] = r.Payload
		}
	}
	var inFlight []InFlightJob
	for _, jobID := range order {
		r := latest[jobID]
		if r.State.IsTerminal() {
			continue
		}
		inFlight = append(inFlight, InFlightJob{
			JobID:           r.JobID,
			IssueID:         r.IssueID,
			Kind:            r.Kind,
			State:           r.State,
			Payload:         r.Payload,
			DecisionPayload: decisionPayload[jobID],
			TriggerSeq:      r.TriggerSeq,
		})
	}
	return inFlight, corrupt, nil
}

// Append writes one record to the journal, appending a newline-delimited JSON line and calling
// Sync so the record survives a crash immediately after the write returns. Appends are not
// themselves atomic against a torn write (unlike cursor.go's tmp+rename replace): a crash mid-Append
// can leave a trailing line whose bytes landed but whose terminating '\n' did not. Left alone, that
// torn line would glue itself to the FRONT of the next record ever appended (bufio.Scanner has no
// line boundary between them), corrupting that next record too — including, catastrophically, a
// terminal ("done"/"skipped"/"failed") record, which would then never satisfy IsDuplicate and cause
// the job to be re-run (Advisor re-invoked, side effects re-issued) on every restart. Append
// self-heals this: before writing, it checks whether the file already ends in '\n' and, if not,
// writes a leading '\n' first to re-establish the line boundary. The torn line itself still fails
// json.Unmarshal in Load and is skipped as corrupt (counted, not fatal) — only the record written
// AFTER a torn line is protected from being destroyed by it.
func (j *Journal) Append(r Record) error {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling journal record: %w", err)
	}
	data = append(data, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()

	// Belt-and-braces dedupe guard (plan §2.2): once a jobId's latest record is terminal, no
	// further record for that jobId is ever written. This is a second line of defense behind
	// Dispatcher.Enqueue's own IsDuplicate check -- it protects every OTHER caller that appends by
	// jobId (Runner.Run's defense-in-depth re-check, Bootstrap's re-enqueue path, a future caller
	// we haven't audited) from silently regressing a completed job's terminal record back to a
	// live one, which would blind IsTerminal()/IsDuplicate() to the fact that the job already ran.
	if j.index != nil {
		if existing, ok := j.index[r.JobID]; ok && existing.State.IsTerminal() && !r.State.IsTerminal() {
			return nil
		}
	}

	dir := filepath.Dir(j.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	// O_RDWR (not O_WRONLY): fileEndsWithoutNewline needs to read the file's final byte to detect a
	// torn trailing write before appending.
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening journal %s: %w", j.path, err)
	}
	defer f.Close()

	if needsLeadingNewline, err := fileEndsWithoutNewline(f); err != nil {
		return fmt.Errorf("checking journal tail %s: %w", j.path, err)
	} else if needsLeadingNewline {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("healing torn journal line: %w", err)
		}
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("appending journal record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}

	if j.index == nil {
		j.index = make(map[string]Record)
	}
	j.index[r.JobID] = r
	return nil
}

// fileEndsWithoutNewline reports whether f (opened O_APPEND) is non-empty and its final byte is not
// '\n' — the signature a crash mid-Append leaves behind.
func fileEndsWithoutNewline(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, info.Size()-1); err != nil {
		return false, err
	}
	return buf[0] != '\n', nil
}

// Compact rewrites the journal DROPPING every record belonging to a jobId whose latest record is
// terminal and older than `olderThan` (plan §2.2: "on start and daily, rewrite the journal
// dropping records of jobs terminal for >7 days"), while preserving the FULL record sequence —
// every state transition, in original order — for every job it retains. It must never collapse a
// retained job down to just its latest record: an in-flight job's "advised" record carries the
// Advisor's decision payload and the compiled batch body, and recovery from advised/acting replays
// that payload verbatim without ever re-invoking the LLM (plan §2.2). Collapsing to "latest record
// only" would silently drop that payload for any job currently sitting in advised/acting/claimed/
// questioned/queued. Uses the same tmp+rename pattern as cursor.go for crash safety.
//
// Compact rewrites the journal from the RECORDS Load() managed to parse — any corrupt line Load
// skipped is therefore permanently erased by the rewrite, not merely skipped-for-now. The returned
// int is the corrupt-line count Load produced, specifically so a caller can log/count that loss
// BEFORE it becomes irrecoverable (validator finding: this used to be silent and permanent).
func (j *Journal) Compact(olderThan time.Time) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	records, corrupt, err := j.Load()
	if err != nil {
		return corrupt, err
	}
	latest := make(map[string]Record, len(records))
	for _, r := range records {
		latest[r.JobID] = r
	}

	dir := filepath.Dir(j.path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".jobs-journal-*.tmp")
	if err != nil {
		return corrupt, fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	w := bufio.NewWriter(tmp)
	for _, r := range records {
		final := latest[r.JobID]
		if final.State.IsTerminal() && final.At.Before(olderThan) {
			// This job's most recent record is terminal and old enough to drop — drop ALL of
			// this job's records, not just this one, so no orphaned intermediate line survives.
			continue
		}
		data, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			return corrupt, fmt.Errorf("marshaling journal record: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			tmp.Close()
			return corrupt, fmt.Errorf("writing compacted journal: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return corrupt, fmt.Errorf("flushing compacted journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return corrupt, fmt.Errorf("syncing compacted journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return corrupt, fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, j.path); err != nil {
		return corrupt, fmt.Errorf("renaming %s to %s: %w", tmpPath, j.path, err)
	}
	fsyncDir(dir)

	newIndex := make(map[string]Record, len(latest))
	for jobID, final := range latest {
		if final.State.IsTerminal() && final.At.Before(olderThan) {
			continue // dropped by the rewrite above; must not survive in the index either
		}
		newIndex[jobID] = final
	}
	j.index = newIndex
	return corrupt, nil
}

package unit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

// sentinel-grouping-v2 repair R3: the v1-vs-v2 comparison over a REAL corpus of stored occurrences.
//
// GROUPING_CORPUS names a JSONL file, one occurrence per line (project, currentIssueId, environment,
// platform, errorClass, message, stacktrace[{file,function,line,in_app}]). The file is never
// committed and its messages never leave this test: the report prints only anonymised issue labels
// (W01.., Z01.., T01..), the error class and counts. The test skips when the variable is unset.
// GROUPING_CORPUS_REPORT, when set, is a path the markdown report is written to.
//
//	GROUPING_CORPUS=~/.cache/sentinel-grouping/corpus.jsonl go test ./tests/unit -run TestGroupingV2Corpus -v

type corpusRecord struct {
	Project        string `json:"project"`
	CurrentIssueID string `json:"currentIssueId"`
	Environment    string `json:"environment"`
	Platform       string `json:"platform"`
	ErrorClass     string `json:"errorClass"`
	Message        string `json:"message"`
	Stacktrace     []struct {
		File     string `json:"file"`
		Function string `json:"function"`
		Line     int32  `json:"line"`
		InApp    bool   `json:"in_app"`
	} `json:"stacktrace"`
}

func (r corpusRecord) config() fingerprint.FingerprintConfig { return r.configWith(false) }

// isTurbopackSource reports a frame whose file is already a source-level Turbopack path
// (`turbopack:///[project]/...`, `[turbopack]/browser/runtime/...`): only the symbolication stage
// produces such paths, so the export that dropped the `symbolicated` key lost it for these frames.
func isTurbopackSource(file string) bool {
	return strings.HasPrefix(file, "turbopack:///") || strings.Contains(file, "[turbopack]/")
}

// configWith is config, optionally treating the turbopack source-level frames as symbolicated.
func (r corpusRecord) configWith(turbopackSymbolicated bool) fingerprint.FingerprintConfig {
	frames := make([]fingerprint.StackFrame, len(r.Stacktrace))
	for i, f := range r.Stacktrace {
		// The stored frame carries `symbolicated` only when the stage rewrote it (omitempty), and the
		// corpus has none, so every frame is a frame symbolication did not rewrite.
		frames[i] = fingerprint.StackFrame{File: f.File, Line: f.Line, Function: f.Function, InApp: f.InApp,
			Symbolicated: turbopackSymbolicated && isTurbopackSource(f.File)}
	}
	return fingerprint.FingerprintConfig{
		Environment: r.Environment, ErrorClass: normalizer.NewNormalizer().NormalizeString(r.ErrorClass),
		Message: r.Message, Platform: r.Platform, NormalisePlatforms: symbolicate.DefaultPlatforms, Stacktrace: frames,
	}
}

// v1Production is the pre-v2 Compute exactly as it ran: file normalised for the gated platforms,
// function verbatim, the first three in_app frames else the first three, no message, no version.
func v1Production(cfg fingerprint.FingerprintConfig) string {
	label := func(f fingerprint.StackFrame) string {
		file := f.File
		if cfg.Platform == "javascript" || cfg.Platform == "browser" {
			file = fingerprint.NormalizeHashInput(file)
		}
		return file + ":" + f.Function
	}
	var parts []string
	for _, f := range cfg.Stacktrace {
		if f.InApp && len(parts) < 3 {
			parts = append(parts, label(f))
		}
	}
	if len(parts) == 0 {
		for _, f := range cfg.Stacktrace {
			if len(parts) < 3 {
				parts = append(parts, label(f))
			}
		}
	}
	input := cfg.Environment + "|" + cfg.ErrorClass
	if len(parts) > 0 {
		input += "|" + strings.Join(parts, "|")
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

func TestGroupingV2Corpus(t *testing.T) {
	path := os.Getenv("GROUPING_CORPUS")
	if path == "" {
		t.Skip("GROUPING_CORPUS is not set; the real-corpus comparison needs a local JSONL export")
	}
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var recs []corpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r corpusRecord
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		recs = append(recs, r)
	}
	require.NoError(t, sc.Err())

	// Some stored occurrences carry an empty errorClass while a sibling occurrence of the SAME
	// current issue carries the class the issue was created with (the issue row has one class; the
	// occurrence export does not always). v1 hashed that class, so an empty one would split the issue
	// in the reconstruction for a reason production never had. Fill it from the issue's non-empty
	// class and count how often that was needed; an issue with no non-empty class keeps "".
	issueClass := map[string]string{}
	for _, r := range recs {
		if r.ErrorClass != "" && issueClass[r.CurrentIssueID] == "" {
			issueClass[r.CurrentIssueID] = r.ErrorClass
		}
	}
	repaired := 0
	for i := range recs {
		if recs[i].ErrorClass == "" && issueClass[recs[i].CurrentIssueID] != "" {
			recs[i].ErrorClass = issueClass[recs[i].CurrentIssueID]
			repaired++
		}
	}

	limits := []int{3, 5, 8}
	byProject := map[string][]corpusRecord{}
	for _, r := range recs {
		byProject[r.Project] = append(byProject[r.Project], r)
	}
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	prefix := map[string]string{"web": "W", "zasri-mobile": "Z", "tuju-mobile": "T"}

	var sb strings.Builder
	fmt.Fprintf(&sb, "| project | occurrences | issues today | v1 reconstruction | v2 N=3 | v2 N=5 | v2 N=8 |\n|---|---|---|---|---|---|---|\n")
	var detail strings.Builder
	emptyMsg := 0
	for _, r := range recs {
		if r.Message == "" {
			emptyMsg++
		}
	}
	fmt.Fprintf(&detail, "\n%d of %d occurrences had an empty errorClass and took their issue's class; %d have an empty message.\n", repaired, len(recs), emptyMsg)
	for _, p := range projects {
		rs := byProject[p]
		current := map[string]bool{}
		v1 := map[string]bool{}
		v2 := map[int]map[string]bool{3: {}, 5: {}, 8: {}}
		for _, r := range rs {
			cfg := r.config()
			current[r.CurrentIssueID] = true
			v1[v1Production(cfg)] = true
			for _, n := range limits {
				v2[n][fingerprint.ComputeWithFrameLimit(cfg, n)] = true
			}
		}
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %d | %d | %d |\n", p, len(rs), len(current), len(v1), len(v2[3]), len(v2[5]), len(v2[8]))

		// Anonymised labels: current issues in sorted-id order.
		ids := make([]string, 0, len(current))
		for id := range current {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		label := map[string]string{}
		class := map[string]string{}
		size := map[string]int{}
		for i, id := range ids {
			label[id] = fmt.Sprintf("%s%02d", prefix[p], i+1)
		}
		for _, r := range rs {
			class[r.CurrentIssueID] = r.ErrorClass
			size[r.CurrentIssueID]++
		}

		// A v1 reconstruction that does not reproduce today's issues makes every pair below suspect.
		var mismatch []string
		v1Owner := map[string]map[string]bool{}
		for _, r := range rs {
			k := r.Environment + "|" + v1Production(r.config())
			if v1Owner[k] == nil {
				v1Owner[k] = map[string]bool{}
			}
			v1Owner[k][r.CurrentIssueID] = true
		}
		for k, owners := range v1Owner {
			if len(owners) > 1 {
				mismatch = append(mismatch, k)
			}
		}
		fmt.Fprintf(&detail, "\n#### %s\n\nv1 reconstruction vs today's issues: %d v1 groups own more than one current issue; %d current issues own more than one v1 group.\n",
			p, len(mismatch), multiGroup(rs, func(r corpusRecord) string { return v1Production(r.config()) }))

		// Merges and splits at N=5: current issue -> set of v2 fingerprints (split), v2 fingerprint
		// (per environment) -> set of current issues (merge).
		fp := func(r corpusRecord) string { return r.Environment + "|" + fingerprint.Compute(r.config()) }
		splits := map[string]map[string]int{}
		merges := map[string]map[string]bool{}
		for _, r := range rs {
			k := fp(r)
			if splits[r.CurrentIssueID] == nil {
				splits[r.CurrentIssueID] = map[string]int{}
			}
			splits[r.CurrentIssueID][k]++
			if merges[k] == nil {
				merges[k] = map[string]bool{}
			}
			merges[k][r.CurrentIssueID] = true
		}
		detail.WriteString("\nMerged (several current issues become one v2 issue):\n\n")
		anyMerge := false
		mkeys := make([]string, 0, len(merges))
		for k := range merges {
			mkeys = append(mkeys, k)
		}
		sort.Strings(mkeys)
		for _, k := range mkeys {
			if len(merges[k]) < 2 {
				continue
			}
			anyMerge = true
			var parts []string
			for id := range merges[k] {
				parts = append(parts, fmt.Sprintf("%s (%s, %d occ.)", label[id], class[id], size[id]))
			}
			sort.Strings(parts)
			fmt.Fprintf(&detail, "- %s [%s]\n", strings.Join(parts, " + "), strings.SplitN(k, "|", 2)[0])
		}
		if !anyMerge {
			detail.WriteString("- none\n")
		}
		detail.WriteString("\nSplit (one current issue becomes several v2 issues):\n\n")
		anySplit := false
		for _, id := range ids {
			if len(splits[id]) < 2 {
				continue
			}
			anySplit = true
			var counts []string
			for _, n := range splits[id] {
				counts = append(counts, fmt.Sprint(n))
			}
			sort.Strings(counts)
			fmt.Fprintf(&detail, "- %s (%s, %d occ.) -> %d issues, occurrences %s\n", label[id], class[id], size[id], len(splits[id]), strings.Join(counts, "/"))
		}
		if !anySplit {
			detail.WriteString("- none\n")
		}
	}

	// The export's two known losses, repaired in turn so the headline number is read with them: 125
	// rows predate error_occurrences.message (a sibling occurrence of the same issue carries it), and
	// the `symbolicated` key is gone (see isTurbopackSource). N=5; the widest fan-out is the largest
	// number of v2 issues one current issue splits into, which the acceptance bound limits to 5.
	issueMsg := map[string]string{}
	for _, r := range recs {
		if r.Message != "" && issueMsg[r.CurrentIssueID] == "" {
			issueMsg[r.CurrentIssueID] = r.Message
		}
	}
	type variant struct {
		name         string
		repairMsg    bool
		turbopackSym bool
	}
	fmt.Fprintf(&detail, "\nv2 N=5 under the export's known losses (issues / widest split of one current issue):\n\n| variant | web | zasri-mobile | tuju-mobile |\n|---|---|---|---|\n")
	for _, v := range []variant{{"as exported", false, false}, {"messages repaired from siblings", true, false}, {"messages repaired + turbopack frames symbolicated", true, true}} {
		cells := map[string]string{}
		for _, p := range projects {
			groups := map[string]bool{}
			perIssue := map[string]map[string]bool{}
			for _, r := range byProject[p] {
				if v.repairMsg && r.Message == "" {
					r.Message = issueMsg[r.CurrentIssueID]
				}
				k := r.Environment + "|" + fingerprint.Compute(r.configWith(v.turbopackSym))
				groups[k] = true
				if perIssue[r.CurrentIssueID] == nil {
					perIssue[r.CurrentIssueID] = map[string]bool{}
				}
				perIssue[r.CurrentIssueID][k] = true
			}
			widest := 0
			for _, g := range perIssue {
				if len(g) > widest {
					widest = len(g)
				}
			}
			cells[p] = fmt.Sprintf("%d / %d", len(groups), widest)
		}
		fmt.Fprintf(&detail, "| %s | %s | %s | %s |\n", v.name, cells["web"], cells["zasri-mobile"], cells["tuju-mobile"])
	}

	report := "\n" + sb.String() + detail.String()
	t.Log(report)
	if out := os.Getenv("GROUPING_CORPUS_REPORT"); out != "" {
		require.NoError(t, os.WriteFile(out, []byte(report), 0o600))
	}
}

// multiGroup counts current issues whose occurrences fall into more than one group of key.
func multiGroup(rs []corpusRecord, key func(corpusRecord) string) int {
	seen := map[string]map[string]bool{}
	for _, r := range rs {
		if seen[r.CurrentIssueID] == nil {
			seen[r.CurrentIssueID] = map[string]bool{}
		}
		seen[r.CurrentIssueID][r.Environment+"|"+key(r)] = true
	}
	n := 0
	for _, g := range seen {
		if len(g) > 1 {
			n++
		}
	}
	return n
}

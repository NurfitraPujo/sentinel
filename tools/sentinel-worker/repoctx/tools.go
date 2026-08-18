package repoctx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
)

// ToolNameReadFile / ToolNameSearchCode are the tool names exposed to the model — the N8d
// Advisors register these against the Repo resolved for the current job (plan §4.5: "Expose as
// Advisor ToolFuncs ... the N8d Advisors consume these").
const (
	ToolNameReadFile   = "read_file"
	ToolNameSearchCode = "search_code"
)

// ToolDefs returns the llm.ToolDef pair describing read_file/search_code, provider-neutral JSON
// schemas for the model's tool-calling turn.
func ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        ToolNameReadFile,
			Description: "Read a file from the repository checkout. Optionally restrict to a 1-indexed inclusive line range. Output is byte-capped.",
			Params: llm.Schema{
				Type: "object",
				Properties: map[string]llm.Schema{
					"path":      {Type: "string", Description: "Path to the file, relative to the repository root. Must not be absolute or reference .git."},
					"startLine": {Type: "integer", Description: "1-indexed inclusive start line. Omit (or 0) for the start of the file.", Nullable: true},
					"endLine":   {Type: "integer", Description: "1-indexed inclusive end line. Omit (or 0) for the end of the file.", Nullable: true},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        ToolNameSearchCode,
			Description: "Search tracked files in the repository checkout for a pattern (git grep -n semantics). Optionally restrict to a glob pathspec. Results are count- and byte-capped.",
			Params: llm.Schema{
				Type: "object",
				Properties: map[string]llm.Schema{
					"pattern": {Type: "string", Description: "Pattern to search for."},
					"glob":    {Type: "string", Description: "Optional pathspec/glob to restrict the search, relative to the repository root (no absolute paths, no '..').", Nullable: true},
				},
				Required: []string{"pattern"},
			},
		},
	}
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

type searchCodeArgs struct {
	Pattern string `json:"pattern"`
	Glob    string `json:"glob"`
}

// ReadFileToolFunc binds ReadFile against repo as an llm.ToolFunc. Results are returned raw
// (unfiltered) — the plan places guard tool-output tracking in the Advisor toolchain that wires
// this func in, not here.
func ReadFileToolFunc(repo *Repo) llm.ToolFunc {
	return func(_ context.Context, arguments string) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("repoctx: invalid %s arguments: %w", ToolNameReadFile, err)
		}
		return ReadFile(repo, args.Path, args.StartLine, args.EndLine)
	}
}

// SearchCodeToolFunc binds SearchCode against repo as an llm.ToolFunc.
func SearchCodeToolFunc(repo *Repo) llm.ToolFunc {
	return func(ctx context.Context, arguments string) (string, error) {
		var args searchCodeArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("repoctx: invalid %s arguments: %w", ToolNameSearchCode, err)
		}
		return SearchCode(ctx, repo, args.Pattern, args.Glob)
	}
}

// Tools returns the read_file/search_code tool map bound against repo, ready to merge into an
// llm.RunLoop call's tools argument.
func Tools(repo *Repo) map[string]llm.ToolFunc {
	return map[string]llm.ToolFunc{
		ToolNameReadFile:   ReadFileToolFunc(repo),
		ToolNameSearchCode: SearchCodeToolFunc(repo),
	}
}

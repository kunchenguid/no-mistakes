package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DocumentStep updates project documentation to reflect code changes.
type DocumentStep struct{}

func (s *DocumentStep) Name() types.StepName { return types.StepDocument }

// documentVerdictSchema is the JSON schema for the document step's structured output.
var documentVerdictSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"verdict": {"type": "string", "enum": ["updated", "skipped"]},
		"summary": {"type": "string"},
		"details": {"type": "string"}
	},
	"required": ["verdict", "summary"]
}`)

type documentVerdict struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
}

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// Check whether there are any changed files.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if strings.TrimSpace(changedFiles) == "" {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}

	sctx.Log("updating documentation...")

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	prompt := fmt.Sprintf(
		`Review the code changes and update any project documentation that needs to reflect the changes.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s
- ignore patterns: %s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed.
   - Identify the intent and scope of the change (new feature, API change, config change, behavioral change, etc.).

2. Identify documentation that needs updating
   - Look for existing documentation files in the project: README.md, docs/, doc comments, config examples, etc.
   - Determine which docs are affected by the change. Common cases:
     - New or changed public APIs - update API docs, doc comments, or usage examples
     - New features or behaviors - update README or relevant guide
     - Changed configuration - update config docs or examples
     - Removed functionality - remove or update stale references

3. Make documentation updates
   - Update only the documentation that is directly affected by this change.
   - Keep updates minimal and accurate - do not rewrite docs that are already correct.
   - Match the existing documentation style and conventions of the project.
   - If no documentation updates are needed (e.g., internal refactoring, test-only changes), skip.

Rules:
- Do NOT change any source code - only update documentation files and doc comments.
- Do NOT create new documentation files unless the change clearly warrants it (e.g., a major new feature with no existing docs).
- Return JSON with "verdict" ("updated" or "skipped"), "summary" (brief description of what was updated and why), and optional "details".`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
	)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: documentVerdictSchema,
		OnChunk:    sctx.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}

	// Parse verdict
	var verdict documentVerdict
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &verdict); err != nil {
			sctx.Log("could not parse structured output, treating as skipped")
			verdict = documentVerdict{Verdict: "skipped", Summary: result.Text}
		}
	}

	// Commit any doc changes the agent made
	if verdict.Verdict == "updated" {
		if err := commitAgentFixes(sctx, s.Name(), verdict.Summary, "update documentation"); err != nil {
			return nil, err
		}
	}

	sctx.Log(fmt.Sprintf("document verdict: %s - %s", verdict.Verdict, verdict.Summary))

	return &pipeline.StepOutcome{}, nil
}

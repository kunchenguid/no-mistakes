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

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// In fix mode, ask the agent to apply documentation updates first
	if sctx.Fixing {
		fixPrompt := fmt.Sprintf(
			`Update any project documentation that needs to reflect the code changes.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s
- ignore patterns: %s

Task:
- Read the relevant diff and changed files yourself before editing.
- Update only the documentation directly affected by the change.
- Keep updates minimal and match the existing documentation style.

Rules:
- Only edit documentation files or doc comments.
- Do not change executable code or tests.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.

Previous documentation findings to address:
%s`,
			sctx.Run.Branch,
			baseSHA,
			sctx.Run.HeadSHA,
			sctx.Repo.DefaultBranch,
			ignorePatterns,
			sctx.PreviousFindings,
		)
		if err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			RequirePreviousFindings: true,
			MissingFindingsError:    "document fix requires previous findings",
			LogMessage:              "asking agent to update documentation...",
			Prompt:                  fixPrompt,
			ErrorPrefix:             "agent document fix",
			FallbackSummary:         "update documentation",
		}); err != nil {
			return nil, err
		}
	}

	// Check whether there are any changed files.
	var diffArgs []string
	if sctx.Fixing {
		diffArgs = []string{"diff", "--name-only", baseSHA}
	} else {
		diffArgs = []string{"diff", "--name-only", baseSHA + ".." + sctx.Run.HeadSHA}
	}
	changedFiles, err := git.Run(ctx, sctx.WorkDir, diffArgs...)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}

	// Assess documentation state
	sctx.Log("checking documentation...")

	prompt := fmt.Sprintf(
		`Review the code changes and identify any documentation gaps.

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

2. Identify documentation gaps
   - Look for existing documentation files in the project: README.md, docs/, doc comments, config examples, etc.
   - Determine which docs are affected by the change. Common cases:
     - New or changed public APIs - update API docs, doc comments, or usage examples
     - New features or behaviors - update README or relevant guide
     - Changed configuration - update config docs or examples
     - Removed functionality - remove or update stale references

3. Return findings
    - Return a finding for each specific documentation gap (file, description of what needs updating).
    - If no documentation updates are needed (e.g., internal refactoring, test-only changes, or documentation is already up to date), return an empty findings array.
    - Do a full documentation pass before returning. Do not stop after the first documentation gap. Continue checking the rest of the affected docs until you have enumerated all specific gaps you can substantiate.

Rules:
- Do NOT make any file changes in this mode.
- Only report gaps where documentation is missing or stale relative to the code change.
- Set action to "auto-fix" for all findings. Documentation gaps are objective.`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
	)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}

	var findings Findings
	if result.Output == nil {
		summary := fallbackDocumentSummary(result.Text)
		sctx.Log("missing structured output, requiring approval")
		findings = Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: summary,
				Action:      types.ActionAskUser,
			}},
			Summary: summary,
		}
	} else if err := unmarshalRequiredFindings(result.Output, &findings); err != nil {
		summary := fallbackDocumentSummary(extractDocumentSummary(result.Output, result.Text))
		sctx.Log("could not parse structured output, requiring approval")
		findings = Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: summary,
				Action:      types.ActionAskUser,
			}},
			Summary: summary,
		}
	}

	needsApproval := len(findings.Items) > 0
	findingsJSON, _ := json.Marshal(findings)
	autoFixable := len(types.AutoFixableFindings(findings).Items) > 0

	sctx.Log(fmt.Sprintf("document findings: %d items", len(findings.Items)))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   autoFixable,
		Findings:      string(findingsJSON),
	}, nil
}

func hasNonIgnoredDocumentChanges(changedFiles string, ignorePatterns []string) bool {
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			return true
		}
	}
	return false
}

func fallbackDocumentSummary(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return "agent returned no structured output"
	}
	return cleaned
}

func extractDocumentSummary(raw []byte, fallback string) string {
	var payload struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Summary) != "" {
		return payload.Summary
	}
	return fallback
}

func unmarshalRequiredFindings(raw []byte, findings *Findings) error {
	parsed, err := types.ParseFindingsJSON(string(raw))
	if err != nil {
		return err
	}
	var payload struct {
		Summary  *string            `json:"summary"`
		Findings *[]json.RawMessage `json:"findings"`
		Items    *[]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Findings == nil && payload.Items == nil {
		return fmt.Errorf("missing findings array")
	}
	if payload.Summary == nil || strings.TrimSpace(*payload.Summary) == "" {
		return fmt.Errorf("missing summary")
	}
	for i, item := range parsed.Items {
		if strings.TrimSpace(item.Severity) == "" {
			return fmt.Errorf("finding %d missing severity", i)
		}
		if strings.TrimSpace(item.Description) == "" {
			return fmt.Errorf("finding %d missing description", i)
		}
		if strings.TrimSpace(item.Action) == "" {
			return fmt.Errorf("finding %d missing action", i)
		}
	}
	*findings = parsed
	return nil
}

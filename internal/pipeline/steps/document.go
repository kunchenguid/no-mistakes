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

	// Skip entirely when nothing the agent would document has changed.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}

	sctx.Log("updating documentation...")

	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + stepInstructionsPromptSection(sctx)
	prompt := fmt.Sprintf(
		`Bring the project documentation fully in sync with the code changes. Discover every documentation gap, fix all of them yourself, verify your edits, and report only what you could not resolve.

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

2. Find every documentation gap
   - Look for existing documentation across the project: README.md, docs/, doc comments, config examples, etc.
   - Be exhaustive. Enumerate all docs affected by the change before you start editing. Common cases:
     - New or changed public APIs - update API docs, doc comments, or usage examples
     - New features or behaviors - update README or relevant guide
     - Changed configuration - update config docs or examples
     - Removed functionality - remove or update stale references
   - Do not stop after the first documentation gap. Keep scanning the rest of the affected docs until you have found every gap you can substantiate.

3. Fix all of them yourself
   - Update each affected documentation file or doc comment directly. Keep edits minimal and match the existing documentation style.
   - After editing, re-read the docs you changed to verify they now reflect the code.
   - This is a single pass with no follow-up round. Do not defer a known gap; resolve every gap you can in this run.

4. Report only what remains
   - Return a finding only for documentation gaps you could not resolve yourself, or that need a human judgment call (e.g. ambiguous intent or conflicting docs).
   - Do not report gaps you already fixed.
   - If you fixed everything and no documentation work remains, return an empty findings array.

Rules:
- Only edit documentation files or doc comments. Do not change executable behavior or tests.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous documentation findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}

	// Commit whatever documentation the agent edited, regardless of how
	// trustworthy its structured output turns out to be.
	commitSummary := extractDocumentSummary(result.Output, "")
	if err := commitAgentFixes(sctx, s.Name(), commitSummary, "update documentation"); err != nil {
		return nil, err
	}

	// Without trustworthy structured output we cannot confirm the agent
	// resolved every gap, so surface it for human review.
	var findings Findings
	if result.Output == nil {
		summary := fallbackDocumentSummary(result.Text)
		sctx.Log("missing structured output, requiring approval")
		return documentApprovalOutcome(summary), nil
	} else if err := unmarshalRequiredFindings(result.Output, &findings); err != nil {
		summary := fallbackDocumentSummary(extractDocumentSummary(result.Output, result.Text))
		sctx.Log("could not parse structured output, requiring approval")
		return documentApprovalOutcome(summary), nil
	}

	needsApproval := len(findings.Items) > 0
	findingsJSON, _ := json.Marshal(findings)

	sctx.Log(fmt.Sprintf("document findings: %d unresolved items", len(findings.Items)))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    findings.Summary,
	}, nil
}

// documentApprovalOutcome builds a single ask-user finding for cases where the
// agent's structured output is missing or unparsable, so a human can confirm
// the documentation state instead of silently trusting an opaque response.
func documentApprovalOutcome(summary string) *pipeline.StepOutcome {
	findings := Findings{
		Items: []Finding{{
			Severity:    "warning",
			Description: summary,
			Action:      types.ActionAskUser,
		}},
		Summary: summary,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    summary,
	}
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

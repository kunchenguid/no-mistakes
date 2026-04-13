package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
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

	sctx.Log("checking documentation...")

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	prompt := fmt.Sprintf(
		`Review the code changes and determine whether project documentation needs to be updated.

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

3. Decide whether documentation updates are needed
	- If the change requires doc updates, return verdict "updated" with a concise summary of what should change.
	- If no documentation updates are needed (e.g., internal refactoring, test-only changes), return verdict "skipped".

Rules:
- Do NOT make any file changes in this mode.
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
	var verdictErr error
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &verdict); err != nil {
			sctx.Log("could not parse structured output, treating as skipped")
			verdictErr = err
			verdict = documentVerdict{Verdict: "skipped", Summary: result.Text}
		}
	}

	if !sctx.Fixing && verdict.Verdict == "updated" {
		findings := Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: verdict.Summary,
			}},
			Summary: verdict.Summary,
		}
		findingsJSON, _ := json.Marshal(findings)
		sctx.Log(fmt.Sprintf("document verdict: %s - %s", verdict.Verdict, verdict.Summary))
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
		}, nil
	}

	if sctx.Fixing {
		outcome, err := s.applyDocumentationUpdates(sctx, baseSHA, verdictErr)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			return outcome, nil
		}
	}

	sctx.Log(fmt.Sprintf("document verdict: %s - %s", verdict.Verdict, verdict.Summary))

	return &pipeline.StepOutcome{}, nil
}

func (s *DocumentStep) applyDocumentationUpdates(sctx *pipeline.StepContext, baseSHA string, initialVerdictErr error) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	if initialVerdictErr != nil {
		outcome, err := malformedDocumentFixOutcome(ctx, sctx.WorkDir)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			return outcome, nil
		}
	}

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	prompt := fmt.Sprintf(
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
- Return JSON with "verdict" ("updated" or "skipped"), "summary", and optional "details" when finished.`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious documentation findings to address:\n" + sctx.PreviousFindings
	}

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: documentVerdictSchema,
		OnChunk:    sctx.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document fix: %w", err)
	}

	var verdict documentVerdict
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &verdict); err != nil {
			sctx.Log("could not parse structured output, treating as skipped")
			return malformedDocumentFixOutcome(ctx, sctx.WorkDir)
		}
	}

	if verdict.Verdict != "updated" {
		return nil, nil
	}

	findings, err := detectOutOfScopeDocumentEdits(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if len(findings.Items) > 0 {
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	if err := commitAgentFixes(sctx, s.Name(), verdict.Summary, "update documentation"); err != nil {
		return nil, err
	}
	return nil, nil
}

func detectOutOfScopeDocumentEdits(ctx context.Context, workDir string) (Findings, error) {
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return Findings{}, fmt.Errorf("list document edits: %w", err)
	}

	findings := Findings{Summary: "document step produced out-of-scope edits"}
	for _, line := range strings.Split(status, "\n") {
		path, untracked := parsePorcelainPath(line)
		if path == "" || isDocumentationPath(path) {
			continue
		}
		if untracked {
			findings.Items = append(findings.Items, Finding{
				Severity:    "warning",
				File:        path,
				Description: "document step modified non-documentation content",
			})
			continue
		}
		diff, err := git.Run(ctx, workDir, "diff", "HEAD", "--", path)
		if err != nil {
			return Findings{}, fmt.Errorf("diff document edit %s: %w", path, err)
		}
		if isDocCommentOnlyDiff(path, diff) {
			continue
		}
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			File:        path,
			Description: "document step modified non-documentation content",
		})
	}
	if len(findings.Items) == 0 {
		findings.Summary = ""
	}
	return findings, nil
}

func isDocumentationPath(path string) bool {
	clean := filepath.ToSlash(strings.TrimSpace(path))
	if clean == "" {
		return false
	}
	base := filepath.Base(clean)
	upperBase := strings.ToUpper(base)
	if strings.HasPrefix(clean, "docs/") {
		return true
	}
	if upperBase == "README" || strings.HasPrefix(upperBase, "README.") {
		return true
	}
	for _, name := range []string{"CHANGELOG", "CONTRIBUTING", "LICENSE"} {
		if upperBase == name || strings.HasPrefix(upperBase, name+".") {
			return true
		}
	}
	for _, ext := range []string{".md", ".mdx", ".rst", ".adoc"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			return true
		}
	}
	return false
}

func malformedDocumentFixOutcome(ctx context.Context, workDir string) (*pipeline.StepOutcome, error) {
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("check document fix status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil, nil
	}
	findings := Findings{
		Items: []Finding{{
			Severity:    "warning",
			Description: "document step changed files but returned malformed structured output",
		}},
		Summary: "document step changed files but returned malformed structured output",
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}, nil
}

func parsePorcelainPath(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	var path string
	switch {
	case strings.HasPrefix(line, "?? "):
		path = strings.TrimSpace(line[3:])
	case len(line) >= 4 && line[2] == ' ':
		path = strings.TrimSpace(line[3:])
	case len(line) >= 3 && line[1] == ' ':
		path = strings.TrimSpace(line[2:])
	default:
		return "", false
	}
	if path == "" {
		return "", false
	}
	if idx := strings.LastIndex(path, " -> "); idx >= 0 {
		path = path[idx+4:]
	}
	return path, strings.HasPrefix(line, "??")
}

func isDocCommentOnlyDiff(path, diff string) bool {
	if !strings.HasSuffix(strings.ToLower(path), ".go") {
		return false
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@") {
			continue
		}
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		text := strings.TrimSpace(line[1:])
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "//") || strings.HasPrefix(text, "/*") || strings.HasPrefix(text, "*") || strings.HasPrefix(text, "*/") {
			continue
		}
		return false
	}
	return true
}

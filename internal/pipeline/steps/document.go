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

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// In fix mode, ask the agent to apply documentation updates first
	if sctx.Fixing {
		if sctx.PreviousFindings == "" {
			return nil, fmt.Errorf("document fix requires previous findings")
		}
		sctx.Log("asking agent to update documentation...")
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
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt:     fixPrompt,
			CWD:        sctx.WorkDir,
			JSONSchema: commitSummarySchema,
			OnChunk:    sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent document fix: %w", err)
		}
		summary, err := extractCommitSummary(result)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
		}
		if err := ensureDocumentOnlyFixes(ctx, sctx.WorkDir); err != nil {
			return nil, err
		}
		if err := commitAgentFixes(sctx, s.Name(), summary, "update documentation"); err != nil {
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
	if result.Output == nil {
		verdict = documentVerdict{Verdict: "updated", Summary: fallbackDocumentSummary(result.Text)}
	} else {
		if err := json.Unmarshal(result.Output, &verdict); err != nil {
			sctx.Log("could not parse structured output, requiring approval")
			verdict = documentVerdict{Verdict: "updated", Summary: result.Text}
		}
	}

	needsApproval := verdict.Verdict == "updated"
	findings := Findings{}
	if needsApproval {
		findings = Findings{
			Items: []Finding{{
				Severity:    "warning",
				Description: verdict.Summary,
			}},
			Summary: verdict.Summary,
		}
	}

	findingsJSON, _ := json.Marshal(findings)
	sctx.Log(fmt.Sprintf("document verdict: %s - %s", verdict.Verdict, verdict.Summary))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
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

type gitStatusEntry struct {
	path      string
	untracked bool
	status    string
}

func ensureDocumentOnlyFixes(ctx context.Context, workDir string) error {
	status, err := git.Run(ctx, workDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("inspect document fixes: %w", err)
	}
	for _, entry := range parseGitStatus(status) {
		if isDocumentationPath(entry.path) {
			continue
		}
		if entry.untracked {
			return fmt.Errorf("document step produced non-document edits in %s", entry.path)
		}
		diff, err := git.Run(ctx, workDir, "diff", "--no-color", "--unified=0", "HEAD", "--", entry.path)
		if err != nil {
			return fmt.Errorf("inspect document diff for %s: %w", entry.path, err)
		}
		if !diffContainsOnlyCommentChanges(entry.path, diff) {
			return fmt.Errorf("document step produced non-document edits in %s", entry.path)
		}
	}
	return nil
}

func parseGitStatus(status string) []gitStatusEntry {
	var entries []gitStatusEntry
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		entries = append(entries, gitStatusEntry{
			path:      path,
			untracked: line[:2] == "??",
			status:    line[:2],
		})
	}
	return entries
}

func isDocumentationPath(path string) bool {
	clean := filepath.ToSlash(strings.TrimSpace(path))
	if clean == "" {
		return false
	}
	if strings.HasPrefix(clean, "docs/") || strings.HasPrefix(clean, "doc/") {
		return true
	}
	base := strings.ToLower(filepath.Base(clean))
	if strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "contributing") {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".md", ".mdx", ".rst", ".adoc":
		return true
	default:
		return false
	}
}

func diffContainsOnlyCommentChanges(path, diff string) bool {
	prefixes := commentPrefixes(filepath.Ext(path))
	if len(prefixes) == 0 {
		return false
	}
	for _, line := range strings.Split(diff, "\n") {
		if line == "" || strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "@@") || strings.HasPrefix(line, `\\`) {
			continue
		}
		if line[0] != '+' && line[0] != '-' {
			continue
		}
		trimmed := strings.TrimSpace(line[1:])
		if trimmed == "" {
			continue
		}
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func commentPrefixes(ext string) []string {
	switch strings.ToLower(ext) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".java", ".rs", ".c", ".cc", ".cpp", ".h", ".hpp":
		return []string{"//", "/*", "*", "*/"}
	case ".py", ".rb", ".sh", ".bash", ".zsh", ".yaml", ".yml":
		return []string{"#"}
	default:
		return nil
	}
}

func fallbackDocumentSummary(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return "agent returned no structured output"
	}
	return cleaned
}

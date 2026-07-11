package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DocumentStep keeps documentation accurate for the change under its
// placement policy, and - when no deterministic lint command is configured -
// also performs the agent-driven lint duty in the same invocation so the
// pipeline pays one cold agent pass for housekeeping instead of two.
type DocumentStep struct{}

func (s *DocumentStep) Name() types.StepName { return types.StepDocument }

// documentPlacementPolicy is the fail-safe default placement policy. It
// replaces the old exhaustive-synchronization incentive: the agent is
// rewarded for updating each fact's single owner and for consolidation,
// deletion, and pointers - not for synchronizing every prose copy. A trusted
// repository-specific policy (config document.instructions) may narrow or
// clarify these rules but never weaken them.
const documentPlacementPolicy = `Documentation placement policy (fail-safe defaults; repository-specific instructions may narrow or clarify them, never weaken them):
- Every fact or contract has exactly one authoritative owner document. Update the owner; never synchronize prose copies of the same fact.
- When this change leaves an existing duplicate stale, remove the duplicate or reduce it to a short pointer to the owner instead of updating another full copy.
- Do not create a new documentation surface merely to close a perceived gap.
- Do not add incident narratives or postmortems to AGENTS.md. For a durable incident lesson, preserve the operative invariant in its owner document and point to the regression test or authoritative implementation.
- AGENTS.md is only for high-value project-intrinsic knowledge useful to almost every future session.
- README.md owns the user-facing product introduction and common usage.
- CONTRIBUTING.md owns contribution mechanics, not product or architecture inventories.
- Code comments own non-obvious local intent, safety invariants, and external constraints - never prose that merely restates code.
- Deep reference docs own detailed conditional material; link to them instead of copying them into always-loaded guidance.
- Generated or schema-backed facts must be generated from their authoritative source and checked for drift, never hand-copied.`

// documentScopeDiscipline bounds the pass to documentation this change made
// stale, replacing the old "be exhaustive across the corpus" instruction.
const documentScopeDiscipline = `Scope discipline:
- Only touch documentation this change made stale, plus direct contradictions that analysis reveals.
- Do not opportunistically rewrite, expand, or restructure unrelated documentation, and do not perform a broad documentation architecture migration here.
- When a larger consolidation is warranted but out of scope, leave this change safe and report one finding proposing the follow-up instead of multiplying edits.
- Preserve load-bearing user guidance, security rationale, compatibility constraints, and onboarding material. A long document is not a defect by itself; duplication and wrong placement are.
- Prefer consolidation, deletion, and pointers to the owner over addition and synchronization.`

// housekeepingLintSection adds the agent-driven lint duty to the combined
// document+lint pass.
const housekeepingLintSection = `

Combined lint duty (same pass - no separate lint agent will run):
- Discover the configured linters and formatters for this repository.
- Run the relevant checks, preferring only the changed files when possible.
- Apply safe formatter, linter, and static-analysis fixes yourself, then re-run the relevant checks.
- Do not run tests or broader behavioral validation.
- Report only unresolved lint, format, or static-analysis issues as findings with "category" set to "lint". Do not report lint issues you already fixed.

Set "category" on every finding: "documentation" for documentation findings, "lint" for lint findings.`

// housekeepingFindingsSchema extends findingsSchema with the per-finding
// category that routes combined-pass findings to their owning gates.
var housekeepingFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"category": {"type": "string", "enum": ["documentation", "lint"]}
				},
				"required": ["severity", "description", "action", "category"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// Combine the agent-driven lint duty into this pass when no deterministic
	// lint command is configured; the lint step then consumes the result
	// instead of paying its own cold agent invocation.
	combinedLint := sctx.Config.Commands.Lint == ""
	if combinedLint {
		sctx.Shared.ClearHousekeepingLint()
	}

	// Skip entirely when nothing the agent would document has changed. No
	// lint result is stashed, so the lint step falls back to its own pass -
	// neither duty is ever silently skipped.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}

	if combinedLint {
		sctx.Log("housekeeping: updating documentation and linting in one pass...")
	} else {
		sctx.Log("updating documentation...")
	}

	prompt := s.buildPrompt(sctx, baseSHA, ignorePatterns, combinedLint)
	schema := findingsSchema
	purpose := "document"
	if combinedLint {
		schema = housekeepingFindingsSchema
		purpose = "housekeeping"
	}

	authorHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve documentation author HEAD: %w", err)
	}

	result, err := sctx.InvokeAgent(types.PurposeDocumentationAuthoring, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: schema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}
	headAfterAuthor, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD after documentation author: %w", err)
	}
	if headAfterAuthor != authorHead {
		return nil, fmt.Errorf("documentation author changed HEAD before independent verification")
	}

	if result == nil {
		return nil, fmt.Errorf("documentation author returned no result")
	}
	authorFindings, err := validateFindingsOutput(result.Output)
	if err != nil {
		return nil, fmt.Errorf("documentation author output inconclusive: %w", err)
	}
	if combinedLint {
		_, lintFindings := splitHousekeepingFindings(authorFindings)
		lintJSON, err := types.MarshalFindingsJSON(lintFindings)
		if err != nil {
			return nil, fmt.Errorf("marshal housekeeping lint findings: %w", err)
		}
		sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{
			FindingsJSON: lintJSON,
			Summary:      authorFindings.Summary,
		})
		sctx.Log(fmt.Sprintf("housekeeping lint result recorded for the lint step: %d unresolved items", len(lintFindings.Items)))
	}

	// Stage the author's complete candidate so deterministic checks and the
	// verifier inspect tracked and newly created documentation alike.
	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage documentation candidate: %w", err)
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "diff", "--cached", "--check"); err != nil {
		return nil, fmt.Errorf("documentation deterministic checks failed: %w", err)
	}
	beforeVerification, err := captureDocumentationCandidate(sctx)
	if err != nil {
		return nil, err
	}

	verifyResult, err := sctx.InvokeAgent(types.PurposeDocumentationVerification, agent.RunOpts{
		Prompt: fmt.Sprintf(
			`Independently verify the staged documentation candidate against the code and the complete branch diff.

Base commit: %s
Candidate HEAD before the staged documentation patch: %s

Rules:
- Do not modify, stage, or commit any files.
- Check documentation accuracy, completeness, examples, configuration, public APIs, and removal of stale claims.
- Treat anything inconclusive or not verifiable as a warning or error.
- Return an empty findings array only when the staged documentation candidate is accurate and complete.`,
			baseSHA,
			beforeVerification.head,
		) + executionContextPromptSection() + userIntentPromptSection(sctx),
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("documentation verification: %w", err)
	}
	afterVerification, err := captureDocumentationCandidate(sctx)
	if err != nil {
		return nil, err
	}
	if afterVerification != beforeVerification {
		return nil, fmt.Errorf("documentation verifier mutated the candidate")
	}
	if verifyResult == nil {
		return nil, fmt.Errorf("documentation verifier returned no result")
	}
	findings, err := validateFindingsOutput(verifyResult.Output)
	if err != nil {
		return nil, fmt.Errorf("documentation verification inconclusive: %w", err)
	}

	needsApproval := hasBlockingFindings(findings.Items)
	if !needsApproval {
		if err := commitAgentFixes(sctx, s.Name(), authorFindings.Summary, "update documentation"); err != nil {
			return nil, err
		}
	}
	findingsJSON, _ := json.Marshal(findings)
	sctx.Log(fmt.Sprintf("document verification findings: %d unresolved items", len(findings.Items)))

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    authorFindings.Summary,
		// Deterministic documentation integrity check the coordinator runs before
		// the fresh tools_balanced verifier: reject conflict markers and
		// whitespace corruption in the changed content.
		RepairChecks: []pipeline.RepairCheck{{
			Command: "git diff --check",
			Run: func(ctx context.Context) (bool, int, string) {
				out, cerr := git.Run(ctx, sctx.WorkDir, "diff", "--check", baseSHA, "HEAD")
				if cerr != nil {
					return true, 1, out
				}
				return true, 0, out
			},
		}},
	}, nil
}

// buildPrompt assembles the document (or combined document+lint) prompt: the
// placement policy, scope discipline, trusted repository-specific policy,
// the task, and - in combined mode - the lint duty.
func (s *DocumentStep) buildPrompt(sctx *pipeline.StepContext, baseSHA, ignorePatterns string, combinedLint bool) string {
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)

	intro := "Keep the project documentation accurate for this change."
	if combinedLint {
		intro = "Perform the combined documentation and lint housekeeping pass for this change."
	}

	editRule := "- Only edit documentation files or doc comments. Do not change executable behavior or tests."
	if combinedLint {
		editRule = "- Documentation edits must only touch documentation files or doc comments. Lint fixes must be safe, mechanical, and behavior-preserving. Never change functional behavior or tests."
	}

	prompt := fmt.Sprintf(
		`%s Analyze what the change made stale, fix each stale fact in its one authoritative location, and report only what you could not resolve.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s
- ignore patterns: %s

%s

%s%s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed, and the intent of the change.

2. Find what this change made stale
   - For each fact or contract the change altered, locate its one authoritative owner document (README, docs/, doc comments, config examples, etc.).
   - Locate existing duplicates of those facts that are now stale.

3. Fix in the authoritative location
   - Update each altered fact in its owner document. Changed user-facing behavior must leave its authoritative user documentation accurate.
   - Remove stale duplicates or reduce them to a short pointer to the owner; do not synchronize full copies.
   - Re-read what you changed to verify it now reflects the code.

4. Report only what remains
   - Return a finding only for gaps you could not resolve, judgment calls (e.g. ambiguous intent or conflicting docs), or an out-of-scope consolidation worth a follow-up.
   - Do not report gaps you already fixed.
   - If nothing remains, return an empty findings array.%s

Rules:
%s
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		intro,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		documentPlacementPolicy,
		documentScopeDiscipline,
		trustedDocumentPolicySection(sctx),
		lintDutySection(combinedLint),
		editRule,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return prompt
}

// trustedDocumentPolicySection renders the repository-specific documentation
// ownership policy. The value comes from the trusted default-branch copy of
// .no-mistakes.yaml (config.EffectiveRepoConfig), so a contributor's pushed
// branch cannot weaken the rules that gate its own review.
func trustedDocumentPolicySection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Document.Instructions)
	if instructions == "" {
		return ""
	}
	return "\n\nRepository documentation ownership policy (trusted, from the default branch; augments the defaults above and cannot weaken them):\n" +
		sanitizePromptMultilineText(instructions)
}

func lintDutySection(combinedLint bool) string {
	if !combinedLint {
		return ""
	}
	return housekeepingLintSection
}

// splitHousekeepingFindings routes combined-pass findings to their owning
// gates. An uncategorized finding counts as documentation - the stricter
// gate (any documentation finding parks; lint parks only on error/warning) -
// so miscategorization fails safe.
func splitHousekeepingFindings(findings Findings) (doc Findings, lint Findings) {
	doc = Findings{Summary: findings.Summary}
	lint = Findings{Summary: findings.Summary}
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryLint {
			lint.Items = append(lint.Items, item)
			continue
		}
		doc.Items = append(doc.Items, item)
	}
	return doc, lint
}

type documentationCandidate struct {
	head   string
	status string
	diff   string
}

func captureDocumentationCandidate(sctx *pipeline.StepContext) (documentationCandidate, error) {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return documentationCandidate{}, fmt.Errorf("resolve documentation candidate HEAD: %w", err)
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return documentationCandidate{}, fmt.Errorf("capture documentation candidate status: %w", err)
	}
	diff, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--binary", "HEAD", "--")
	if err != nil {
		return documentationCandidate{}, fmt.Errorf("capture documentation candidate diff: %w", err)
	}
	return documentationCandidate{head: head, status: status, diff: diff}, nil
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

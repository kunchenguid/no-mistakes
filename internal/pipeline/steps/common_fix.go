package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/conventional"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var commitSummarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string"}
	},
	"required": ["summary"]
}`)

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	ctx := sctx.Ctx
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
		return fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	if summary == "" {
		summary = fallbackSummary
	}
	commitMessage := deterministicFixCommitMessage(sctx, stepName, summary)
	if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", commitMessage); err != nil {
		return fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	return nil
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func deterministicFixCommitMessage(sctx *pipeline.StepContext, stepName types.StepName, summary string) string {
	if summary == "" {
		summary = "apply fixes"
	}
	base := fmt.Sprintf("no-mistakes(%s): %s", stepName, summary)
	return prependTicket(base, fixCommitTicket(sctx))
}

// fixedFixCommitMessage builds the subject for the non-step "apply fixes"
// commits (push, CI) so they also lead with the work-item id when configured.
func fixedFixCommitMessage(sctx *pipeline.StepContext, text string) string {
	return fixedFixCommitMessageWithPRTitle(sctx, text, "")
}

// fixedFixCommitMessageWithPRTitle is fixedFixCommitMessage with a PR-title
// candidate, used by the CI step where a PR already exists and its title may
// carry the work-item id when the branch and commits do not.
func fixedFixCommitMessageWithPRTitle(sctx *pipeline.StepContext, text, prTitle string) string {
	base := fmt.Sprintf("no-mistakes: %s", text)
	return prependTicket(base, resolveTicket(sctx, prTitle))
}

// prependTicket prepends "<ticket>: " to subject unless ticket is empty or
// subject already leads with it.
func prependTicket(subject, ticket string) string {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return subject
	}
	if subject == ticket || strings.HasPrefix(subject, ticket+":") {
		return subject
	}
	return ticket + ": " + subject
}

// fixCommitTicket resolves the work-item id for a gate-authored commit subject
// from the branch and author commits (no PR title; the non-CI fix paths run
// before a PR exists). Returns "" to keep the default
// "no-mistakes(<step>): ..." subject.
func fixCommitTicket(sctx *pipeline.StepContext) string {
	return resolveTicket(sctx, "")
}

// resolveTicket resolves the work-item id by matching ticket_prefix_pattern
// against a fixed precedence: the branch name, then the PR title (when one is
// supplied), then the first author commit subject on the branch (oldest first)
// that carries a match. Returns "" when the pattern is empty or nothing
// matches.
func resolveTicket(sctx *pipeline.StepContext, prTitle string) string {
	if sctx == nil || sctx.Config == nil || sctx.Run == nil {
		return ""
	}
	pattern := strings.TrimSpace(sctx.Config.TicketPrefixPattern)
	if pattern == "" {
		return ""
	}
	if id := conventional.ExtractTicket(sctx.Run.Branch, pattern); id != "" {
		return id
	}
	if id := conventional.ExtractTicket(prTitle, pattern); id != "" {
		return id
	}
	return firstAuthorCommitTicket(sctx, pattern)
}

// firstAuthorCommitTicket scans the branch's commit subjects oldest-first and
// returns the first match for pattern on a subject not authored by the gate.
// Best-effort: a git read failure yields "".
func firstAuthorCommitTicket(sctx *pipeline.StepContext, pattern string) string {
	if sctx.Ctx == nil || sctx.Repo == nil || strings.TrimSpace(sctx.WorkDir) == "" {
		return ""
	}
	base := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.IntegrationBranch())
	out, err := git.Run(sctx.Ctx, sctx.WorkDir, "log", "--format=%s", "--reverse", base+".."+sctx.Run.HeadSHA)
	if err != nil {
		return ""
	}
	for _, subject := range strings.Split(out, "\n") {
		subject = strings.TrimSpace(subject)
		if subject == "" || isGateAuthoredSubject(subject) {
			continue
		}
		if id := conventional.ExtractTicket(subject, pattern); id != "" {
			return id
		}
	}
	return ""
}

// isGateAuthoredSubject reports whether subject was written by the gate, i.e.
// it begins with "no-mistakes" or with "<id>: no-mistakes".
func isGateAuthoredSubject(subject string) bool {
	subject = strings.TrimSpace(subject)
	if strings.HasPrefix(subject, "no-mistakes") {
		return true
	}
	if _, rest, ok := strings.Cut(subject, ": "); ok && strings.HasPrefix(strings.TrimSpace(rest), "no-mistakes") {
		return true
	}
	return false
}

// executeFixMode runs the fix agent and commits any resulting changes. It
// returns the agent's one-line fix summary (empty when the agent returned
// nothing parseable), which the caller should place on StepOutcome.FixSummary
// so the executor can persist it on the round record.
func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     opts.Prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if err := commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary); err != nil {
		return "", err
	}
	return summary, nil
}

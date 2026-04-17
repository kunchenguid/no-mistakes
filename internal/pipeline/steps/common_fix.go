package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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
	commitMessage := deterministicFixCommitMessage(stepName, summary)
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

func deterministicFixCommitMessage(stepName types.StepName, summary string) string {
	if summary == "" {
		summary = "apply fixes"
	}
	return fmt.Sprintf("no-mistakes(%s): %s", stepName, summary)
}

func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) error {
	if !sctx.Fixing {
		return nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return errors.New(opts.MissingFindingsError)
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
		return fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	return commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary)
}

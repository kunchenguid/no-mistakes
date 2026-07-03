package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DefaultCommandTimeout bounds a custom command step that does not set its own
// timeout, so an unbounded command (e.g. a hung `xcodebuild`) cannot wedge the
// gate forever.
const DefaultCommandTimeout = 30 * time.Minute

// CommandStep runs a repo-defined shell command as a pipeline step. Unlike the
// built-in steps it carries its identity and configuration as fields, since a
// repo may define several. The command string and every option originate from
// the trusted default-branch config (see the daemon's EffectiveRepoConfig), so
// a pushed branch cannot inject the command that runs here.
type CommandStep struct {
	StepName     types.StepName
	Command      string
	FindingsPath string        // optional worktree-relative path the command writes findings JSON to
	Timeout      time.Duration // 0 ⇒ DefaultCommandTimeout
	AutoFix      bool
}

func (s *CommandStep) Name() types.StepName { return s.StepName }

// Execute runs the configured command, turns its result into findings, and
// gates on failure. In fix mode (auto-fix loop or a user "fix" action) it first
// asks the agent to resolve the previously-reported findings, then re-runs the
// command — mirroring the built-in lint/test fix loop.
func (s *CommandStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	var fixSummary string
	if sctx.Fixing {
		summary, err := s.runFix(sctx)
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	defaultAction := types.ActionAskUser
	if s.AutoFix {
		defaultAction = types.ActionAutoFix
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}

	// Remove a stale findings file so a previous round's output is never parsed
	// as this round's result.
	findingsAbs := ""
	if s.FindingsPath != "" {
		findingsAbs = s.FindingsPath
		if !filepath.IsAbs(findingsAbs) {
			findingsAbs = filepath.Join(sctx.WorkDir, findingsAbs)
		}
		_ = os.Remove(findingsAbs)
	}

	ctx, cancel := context.WithTimeout(sctx.Ctx, timeout)
	defer cancel()

	sctx.Log(fmt.Sprintf("running %s: %s", s.StepName, s.Command))
	output, exitCode, runErr := runShellCommandWithEnv(ctx, sctx.WorkDir, sctx.Env, s.Command)

	// A cancelled run (superseded push, user abort) must fail the run — never be
	// mistaken for a per-step timeout, whose derived deadline is separate.
	if sctx.Ctx.Err() != nil {
		return nil, sctx.Ctx.Err()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		sctx.Log(output)
		return s.timeoutOutcome(timeout, fixSummary), nil
	}
	if runErr != nil {
		return nil, fmt.Errorf("run %s command: %w", s.StepName, runErr)
	}

	sctx.Log(output)

	findings, err := s.collectFindings(findingsAbs, output, exitCode, defaultAction)
	if err != nil {
		return nil, err
	}

	if exitCode == 0 && !hasBlockingFindings(findings.Items) {
		sctx.Log(fmt.Sprintf("%s passed", s.StepName))
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
	}

	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   s.AutoFix,
		Findings:      string(findingsJSON),
		ExitCode:      exitCode,
		FixSummary:    fixSummary,
	}, nil
}

// collectFindings builds the findings for a completed run. When a findings_json
// path is configured and present, it is parsed into real per-line findings;
// otherwise the step falls back to exit-code mapping (a single synthetic
// finding), matching the built-in lint/test steps.
func (s *CommandStep) collectFindings(findingsAbs, output string, exitCode int, defaultAction string) (Findings, error) {
	if findingsAbs != "" {
		data, err := os.ReadFile(findingsAbs)
		switch {
		case err == nil:
			findings, perr := parseCommandFindings(data)
			if perr != nil {
				return Findings{}, fmt.Errorf("parse findings_json from %s: %w", s.FindingsPath, perr)
			}
			for i := range findings.Items {
				if findings.Items[i].Action == "" {
					findings.Items[i].Action = defaultAction
				}
			}
			if findings.Summary == "" {
				findings.Summary = fmt.Sprintf("%s reported %d finding(s)", s.StepName, len(findings.Items))
			}
			return findings, nil
		case errors.Is(err, os.ErrNotExist):
			// Absent file: the command may not write it on success. Fall through
			// to exit-code mapping.
		default:
			return Findings{}, fmt.Errorf("read findings_json from %s: %w", s.FindingsPath, err)
		}
	}

	if exitCode != 0 {
		return Findings{
			Items: []Finding{{
				Severity:    "error",
				Description: fmt.Sprintf("%s failed with exit code %d", s.StepName, exitCode),
				Action:      defaultAction,
			}},
			Summary: firstNonEmptyLine(output, fmt.Sprintf("%s failed", s.StepName)),
		}, nil
	}
	return Findings{}, nil
}

// timeoutOutcome parks the step with a clear, non-auto-fixable timeout finding.
// Timeouts are surfaced for a human/agent decision rather than auto-fixed,
// since a hung command is rarely resolved by another code edit.
func (s *CommandStep) timeoutOutcome(timeout time.Duration, fixSummary string) *pipeline.StepOutcome {
	msg := fmt.Sprintf("%s timed out after %s", s.StepName, timeout)
	findings := Findings{
		Items: []Finding{{
			Severity:    "error",
			Description: msg,
			Action:      types.ActionAskUser,
		}},
		Summary: msg,
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		ExitCode:      -1,
		FixSummary:    fixSummary,
	}
}

// runFix asks the agent to resolve the previously-reported findings, then
// commits any changes. The command itself re-runs after this returns.
func (s *CommandStep) runFix(sctx *pipeline.StepContext) (string, error) {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + stepInstructionsPromptSection(sctx)
	prompt := fmt.Sprintf(
		`Fix the issues reported by the %q check in this repository. Run the check, identify the issues, and fix them.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- check command: %s

Rules:
- Make the smallest correct root-cause fix.
- Do not refactor beyond what is needed for that root-cause fix.
- Re-run the check command before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		string(s.StepName),
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		s.Command,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings to address:\n" + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      fmt.Sprintf("asking agent to fix %s issues...", s.StepName),
		Prompt:          prompt,
		ErrorPrefix:     fmt.Sprintf("agent fix %s", s.StepName),
		FallbackSummary: fmt.Sprintf("fix %s issues", s.StepName),
	})
}

// parseCommandFindings decodes a command's findings JSON, accepting either the
// full findings object ({"findings": [...], "summary": ...}) or a bare array of
// finding objects.
func parseCommandFindings(data []byte) (Findings, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Findings{}, nil
	}
	if trimmed[0] == '[' {
		var items []Finding
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return Findings{}, err
		}
		return Findings{Items: items}, nil
	}
	return types.ParseFindingsJSON(string(trimmed))
}

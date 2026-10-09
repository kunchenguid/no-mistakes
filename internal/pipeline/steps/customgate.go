package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A GetRun response carrying the maximum 16 configured gates uses well under
// 16 KiB outside findings_json. Keep four times that measured envelope so new
// run or step fields cannot make an admitted gate refusal exceed the scanner.
const gateGetRunEnvelopeReserveBytes = 64 << 10

// CustomGateStep runs one repository-declared extra check immediately after
// its anchor core step. It can only add a verdict to a run: the executor places
// it after the anchor and no core step consults it. A failed check parks for an
// operator decision, so the gate cannot weaken what the core steps decided.
type CustomGateStep struct {
	Gate config.Gate
}

func (s *CustomGateStep) Name() types.StepName { return s.Gate.StepName() }

func (s *CustomGateStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	fixSummary, err := s.runFixTurn(sctx)
	if err != nil {
		return nil, err
	}
	return s.executeCommand(sctx, fixSummary)
}

// runFixTurn repairs the worktree when the gate's park was answered with `fix`,
// and returns the agent's commit summary. The caller then re-runs the gate's
// own check, so a re-parked verdict describes the repaired worktree rather than
// the unchanged one that produced the previous findings.
//
// This is the same fix protocol Test and Lint use, and a gate needs it for the
// same reason: without it, answering `fix` costs a full extra execution that
// provably cannot change the verdict. The gate's identity carries the intent -
// the command that must exit 0, because a bare finding does not say what
// passing would mean.
func (s *CustomGateStep) runFixTurn(sctx *pipeline.StepContext) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	baseBranch := effectivePRBaseBranch(sctx)
	baseSHA, err := resolveBranchBaseSHA(sctx.Ctx, sctx, sctx.Run.BaseSHA, baseBranch)
	if err != nil {
		return "", err
	}

	requirement := fmt.Sprintf("This gate passes only when the following command exits 0 and reports no error findings through NO_MISTAKES_FINDINGS_FILE:\n%s", strings.TrimSpace(s.Gate.Command))

	prompt := fmt.Sprintf(
		`Fix the violations reported by the repository gate %q.

Context:
- branch: %s
- base commit: %s
- target commit: %s

%s

Rules:
- Make the smallest correct root-cause fix that satisfies the gate.
- Do not refactor beyond what is needed for that root-cause fix.
- Do not weaken, disable, or narrow the gate itself to make it pass.
- Re-run or re-check the gate's own requirement above before finishing, and nothing broader.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		s.Gate.Name,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		requirement,
		executionContextPromptSection(sctx.WorkDir)+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx),
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous gate findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}

	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      fmt.Sprintf("asking agent to satisfy gate %q...", s.Gate.Name),
		Prompt:          prompt,
		ErrorPrefix:     fmt.Sprintf("agent fix gate %q", s.Gate.Name),
		FallbackSummary: fmt.Sprintf("satisfy %s gate", s.Gate.Name),
	})
}

func (s *CustomGateStep) executeCommand(sctx *pipeline.StepContext, fixSummary string) (*pipeline.StepOutcome, error) {
	command := strings.TrimSpace(s.Gate.Command)
	workDir, err := filepath.Abs(sctx.WorkDir)
	if err == nil {
		workDir, err = filepath.EvalSymlinks(workDir)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve gate worktree: %w", err)
	}
	// A sibling directory stays outside the worktree even when TMPDIR points inside it.
	dir, err := os.MkdirTemp(filepath.Dir(workDir), ".no-mistakes-gate-")
	if err != nil {
		return nil, fmt.Errorf("create gate findings directory: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "findings.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return nil, fmt.Errorf("create gate findings file: %w", err)
	}
	commandContext := *sctx
	commandContext.Env = append(append([]string(nil), sctx.Env...), "NO_MISTAKES_FINDINGS_FILE="+path)
	sctx.Log(fmt.Sprintf("running gate %q: %s", s.Gate.Name, command))
	output, exitCode, err := runStepShellCommand(&commandContext, command)
	if err != nil {
		logConfiguredCommandOutput(sctx, output, s.Name())
		return nil, fmt.Errorf("run gate %q command: %w", s.Gate.Name, err)
	}
	items, reported, reportErr := readGateFindings(path)
	if reportErr == nil && !reported && exitCode == 0 {
		return &pipeline.StepOutcome{FixSummary: fixSummary}, nil
	}
	needsApproval := exitCode != 0
	if reportErr != nil {
		needsApproval = true
		items = []Finding{{
			Severity:    types.FindingSeverityError,
			Description: fmt.Sprintf("gate %q findings file invalid: %v", s.Gate.Name, reportErr),
			Action:      types.ActionAskUser,
		}}
	} else if !reported {
		items = []Finding{{
			Severity:    types.FindingSeverityError,
			Description: fmt.Sprintf("gate %q failed with exit code %d", s.Gate.Name, exitCode),
			Action:      types.ActionAskUser,
		}}
	}
	for _, item := range items {
		if item.Severity == types.FindingSeverityError {
			needsApproval = true
		}
	}
	findings := Findings{
		Items:   items,
		Summary: logConfiguredCommandOutput(sctx, output, s.Name()),
		Tested:  []string{command},
	}
	findingsJSON, refused, err := fitGateFindingsTransport(sctx, findings)
	if err != nil {
		return nil, fmt.Errorf("fit gate findings transport: %w", err)
	}
	if refused {
		needsApproval = true
	}
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		// A gate must never repair on the pipeline's own initiative: it states a
		// repository rule, so deciding that the change should be altered to
		// satisfy it is the author's call, not the pipeline's. Answering the park
		// with `fix` IS that authorization, and runFixTurn services it - being
		// non-auto-fixable is about who decides, not about whether a repair is
		// possible.
		AutoFixable: false,
		Findings:    findingsJSON,
		ExitCode:    exitCode,
		FixSummary:  fixSummary,
	}, nil
}

func fitGateFindingsTransport(sctx *pipeline.StepContext, findings Findings) (string, bool, error) {
	remaining, err := remainingGateFindingsTransportBytes(sctx)
	if err != nil {
		return "", false, err
	}
	// Normal reports keep half the frame for the run envelope. A bounded
	// refusal can use the remaining frame capacity when earlier steps have
	// already consumed that report budget.
	limit := remaining - ipc.MaxFrameBytes/2 - configuredGateCount(sctx)*gateTransportRefusalReserveBytes()
	if limit < 0 {
		limit = 0
	}
	findingsJSON, encodedSize := encodeGateFindings(findings)
	if encodedSize <= limit {
		return findingsJSON, false, nil
	}
	refusalJSON, refusalSize := encodeGateFindings(gateTransportRefusal(encodedSize, limit))
	if refusalSize > remaining {
		return "", false, fmt.Errorf("transport refusal needs %d bytes, only %d bytes remain", refusalSize, remaining)
	}
	return refusalJSON, true, nil
}

func gateTransportRefusal(encodedSize, limit int) Findings {
	return Findings{Items: []Finding{{
		Severity:    types.FindingSeverityError,
		Description: fmt.Sprintf("gate findings payload is too large to transport: encoded size %d bytes, limit %d bytes", encodedSize, limit),
		Action:      types.ActionAskUser,
	}}}
}

func gateTransportRefusalReserveBytes() int {
	maxInt := int(^uint(0) >> 1)
	_, size := encodeGateFindings(gateTransportRefusal(maxInt, maxInt))
	return size
}

func configuredGateCount(sctx *pipeline.StepContext) int {
	if sctx.Config != nil && len(sctx.Config.Gates) > 0 {
		return len(sctx.Config.Gates)
	}
	return 1
}

func encodeGateFindings(findings Findings) (string, int) {
	raw, _ := json.Marshal(findings)
	encoded, _ := json.Marshal(string(raw))
	return string(raw), len(encoded)
}

func remainingGateFindingsTransportBytes(sctx *pipeline.StepContext) (int, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return 0, err
	}
	remaining := ipc.MaxFrameBytes - gateGetRunEnvelopeReserveBytes
	for _, step := range steps {
		if step.ID == sctx.StepResultID || step.FindingsJSON == nil {
			continue
		}
		encoded, _ := json.Marshal(*step.FindingsJSON)
		remaining -= len(encoded)
		if remaining <= 0 {
			return 0, nil
		}
	}
	return remaining, nil
}

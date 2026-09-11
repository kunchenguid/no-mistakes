package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// analyzerCorrectionMaxAttempts is the number of analyzer invocations
// allowed for one Test or Review Execute, including the first. An invalid
// findings payload is not a product defect: it is returned to a fresh
// agent with the validation errors so the caller can correct and resubmit.
// Only exhausting this bound is a genuine blocking failure. The bound is
// independent of auto_fix.*, which is for repairing the product rather
// than correcting structured output.
const analyzerCorrectionMaxAttempts = 3

type analyzerCorrection struct {
	schema            json.RawMessage
	purpose           string
	correctionPurpose string
	env               []string
	workload          *agent.InvocationWorkload
	logName           string
	exhaustedOp       string
	startContext      func() (context.Context, context.CancelFunc, time.Duration)
	wrapError         func(ctx context.Context, timeout time.Duration, err error) error
	run               func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	parse             func(*agent.Result) (Findings, error)
	correctionPrompt  func(err error, rejected []byte) string
}

func runAnalyzerWithCorrection(sctx *pipeline.StepContext, prompt string, cfg analyzerCorrection) (Findings, error) {
	current := prompt
	var lastErr error
	for attempt := 1; attempt <= analyzerCorrectionMaxAttempts; attempt++ {
		if attempt > 1 {
			sctx.Log(fmt.Sprintf(
				"%s rejected (%s); asking agent to correct and resubmit (attempt %d of %d)",
				cfg.logName,
				strings.ReplaceAll(lastErr.Error(), "\n", "; "),
				attempt,
				analyzerCorrectionMaxAttempts,
			))
		}
		ctx, cancel, timeout := cfg.startContext()
		purpose := cfg.purpose
		if attempt > 1 && cfg.correctionPurpose != "" {
			purpose = cfg.correctionPurpose
		}
		result, err := cfg.run(ctx, agent.RunOpts{
			Prompt:     current,
			CWD:        sctx.WorkDir,
			Env:        cfg.env,
			JSONSchema: cfg.schema,
			OnChunk:    sctx.LogChunk,
			Purpose:    purpose,
			Workload:   cfg.workload,
		})
		runErr := cfg.wrapError(ctx, timeout, err)
		if runErr != nil && (context.Cause(ctx) != nil || !agent.IsStructuredOutputRejected(runErr)) {
			cancel()
			return Findings{}, runErr
		}
		cancel()

		var valErr error
		if runErr != nil {
			// Adapters that enforce JSON schemas may reject the response in their
			// finalizer and therefore have no Result to parse. That is still bad
			// analyzer input, not an unrecoverable step failure.
			valErr = runErr
		} else {
			var findings Findings
			findings, valErr = cfg.parse(result)
			if valErr == nil {
				return findings, nil
			}
		}
		lastErr = valErr
		if attempt == analyzerCorrectionMaxAttempts {
			break
		}
		var rejected []byte
		if result != nil {
			rejected = result.Output
		}
		current = cfg.correctionPrompt(valErr, rejected)
	}
	return Findings{}, fmt.Errorf("%s after %d attempts: %w", cfg.exhaustedOp, analyzerCorrectionMaxAttempts, lastErr)
}

func analyzerRejectedPayloadSection(err error, rejected []byte) string {
	var b strings.Builder
	b.WriteString("Validation errors:\n")
	b.WriteString(sanitizePromptMultilineText(err.Error()))
	if len(rejected) > 0 {
		b.WriteString("\n\nRejected payload:\n<rejected-json>\n")
		b.WriteString(sanitizePromptMultilineText(string(rejected)))
		b.WriteString("\n</rejected-json>")
	}
	b.WriteString("\n")
	return b.String()
}

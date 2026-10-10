package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type measuredAgent struct {
	agent.Agent
	Attempts        []map[string]any
	AdapterAttempts []map[string]any
}

func attemptRecord(started, completed time.Time, result *agent.Result, err error) map[string]any {
	row := map[string]any{
		"at": started.UTC().Format(time.RFC3339), "wall_ms": completed.Sub(started).Milliseconds(),
		"schema_valid": err == nil, "schema_rejected": agent.IsStructuredOutputRejected(err), "schema_field": nil,
	}
	if err != nil {
		row["error"] = err.Error()
		var violation *agent.SchemaViolation
		if errors.As(err, &violation) {
			row["schema_field"] = violation.Field
		}
	}
	if result != nil {
		row["model"] = result.Model
		row["provider"] = result.ModelProvider
		if result.UsageReported {
			row["usage"] = result.Usage
		}
		if len(result.Output) > 0 {
			row["output"] = result.Output
		}
	}
	return row
}

func (a *measuredAgent) ReportsAgentAttempts() bool { return agent.ReportsAgentAttempts(a.Agent) }

func (a *measuredAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	started := time.Now()
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt agent.Attempt) {
		row := attemptRecord(attempt.StartedAt, attempt.CompletedAt, attempt.Result, attempt.Err)
		row["purpose"] = opts.Purpose
		a.AdapterAttempts = append(a.AdapterAttempts, row)
		if previous != nil {
			previous(attempt)
		}
	}
	result, err := a.Agent.Run(ctx, opts)
	row := attemptRecord(started, time.Now(), result, err)
	row["purpose"] = opts.Purpose
	a.Attempts = append(a.Attempts, row)
	if err == nil {
		if result == nil {
			err = errors.New("Pi returned no result")
		} else if result.ModelProvider != "openai-codex" || result.Model != "gpt-6.1-sol" {
			err = fmt.Errorf("unexpected serving model: %s/%s", result.ModelProvider, result.Model)
		}
	}
	return result, err
}

func main() {
	cwd := flag.String("cwd", ".", "repository whose change is reviewed")
	base := flag.String("base", "728ffe0f226527a77358bb265be6073c0786367e", "review base commit")
	head := flag.String("head", "667530452f6eede6989beeff224954594942d35e", "review head commit")
	arm := flag.String("arm", "main", "implementation label")
	implementation := flag.String("implementation", "", "implementation commit")
	workload := flag.String("case", "submodule-preservation", "workload label")
	trial := flag.Int("trial", 1, "repetition within this workload and arm")
	sequence := flag.Int("sequence", 0, "interleaved launch ordinal")
	n := flag.Int("n", 1, "independent reviews")
	flag.Parse()
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	cfg.PR.BaseBranch = *base
	for i := 0; i < *n; i++ {
		ag, err := agent.New(types.AgentPi, "pi", []string{"--provider", "openai-codex", "--model", "gpt-6.1-sol", "--thinking", "high", "--no-extensions", "--no-skills", "--no-prompt-templates"})
		if err != nil {
			panic(err)
		}
		measured := &measuredAgent{Agent: ag}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		started := time.Now()
		var validationErrors []string
		log := func(s string) {
			fmt.Fprintln(os.Stderr, s)
			if strings.HasPrefix(s, "review analyzer findings rejected (") {
				validationErrors = append(validationErrors, s)
			}
		}
		outcome, err := (&steps.ReviewStep{}).Execute(&pipeline.StepContext{
			Ctx: ctx, WorkDir: *cwd, Config: cfg, Agent: measured, EvalReplay: true,
			Repo: &db.Repo{}, Run: &db.Run{Branch: "benchmark-" + *workload, BaseSHA: *base, HeadSHA: *head},
			Log: log, LogChunk: func(string) {}, LogFile: func(string) {},
		})
		cancel()
		_ = measured.Close()
		firstValid := len(measured.AdapterAttempts) > 0 && measured.AdapterAttempts[0]["schema_valid"] == true
		exhausted := err != nil && strings.HasPrefix(err.Error(), "validate review analyzer findings after ")
		row := map[string]any{
			"arm": *arm, "implementation": *implementation, "case": *workload, "trial": *trial,
			"sequence": *sequence, "iteration": i + 1, "at": started.UTC().Format(time.RFC3339),
			"base": *base, "head": *head, "wall_ms": time.Since(started).Milliseconds(),
			"attempts": measured.Attempts, "adapter_attempts": measured.AdapterAttempts,
			"schema_valid_first_attempt": firstValid, "schema_retries": max(0, len(measured.Attempts)-1),
			"adapter_retries": max(0, len(measured.AdapterAttempts)-len(measured.Attempts)),
			"completed":       err == nil, "schema_exhausted": exhausted, "validation_errors": validationErrors,
		}
		if err != nil {
			row["error"] = err.Error()
		}
		if outcome != nil {
			row["needs_approval"] = outcome.NeedsApproval
			row["findings"] = json.RawMessage(outcome.Findings)
			row["reviewed_paths"] = outcome.ReviewedPaths
		}
		if encodeErr := json.NewEncoder(os.Stdout).Encode(row); encodeErr != nil {
			fmt.Fprintln(os.Stderr, encodeErr)
			os.Exit(1)
		}
		if err != nil && !exhausted {
			os.Exit(1)
		}
	}
}

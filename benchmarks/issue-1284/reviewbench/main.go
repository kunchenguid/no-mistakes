package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	Attempts []map[string]any
}

func (a *measuredAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	started := time.Now()
	result, err := a.Agent.Run(ctx, opts)
	row := map[string]any{"purpose": opts.Purpose, "wall_ms": time.Since(started).Milliseconds(), "schema_valid": err == nil}
	if err != nil {
		row["error"] = err.Error()
		row["schema_rejected"] = agent.IsStructuredOutputRejected(err)
	}
	if result != nil {
		row["model"] = result.Model
		row["provider"] = result.ModelProvider
		row["usage"] = result.Usage
	}
	a.Attempts = append(a.Attempts, row)
	return result, err
}

func main() {
	cwd := flag.String("cwd", ".", "repository whose change is reviewed")
	base := flag.String("base", "728ffe0f226527a77358bb265be6073c0786367e", "review base commit")
	head := flag.String("head", "667530452f6eede6989beeff224954594942d35e", "review head commit")
	arm := flag.String("arm", "main", "implementation label")
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
		outcome, err := (&steps.ReviewStep{}).Execute(&pipeline.StepContext{
			Ctx: ctx, WorkDir: *cwd, Config: cfg, Agent: measured, EvalReplay: true,
			Repo: &db.Repo{}, Run: &db.Run{Branch: "benchmark-submodule-preservation", BaseSHA: *base, HeadSHA: *head},
			Log: func(s string) { fmt.Fprintln(os.Stderr, s) }, LogChunk: func(string) {}, LogFile: func(string) {},
		})
		cancel()
		_ = measured.Close()
		row := map[string]any{"arm": *arm, "iteration": i + 1, "at": started.UTC().Format(time.RFC3339), "base": *base, "head": *head, "wall_ms": time.Since(started).Milliseconds(), "attempts": measured.Attempts, "completed": err == nil}
		if err != nil {
			row["error"] = err.Error()
		}
		if outcome != nil {
			row["needs_approval"] = outcome.NeedsApproval
			row["findings"] = json.RawMessage(outcome.Findings)
			row["reviewed_paths"] = outcome.ReviewedPaths
		}
		_ = json.NewEncoder(os.Stdout).Encode(row)
		if err != nil && !agent.IsStructuredOutputRejected(err) {
			os.Exit(1)
		}
	}
}

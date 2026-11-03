package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_QuotaSwitchAmendsTheRunsRecord pins the executor's side of the
// quota-aware mid-run switch: a quota_switch lifecycle event is recorded on
// the run's quota-routing record (from, to, reason, in order), the narrative
// still reaches the step log, and the switch never touches activity.
func TestExecutor_QuotaSwitchAmendsTheRunsRecord(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	record := quotarouting.NewRecord(true,
		[]types.AgentName{types.AgentClaude, types.AgentCodex},
		[]types.AgentName{types.AgentCodex, types.AgentClaude},
		quotarouting.Selection{
			Choice: types.AgentCodex,
			Order:  []types.AgentName{types.AgentCodex, types.AgentClaude},
			Reason: "ranked by spendPriority",
		})
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunQuotaRouting(run.ID, string(payload)); err != nil {
		t.Fatal(err)
	}

	var stepResultID string
	switchingAgent := &lifecycleEmittingAgent{
		events: []agent.LifecycleEvent{
			{Agent: "codex", Phase: agent.LifecyclePhaseStart, PID: 7, Message: "codex started pid=7"},
			{Agent: "codex", Phase: agent.LifecyclePhaseQuotaSwitch, NextAgent: "claude",
				Message: "agent codex exhausted its quota (quota exceeded); switching to claude with a fresh session"},
		},
	}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			stepResultID = sctx.StepResultID
			if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"}); err != nil {
				t.Fatalf("run agent: %v", err)
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, switchingAgent, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	storedPayload, err := database.GetRunQuotaRouting(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := quotarouting.ParseRecord(storedPayload)
	if err != nil || stored == nil {
		t.Fatalf("read amended record: %v %v", stored, err)
	}
	if len(stored.Switches) != 1 {
		t.Fatalf("switches = %+v, want the single codex->claude switch", stored.Switches)
	}
	got := stored.Switches[0]
	if got.From != "codex" || got.To != "claude" {
		t.Fatalf("switch = %s -> %s, want codex -> claude", got.From, got.To)
	}
	if !strings.Contains(got.Reason, "quota exceeded") || !strings.Contains(got.Reason, "fresh session") {
		t.Fatalf("switch reason = %q, want the lifecycle prose", got.Reason)
	}
	if got.At == 0 || got.At > time.Now().Unix() {
		t.Fatalf("switch at = %d, want a plausible wall-clock stamp", got.At)
	}

	sr, err := database.GetStepResult(stepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if sr.LogPath == nil {
		t.Fatal("expected a step log path")
	}
	logBytes, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), filepath.Base(*sr.LogPath)))
	if err != nil {
		t.Fatalf("read step log: %v", err)
	}
	if !strings.Contains(string(logBytes), "exhausted its quota") {
		t.Fatalf("step log = %q, want the switch narrative an operator reads", logBytes)
	}
}

// TestExecutor_QuotaSwitchWithoutARecordIsOnlyLogged pins the pre-feature and
// lost-write shape: a switch event on a run with no stored record must not
// create one and must not fail the run.
func TestExecutor_QuotaSwitchWithoutARecordIsOnlyLogged(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	switchingAgent := &lifecycleEmittingAgent{
		events: []agent.LifecycleEvent{
			{Agent: "pi", Phase: agent.LifecyclePhaseQuotaSwitch, NextAgent: "codex",
				Message: "agent pi exhausted its quota; switching to codex"},
		},
	}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"}); err != nil {
				t.Fatalf("run agent: %v", err)
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, switchingAgent, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("a switch on a run without a record must not fail the run: %v", err)
	}
	payload, err := database.GetRunQuotaRouting(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := quotarouting.ParseRecord(payload); err != nil || record != nil {
		t.Fatalf("no record must be created for a pre-feature run, got %v %v", record, err)
	}
}

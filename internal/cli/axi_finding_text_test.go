package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// longAskUserDescription is well past any inline cap and keeps its reasoning in
// the tail, where a prefix cut used to drop it.
func longAskUserDescription() string {
	return "The new retry wrapper changes delivery semantics. " +
		strings.Repeat("Context the reviewer needs to weigh the tradeoff. ", 40) +
		"Decision needed: keep at-least-once delivery or restore at-most-once."
}

type decodedFindingRow struct {
	ID          string `toon:"id"`
	Action      string `toon:"action"`
	Description string `toon:"description"`
}

// The skill requires ask-user findings to be relayed verbatim, and the gate is
// the only place a driver reads them, so the gate must never cut one.
func TestGateRendersFindingDescriptionsVerbatim(t *testing.T) {
	desc := longAskUserDescription()
	gate := stepView{
		Name:   string(types.StepReview),
		Status: string(types.StepStatusAwaitingApproval),
		FindingsJSON: findingsJSON(t, []types.Finding{
			{ID: "r1", Severity: "warning", File: "internal/retry.go", Action: types.ActionAskUser, Description: desc},
		}, "one tradeoff"),
	}

	var doc struct {
		Gate struct {
			Findings []decodedFindingRow `toon:"findings"`
		} `toon:"gate"`
	}
	out := axiDoc(gateFields(gate)...)
	if err := toon.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode gate: %v\n%s", err, out)
	}
	if len(doc.Gate.Findings) != 1 || doc.Gate.Findings[0].Description != desc {
		t.Fatalf("gate finding description was not rendered verbatim:\n%s", out)
	}
}

// After a gate resolves, status only counts a step's findings, so `axi logs`
// is the read path for their text; the summary stays bounded unless --full.
func TestAxiLogsRendersRecordedFindingsAfterTheGateResolves(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)

	dbRun, err := database.InsertRun(repo.ID, "feature/findings", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(dbRun.ID, types.RunCompleted); err != nil {
		t.Fatalf("mark run completed: %v", err)
	}
	stepResult, err := database.InsertStepResult(dbRun.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	desc := longAskUserDescription()
	summary := strings.Repeat("s", maxGateSummary+25)
	if err := database.SetStepFindings(stepResult.ID, findingsJSON(t, []types.Finding{
		{ID: "r1", Severity: "warning", File: "internal/retry.go", Action: types.ActionAskUser, Description: desc},
	}, summary)); err != nil {
		t.Fatalf("set findings: %v", err)
	}
	if err := database.UpdateStepStatus(stepResult.ID, types.StepStatusCompleted); err != nil {
		t.Fatalf("complete step: %v", err)
	}

	type logsDoc struct {
		Summary  string              `toon:"summary"`
		Findings []decodedFindingRow `toon:"findings"`
		Help     []string            `toon:"help"`
	}
	runLogs := func(full bool) logsDoc {
		t.Helper()
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&out)
		if err := runAxiLogs(cmd, string(types.StepReview), dbRun.ID, full); err != nil {
			t.Fatalf("axi logs (full=%v): %v\n%s", full, err, out.String())
		}
		var doc logsDoc
		if err := toon.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("decode logs (full=%v): %v\n%s", full, err, out.String())
		}
		if len(doc.Findings) != 1 || doc.Findings[0].Description != desc || doc.Findings[0].Action != types.ActionAskUser {
			t.Fatalf("logs (full=%v) did not render the recorded finding verbatim:\n%s", full, out.String())
		}
		return doc
	}

	bounded := runLogs(false)
	if bounded.Summary == summary || !strings.Contains(bounded.Summary, "truncated, 1225 chars total") {
		t.Fatalf("default logs should bound the summary, got %q", bounded.Summary)
	}
	fullCommand := axiLogsFullCommand(string(types.StepReview), dbRun.ID)
	if len(bounded.Help) != 1 || !strings.Contains(bounded.Help[0], fullCommand) {
		t.Fatalf("a bounded summary should point to %q even without a log, got %v", fullCommand, bounded.Help)
	}

	logDir := p.RunLogDir(dbRun.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "review.log"), []byte("review finished\n"), 0o644); err != nil {
		t.Fatalf("write review log: %v", err)
	}
	complete := runLogs(true)
	if complete.Summary != summary {
		t.Fatalf("--full should render the complete summary, got %d chars", len(complete.Summary))
	}
	if len(complete.Help) != 0 {
		t.Fatalf("--full output has nothing left to expand, got help %v", complete.Help)
	}
}

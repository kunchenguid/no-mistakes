package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A repository gate keeps its own step log, and the truncation marker a failing
// command gate emits tells the operator to read it with exactly this command.
// The CLI used to refuse the name it had just told them to type.
func TestAxiLogsReadsARepositoryGateStepLog(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)

	dbRun, err := database.InsertRun(repo.ID, "feature/gates", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(dbRun.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	step := types.CustomGateStepName(types.StepTest, "mutation-budget")
	logDir := p.RunLogDir(dbRun.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, string(step)+".log"), []byte("mutation score 41%\n"), 0o644); err != nil {
		t.Fatalf("write gate log: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiLogs(cmd, string(step), dbRun.ID, true); err != nil {
		t.Fatalf("axi logs --step %s: %v\n%s", step, err, out.String())
	}
	if !strings.Contains(out.String(), "mutation score 41%") {
		t.Fatalf("gate step log was not rendered:\n%s", out.String())
	}
}

// Accepting gate names must not turn --step into an arbitrary file read: the
// label is operator input joined straight onto the run's log directory.
func TestAxiLogsRefusesAGateStepNameThatWouldEscapeTheLogDirectory(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)

	dbRun, err := database.InsertRun(repo.ID, "feature/gates", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(dbRun.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	logDir := p.RunLogDir(dbRun.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	secret := filepath.Join(filepath.Dir(filepath.Dir(logDir)), "stolen.log")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-CONTENTS\n"), 0o644); err != nil {
		t.Fatalf("write out-of-tree file: %v", err)
	}

	for _, step := range []string{"gate.test.../../stolen", `gate.test...\..\stolen`, "gate.test.a/b"} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&out)
		if _, err := runAxiLogs(cmd, step, dbRun.ID, true); err == nil {
			t.Errorf("axi logs --step %q was accepted, want refusal", step)
		}
		if strings.Contains(out.String(), "TOP-SECRET-CONTENTS") {
			t.Errorf("axi logs --step %q read a file outside the run log directory", step)
		}
	}
}

// A gate's name is repository-declared text, so it must not reach remote
// telemetry verbatim - but it is a real step, so it must not read as invalid
// either.
func TestSanitizeAxiTelemetryStep_CollapsesAGateToABoundedToken(t *testing.T) {
	if got := sanitizeAxiTelemetryStep(string(types.CustomGateStepName(types.StepReview, "arch-fitness"))); got != "gate" {
		t.Fatalf("sanitizeAxiTelemetryStep(gate) = %q, want %q", got, "gate")
	}
	if got := sanitizeAxiTelemetryStep("review"); got != "review" {
		t.Fatalf("sanitizeAxiTelemetryStep(review) = %q", got)
	}
	if got := sanitizeAxiTelemetryStep("secret user text"); got != "invalid" {
		t.Fatalf("sanitizeAxiTelemetryStep(garbage) = %q, want invalid", got)
	}
}

// A gate is trusted-config-only, so a pushed branch must not be able to switch
// one off through a push option. Read-only surfaces widened; skip did not.
func TestSkipPushOptionRefusesARepositoryGateStepName(t *testing.T) {
	gate := string(types.CustomGateStepName(types.StepTest, "mutation-budget"))
	if _, err := parseSkipSteps(gate); err == nil {
		t.Fatalf("parseSkipSteps(%q) was accepted, want refusal", gate)
	}
	if _, err := parseSkipPushOptions([]string{"no-mistakes.skip=" + gate}); err == nil {
		t.Fatalf("no-mistakes.skip=%s was accepted, want refusal", gate)
	}
}

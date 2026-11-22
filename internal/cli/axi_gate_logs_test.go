package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
	if err := runAxiLogs(cmd, string(step), dbRun.ID, true); err != nil {
		t.Fatalf("axi logs --step %s: %v\n%s", step, err, out.String())
	}
	if !strings.Contains(out.String(), "mutation score 41%") {
		t.Fatalf("gate step log was not rendered:\n%s", out.String())
	}
}

func TestAxiDocumentOffChangePathsOnDeliveryRetry(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	source, err := database.InsertRun(repo.ID, "feature/carried", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	document, err := database.InsertStepResult(source.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertAgentInvocation(db.AgentInvocation{
		RunID: source.ID, StepName: string(types.StepDocument), Round: 1,
		Purpose: "housekeeping", Agent: "codex", SessionMode: db.InvocationModeCold,
		StartedAt: 1, CompletedAt: 2, DurationMS: 1000, ExitStatus: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[],"document_off_change_paths":["README.md"],"document_off_change_omitted":true}`
	insertAxiRound(t, database, document.ID, 1, "initial", &raw)
	if err := database.UpdateStepStatus(document.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	document, err = database.GetStepResult(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	runID := source.ID
	for range 2 {
		retry, err := database.InsertRun(repo.ID, "feature/carried", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		document, err = database.InsertCarriedStepResult(retry.ID, document)
		if err != nil {
			t.Fatal(err)
		}
		runID = retry.ID
	}
	logDir := p.RunLogDir(source.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "document.log"), []byte("Full path: owner.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiStatus(cmd, runID); err != nil {
		t.Fatalf("status: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "README.md") || !strings.Contains(out.String(), "More paths in the Document step log.") {
		t.Fatalf("retry status lost original Document metadata:\n%s", &out)
	}
	if strings.Contains(out.String(), "shared_work") {
		t.Fatalf("retry status reported source-run housekeeping:\n%s", &out)
	}
	out.Reset()
	if err := runAxiLogs(cmd, "document", runID, true); err != nil {
		t.Fatalf("logs: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "source_run: "+source.ID) || !strings.Contains(out.String(), "Full path: owner.md") {
		t.Fatalf("retry logs omitted the original Document log:\n%s", &out)
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
		if err := runAxiLogs(cmd, step, dbRun.ID, true); err == nil {
			t.Errorf("axi logs --step %q was accepted, want refusal", step)
		}
		if strings.Contains(out.String(), "TOP-SECRET-CONTENTS") {
			t.Errorf("axi logs --step %q read a file outside the run log directory", step)
		}
	}
}

func TestAxiLogsRefusesGatesOnForbiddenAnchors(t *testing.T) {
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

	for _, step := range []string{"gate.intent.fake", "gate.push.fake", "gate.pr.fake", "gate.ci.fake"} {
		if err := os.WriteFile(filepath.Join(logDir, step+".log"), []byte("must not be readable\n"), 0o644); err != nil {
			t.Fatalf("write forbidden gate log: %v", err)
		}
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&out)
		if err := runAxiLogs(cmd, step, dbRun.ID, true); err == nil {
			t.Errorf("axi logs --step %q was accepted, want refusal", step)
		}
		if strings.Contains(out.String(), "must not be readable") {
			t.Errorf("axi logs --step %q rendered a forbidden gate log", step)
		}
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

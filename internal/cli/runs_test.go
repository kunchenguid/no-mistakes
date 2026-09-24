package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunsListsDurableIDBeforeLastFieldPRURL(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	run, err := database.InsertRun(repo.ID, "feature/readiness", strings.Repeat("a", 40), "base")
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/kunchenguid/no-mistakes/pull/123"
	if err := database.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"runs"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(out.String())
	if len(fields) < 3 || fields[0] != "completed" || fields[len(fields)-2] != "id:"+run.ID || fields[len(fields)-1] != prURL {
		t.Fatalf("run discovery row = %q, want status, durable ID, then last-field PR URL", out.String())
	}
}

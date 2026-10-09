package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

func TestVerificationPlanCannotBeChangedOrRebound(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "plan.txt")
	if err := os.WriteFile(source, []byte("observable result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := verificationplan.Capture(filepath.Join(root, "inputs"), source, repo.ID, "feature", "head")
	if err != nil {
		t.Fatal(err)
	}
	intent := &RunIntent{Summary: "  unchanged intent\n", Source: RunIntentSourceAgent, Score: 1}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", intent, "", "", "", "", false, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []any{nil, `{"path":"other"}`} {
		if _, err := d.sql.Exec(`UPDATE runs SET verification_plan = ? WHERE id = ?`, replacement, run.ID); err == nil {
			t.Fatal("immutable attachment replaced")
		}
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.VerificationPlan == nil || *got.VerificationPlan != *plan || got.Intent == nil || *got.Intent != intent.Summary {
		t.Fatalf("read snapshot and intent: %+v %v", got, err)
	}
	if _, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", intent, "", "", "", "", false, plan); err == nil {
		t.Fatal("same capture attached to another run")
	}
	absent, err := d.InsertRun(repo.ID, "without-plan", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET verification_plan = ? WHERE id = ?`, plan, absent.ID); err == nil {
		t.Fatal("plan added after run creation")
	}
}

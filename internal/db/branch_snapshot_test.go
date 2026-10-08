package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGetBranchSnapshotScopeOrderAndStorageErrors(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, branch := range []string{"feature", "feature", "other"} {
		r, err := d.InsertRun(repo.ID, branch, strings.Repeat("a", 40), strings.Repeat("b", 40))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.sql.Exec(`UPDATE runs SET created_at = 1234567890 WHERE id = ?`, r.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	step, err := d.InsertStepResult(ids[1], types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	pin := `[{"name":"static","after":"review","command":"true"}]`
	if err := d.SetRunGates(ids[1], pin); err != nil {
		t.Fatal(err)
	}
	items, err := d.GetBranchSnapshot(context.Background(), repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Run.ID != ids[1] || items[1].Run.ID != ids[0] || items[0].GatesJSON != pin || len(items[0].Steps) != 1 || items[0].Steps[0].Status != types.StepStatusRunning {
		t.Fatalf("snapshot = %#v", items)
	}
	if items, err := d.GetBranchSnapshot(context.Background(), repo.ID, "absent"); err != nil || len(items) != 0 {
		t.Fatalf("empty: %v, %v", items, err)
	}
	if _, err := d.GetBranchSnapshot(context.Background(), "absent", "feature"); err == nil {
		t.Fatal("missing repository succeeded")
	}
	if _, err := d.sql.Exec(`UPDATE runs SET created_at = 'malformed' WHERE id = ?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetBranchSnapshot(context.Background(), repo.ID, "feature"); err == nil {
		t.Fatal("malformed stored timestamp succeeded")
	}
	if _, err := d.sql.Exec(`UPDATE runs SET created_at = 1234567890 WHERE id = ?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE step_results SET step_order = 'malformed' WHERE id = ?`, step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetBranchSnapshot(context.Background(), repo.ID, "feature"); err == nil {
		t.Fatal("malformed step succeeded")
	}
}

func TestGetBranchSnapshotReadsLegacyReportingColumnsWithoutMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	repo, err := writer.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := writer.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	step, err := writer.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	evidence := `{"findings":[],"summary":"live failure approved","verdict":"no-go"}`
	if err := writer.SetStepFindings(step.ID, evidence); err != nil {
		t.Fatal(err)
	}
	if err := writer.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	// Launch/closing metadata is unrelated to publication reporting, and
	// optional outcome reasons were not present in historical databases.
	for _, statement := range []string{
		`ALTER TABLE runs DROP COLUMN closing_issue_refs`,
		`ALTER TABLE runs DROP COLUMN gates_json`,
		`ALTER TABLE runs DROP COLUMN review_approved_head_sha`,
		`ALTER TABLE runs DROP COLUMN last_pushed_sha`,
		`ALTER TABLE runs DROP COLUMN push_target_kind`,
		`ALTER TABLE runs DROP COLUMN push_target_fingerprint`,
		`ALTER TABLE runs DROP COLUMN push_ref`,
		`ALTER TABLE runs DROP COLUMN last_pushed_at`,
		`ALTER TABLE runs DROP COLUMN push_generation`,
		`ALTER TABLE runs DROP COLUMN push_active`,
		`ALTER TABLE step_results DROP COLUMN override_reason`,
		`ALTER TABLE step_results DROP COLUMN approval_reason`,
		`ALTER TABLE step_results DROP COLUMN skip_reason`,
	} {
		if _, err := writer.sql.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	items, err := reader.GetBranchSnapshot(context.Background(), repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Run.ID != run.ID || items[0].Run.HeadSHA != run.HeadSHA || len(items[0].Steps) != 1 || items[0].Steps[0].ID != step.ID {
		t.Fatalf("legacy snapshot = %#v", items)
	}
	if items[0].Steps[0].FindingsJSON == nil || *items[0].Steps[0].FindingsJSON != evidence || items[0].Steps[0].TestOverrideReason() == "" {
		t.Fatal("snapshot lost Test evidence needed to qualify the outcome")
	}
	if items[0].Steps[0].ApprovalReason != nil || items[0].Steps[0].OverrideReason != nil || items[0].Steps[0].SkipReason != nil {
		t.Fatal("legacy reason was fabricated")
	}
	r := items[0].Run
	if r.ReviewApprovedHeadSHA != nil || r.LastPushedSHA != nil || r.PushTargetKind != nil || r.PushTargetFingerprint != nil || r.PushRef != nil || r.LastPushedAt != nil || r.PushGeneration != nil || r.PushActive || items[0].GatesJSON != "" {
		t.Fatal("legacy provenance was fabricated")
	}
	if reader.hasColumn("runs", "review_approved_head_sha") || reader.hasColumn("runs", "last_pushed_sha") || reader.hasColumn("runs", "closing_issue_refs") || reader.hasColumn("step_results", "approval_reason") {
		t.Fatal("read-only snapshot migrated storage")
	}
}

// The inventory exceeds a typical SQL parameter batch and has interleaved
// step orders. Every run must retain its own pin and ordered step evidence.
func TestGetBranchSnapshotWholeInventory(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	otherRepo, err := d.InsertRepo("/test/other", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const count = 1100
	for i := 0; i < count+2; i++ {
		id := fmt.Sprintf("run-%04d", i)
		repoID, branch := repo.ID, "feature"
		if i == count {
			branch = "other"
		} else if i == count+1 {
			repoID = otherRepo.ID
		}
		pin := fmt.Sprintf(`[{"name":"gate-%d","after":"review","command":"true"}]`, i)
		if _, err := tx.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at, gates_json) VALUES (?, ?, ?, 'head', 'base', 'running', ?, 1, ?)`, id, repoID, branch, i+1, pin); err != nil {
			t.Fatal(err)
		}
		for _, order := range []int{2, 1} {
			name := "test"
			if order == 1 {
				name = "review"
			}
			if _, err := tx.Exec(`INSERT INTO step_results (id, run_id, step_name, step_order, status, findings_json, approval_reason) VALUES (?, ?, ?, ?, 'completed', ?, ?)`, fmt.Sprintf("%s-%d", id, order), id, name, order, fmt.Sprintf("evidence-%d", i), fmt.Sprintf("reason-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	items, err := d.GetBranchSnapshot(context.Background(), repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("inventory count = %d", len(items))
	}
	for index, item := range items {
		i := count - 1 - index
		id := fmt.Sprintf("run-%04d", i)
		if item.Run.ID != id || item.GatesJSON != fmt.Sprintf(`[{"name":"gate-%d","after":"review","command":"true"}]`, i) || len(item.Steps) != 2 {
			t.Fatalf("run %d: %#v", i, item)
		}
		for j, step := range item.Steps {
			if step.RunID != id || step.StepOrder != j+1 || step.ApprovalReason == nil || *step.ApprovalReason != fmt.Sprintf("reason-%d", i) {
				t.Fatalf("run %d step %d: %#v", i, j, step)
			}
			if index == 0 && step.StepName == types.StepTest {
				if step.FindingsJSON == nil || *step.FindingsJSON != fmt.Sprintf("evidence-%d", i) {
					t.Fatal("newest Test evidence lost")
				}
			} else if step.FindingsJSON != nil {
				t.Fatal("evidence outside the newest Test sampled")
			}
		}
	}
}

func TestGetBranchSnapshotMissingStepStorageCannotLookEmpty(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`DROP TABLE step_results`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetBranchSnapshot(context.Background(), repo.ID, "absent"); err == nil {
		t.Fatal("missing step table returned an empty success")
	}
}

// A separate writer connection changes run provenance, step state and branch
// membership atomically. Every reader must see all three before or after that
// commit, never a mix sampled by separate status and inventory reads.
func TestGetBranchSnapshotConsistentDuringWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	repo, err := writer.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	r, err := writer.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	s, err := writer.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			tx, err := writer.sql.Begin()
			if err != nil {
				done <- err
				return
			}
			err = func() error {
				if _, err := tx.Exec(`UPDATE runs SET head_sha = ?, gates_json = ? WHERE id = ?`, fmt.Sprint(i), fmt.Sprintf(`[{"name":"generation-%d"}]`, i), r.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE step_results SET step_order = ? WHERE id = ?`, i, s.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM runs WHERE id = 'inventory-marker'`); err != nil {
					return err
				}
				if i%2 == 1 {
					if _, err := tx.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES ('inventory-marker', ?, 'feature', 'marker', 'base', 'pending', 1, 1)`, repo.ID); err != nil {
						return err
					}
				}
				return tx.Commit()
			}()
			if err != nil {
				tx.Rollback()
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		items, err := reader.GetBranchSnapshot(context.Background(), repo.ID, "feature")
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Run.HeadSHA == strings.Repeat("a", 40) {
			continue
		}
		generation := items[0].Steps[0].StepOrder
		if items[0].Run.HeadSHA != fmt.Sprint(generation) || items[0].GatesJSON != fmt.Sprintf(`[{"name":"generation-%d"}]`, generation) || len(items) != 1+generation%2 {
			t.Fatalf("mixed snapshot: run=%s step=%d inventory=%d", items[0].Run.HeadSHA, generation, len(items))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

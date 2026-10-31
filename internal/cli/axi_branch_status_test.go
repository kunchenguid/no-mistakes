package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func branchTestPtr[T any](v T) *T { return &v }

func branchTestSnapshot() db.BranchRunSnapshot {
	r := &db.Run{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", RepoID: "repo-1", Branch: "feature/report", CreatedAt: 1700000001,
		Status: types.RunRunning, HeadSHA: strings.Repeat("a", 40), ReviewApprovedHeadSHA: branchTestPtr(strings.Repeat("b", 40)),
		LastPushedSHA: branchTestPtr(strings.Repeat("c", 40)), LastPushedAt: branchTestPtr(int64(1700000010)), PushGeneration: branchTestPtr(int64(2)),
		PushTargetKind: branchTestPtr("fork"), PushTargetFingerprint: branchTestPtr(strings.Repeat("d", 64)), PushRef: branchTestPtr("refs/heads/feature/report"),
		PRURL: branchTestPtr("https://example.com/repository/pull/1"),
	}
	item := db.BranchRunSnapshot{Run: r, GatesJSON: `[{"name":"static","after":"lint","command":"true"}]`}
	for _, name := range append(types.AllSteps(), types.CustomGateStepName(types.StepLint, "static")) {
		status := types.StepStatusCompleted
		if name == types.StepCI {
			status = types.StepStatusRunning
		}
		item.Steps = append(item.Steps, &db.StepResult{ID: string(name), RunID: r.ID, StepName: name, StepOrder: name.Order(), Status: status})
	}
	return item
}

func decodeBranchFields(t *testing.T, items []db.BranchRunSnapshot) map[string]any {
	t.Helper()
	fields, err := branchStatusFields("repo-1", "feature/report", items)
	if err != nil {
		t.Fatal(err)
	}
	value, err := toon.DecodeString(axiDoc(fields...))
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, axiDoc(fields...))
	}
	return value.(map[string]any)
}

func TestBranchStatusSnapshotFields(t *testing.T) {
	item := branchTestSnapshot()
	older := branchTestSnapshot()
	older.Run.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAU"
	older.Run.CreatedAt--
	older.Run.Status = types.RunPending
	older.Steps = nil
	got := decodeBranchFields(t, []db.BranchRunSnapshot{item, older})
	for key, want := range map[string]any{
		"schema": "branch-publication.v1", "inventory_complete": true, "creation_order": "created_at DESC, id DESC", "newest_run_id": item.Run.ID,
	} {
		if got[key] != want {
			t.Errorf("%s = %#v, want %#v", key, got[key], want)
		}
	}
	scope := got["scope"].(map[string]any)
	if scope["repository_id"] != "repo-1" || scope["branch"] != item.Run.Branch {
		t.Fatalf("scope = %#v", scope)
	}
	run := got["run"].(map[string]any)
	for key, want := range map[string]any{
		"id": item.Run.ID, "repository_id": "repo-1", "branch": item.Run.Branch, "created_at": float64(item.Run.CreatedAt),
		"status": "running", "current_step": "ci", "outcome": nil, "head_sha": item.Run.HeadSHA,
		"reviewed_head_sha": *item.Run.ReviewApprovedHeadSHA, "last_pushed_sha": *item.Run.LastPushedSHA,
		"last_pushed_at": float64(*item.Run.LastPushedAt), "push_generation": float64(2), "push_active": false,
		"push_target_kind": "fork", "push_target_fingerprint": *item.Run.PushTargetFingerprint, "push_ref": *item.Run.PushRef, "pr": *item.Run.PRURL,
	} {
		if run[key] != want {
			t.Errorf("run.%s = %#v, want %#v", key, run[key], want)
		}
	}
	steps := run["steps"].([]any)
	wantNames := []string{"intent", "rebase", "review", "test", "document", "lint", "gate.lint.static", "push", "pr", "ci"}
	for i, value := range steps {
		row := value.(map[string]any)
		wantStatus := "completed"
		if i == len(steps)-1 {
			wantStatus = "running"
		}
		if row["step"] != wantNames[i] || row["position"] != float64(i+1) || row["recorded"] != true || row["status"] != wantStatus {
			t.Errorf("step %d = %#v", i, row)
		}
	}
	inventory := got["runs"].([]any)
	if len(inventory) != 2 || inventory[0].(map[string]any)["id"] != item.Run.ID || inventory[1].(map[string]any)["status"] != "pending" {
		t.Fatalf("inventory = %#v", inventory)
	}
	if inventory[0].(map[string]any)["created_at"] != float64(item.Run.CreatedAt) || inventory[0].(map[string]any)["current_step"] != "ci" {
		t.Fatalf("inventory lost creation order or phase: %#v", inventory)
	}
	others := got["other_running_run_ids"].([]any)
	if len(others) != 1 || others[0] != older.Run.ID {
		t.Fatalf("other running = %#v", others)
	}
}

func TestBranchStatusNullProvenanceAndMissingSteps(t *testing.T) {
	item := branchTestSnapshot()
	item.Run.ReviewApprovedHeadSHA, item.Run.LastPushedSHA, item.Run.LastPushedAt, item.Run.PushGeneration = nil, nil, nil, nil
	item.Run.PushTargetKind, item.Run.PushTargetFingerprint, item.Run.PushRef = nil, nil, nil
	item.Run.PushActive = true
	item.Steps = nil
	run := decodeBranchFields(t, []db.BranchRunSnapshot{item})["run"].(map[string]any)
	for _, key := range []string{"reviewed_head_sha", "last_pushed_sha", "last_pushed_at", "push_generation", "push_target_kind", "push_target_fingerprint", "push_ref", "current_step"} {
		v, exists := run[key]
		if !exists || v != nil {
			t.Errorf("%s must be explicit null, got %#v", key, v)
		}
	}
	if run["push_active"] != true {
		t.Fatal("lost active push marker")
	}
	for _, value := range run["steps"].([]any) {
		row := value.(map[string]any)
		if row["recorded"] != false || row["status"] != nil {
			t.Fatalf("unrecorded step read as pending: %#v", row)
		}
	}
}

func TestBranchStatusTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		status types.RunStatus
		want   string
	}{{types.RunCompleted, "passed"}, {types.RunFailed, "failed"}, {types.RunCancelled, "cancelled"}, {types.RunCIMonitorInterrupted, "ci-monitor-interrupted"}} {
		item := branchTestSnapshot()
		item.Run.Status = tc.status
		run := decodeBranchFields(t, []db.BranchRunSnapshot{item})["run"].(map[string]any)
		if run["outcome"] != tc.want || run["current_step"] != nil {
			t.Fatalf("terminal run = %#v", run)
		}
	}
	item := branchTestSnapshot()
	item.Run.Status = types.RunCompleted
	for _, s := range item.Steps {
		if s.StepName == types.StepCI {
			s.OverrideReason = branchTestPtr("explicit approval")
		}
	}
	if got := decodeBranchFields(t, []db.BranchRunSnapshot{item})["run"].(map[string]any)["outcome"]; got != "passed-with-override" {
		t.Fatalf("outcome = %v", got)
	}
}

func TestBranchStatusPreservesApprovedTestOutcomes(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		want    string
	}{{"no-go", "passed-with-override"}, {"inconclusive", "passed-with-override"}, {"no-surface", "passed"}} {
		t.Run(tc.verdict, func(t *testing.T) {
			item := branchTestSnapshot()
			item.Run.Status = types.RunCompleted
			for _, step := range item.Steps {
				step.Status = types.StepStatusCompleted
				if step.StepName == types.StepTest {
					step.ApprovalReason = branchTestPtr("explicit operator decision")
					step.FindingsJSON = branchTestPtr(`{"findings":[],"summary":"validation result","verdict":"` + tc.verdict + `"}`)
				}
			}
			if got := decodeBranchFields(t, []db.BranchRunSnapshot{item})["run"].(map[string]any)["outcome"]; got != tc.want {
				t.Fatalf("outcome = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestBranchStatusEmpty(t *testing.T) {
	got := decodeBranchFields(t, nil)
	if got["newest_run_id"] != nil || got["run"] != nil || len(got["runs"].([]any)) != 0 || len(got["other_running_run_ids"].([]any)) != 0 || got["inventory_complete"] != true {
		t.Fatalf("empty snapshot = %#v", got)
	}
}

func TestBranchStatusRejectsMalformedRecords(t *testing.T) {
	cases := map[string]func(*db.BranchRunSnapshot){
		"wrong repo":       func(s *db.BranchRunSnapshot) { s.Run.RepoID = "other" },
		"wrong branch":     func(s *db.BranchRunSnapshot) { s.Run.Branch = "other" },
		"empty id":         func(s *db.BranchRunSnapshot) { s.Run.ID = "" },
		"creation time":    func(s *db.BranchRunSnapshot) { s.Run.CreatedAt = 0 },
		"run status":       func(s *db.BranchRunSnapshot) { s.Run.Status = "bogus" },
		"head":             func(s *db.BranchRunSnapshot) { s.Run.HeadSHA = "abc" },
		"reviewed":         func(s *db.BranchRunSnapshot) { s.Run.ReviewApprovedHeadSHA = branchTestPtr("bad") },
		"pushed":           func(s *db.BranchRunSnapshot) { s.Run.LastPushedSHA = branchTestPtr("bad") },
		"target kind":      func(s *db.BranchRunSnapshot) { s.Run.PushTargetKind = branchTestPtr("bad") },
		"fingerprint":      func(s *db.BranchRunSnapshot) { s.Run.PushTargetFingerprint = branchTestPtr("bad") },
		"ref":              func(s *db.BranchRunSnapshot) { s.Run.PushRef = branchTestPtr("refs/heads/other") },
		"generation":       func(s *db.BranchRunSnapshot) { s.Run.PushGeneration = branchTestPtr(int64(-1)) },
		"publication time": func(s *db.BranchRunSnapshot) { s.Run.LastPushedAt = branchTestPtr(int64(-1)) },
		"gate pin":         func(s *db.BranchRunSnapshot) { s.GatesJSON = "{" },
		"step status":      func(s *db.BranchRunSnapshot) { s.Steps[0].Status = "bad" },
		"step order":       func(s *db.BranchRunSnapshot) { s.Steps[0].StepOrder = 999 },
		"step identity":    func(s *db.BranchRunSnapshot) { s.Steps[0].RunID = "other" },
		"step duplicate":   func(s *db.BranchRunSnapshot) { s.Steps = append(s.Steps, s.Steps[0]) },
		"unexpected gate":  func(s *db.BranchRunSnapshot) { s.GatesJSON = "" },
		"active steps":     func(s *db.BranchRunSnapshot) { s.Steps[0].Status = types.StepStatusRunning },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := branchTestSnapshot()
			mutate(&s)
			if _, err := branchStatusFields("repo-1", "feature/report", []db.BranchRunSnapshot{s}); err == nil {
				t.Fatal("malformed record passed")
			}
		})
	}
}

func TestAxiBranchStatusCommand(t *testing.T) {
	dir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, dir)
	created, err := database.InsertRun(repo.ID, "feature/report", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newAxiStatusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--branch", "feature/report"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	decoded, err := toon.DecodeString(out.String())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.(map[string]any)["newest_run_id"] != created.ID {
		t.Fatalf("%s", out.String())
	}
	if err := database.UpdateRunStatus(created.ID, "malformed"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	cmd = newAxiStatusCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--branch", "feature/report"})
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "error:") || strings.Contains(out.String(), "inventory_complete") {
		t.Fatalf("malformed storage succeeded: %v\n%s", err, out.String())
	}
}

func TestBranchStatusInventoryIsUncappedAndTieOrdered(t *testing.T) {
	items := make([]db.BranchRunSnapshot, 25)
	for i := range items {
		items[i] = branchTestSnapshot()
		items[i].Run.ID = strings.Repeat("z", 25-i)
		items[i].Run.CreatedAt = 1700000000
		items[i].Run.Status = types.RunPending
		items[i].Steps = nil
	}
	got := decodeBranchFields(t, items)
	if len(got["runs"].([]any)) != len(items) || len(got["other_running_run_ids"].([]any)) != len(items)-1 || got["newest_run_id"] != items[0].Run.ID {
		t.Fatal("inventory was capped or tie order was lost")
	}
	items[0], items[1] = items[1], items[0]
	if _, err := branchStatusFields("repo-1", "feature/report", items); err == nil {
		t.Fatal("contradictory creation order passed")
	}
}

func TestAxiBranchStatusDoesNotCreateStorage(t *testing.T) {
	dir, _, _, _ := setupAxiQueryRepo(t)
	chdir(t, dir)
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("NM_HOME", missing)
	var out bytes.Buffer
	cmd := newAxiStatusCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--branch", "feature/report"})
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "error:") {
		t.Fatalf("missing database returned success: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read created app storage: %v", err)
	}
}

func TestAxiBranchStatusRejectsInvalidScope(t *testing.T) {
	for _, args := range [][]string{{"--branch", ""}, {"--branch", "HEAD"}, {"--branch", "bad..ref"}, {"--branch", "x", "--run", "y"}} {
		cmd := newAxiStatusCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

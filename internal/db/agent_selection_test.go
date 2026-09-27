package db

import (
	"path/filepath"
	"testing"
)

// TestRecordAgentSelections_RoundTripsInOrder proves a run's routing history is
// readable after the fact: the opening selection first, every later switch after
// it, with the evidence that justified each one.
func TestRecordAgentSelections_RoundTripsInOrder(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, err := database.InsertRepo("/tmp/quota-repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.InsertRun(repo.ID, "other", "head", "base")
	if err != nil {
		t.Fatal(err)
	}

	reportAt := int64(1_790_000_000)
	if err := database.RecordAgentSelection(AgentSelection{
		RunID:     run.ID,
		Step:      "review",
		Reason:    "run-start",
		Agent:     "claude",
		Provider:  "claude",
		Model:     "claude-sonnet-4-5",
		Evidence:  `{"step":"review","candidates":[]}`,
		ReportAt:  &reportAt,
		CreatedAt: reportAt + 1,
	}); err != nil {
		t.Fatalf("record selection: %v", err)
	}
	if err := database.RecordAgentSelection(AgentSelection{
		RunID:     run.ID,
		Step:      "push",
		Reason:    "step-boundary",
		Agent:     "codex",
		Provider:  "codex",
		Model:     "gpt-5.1-codex",
		Evidence:  `{"step":"push","candidates":[]}`,
		CreatedAt: reportAt + 2,
	}); err != nil {
		t.Fatalf("record selection: %v", err)
	}
	// Another run's history is never mixed in.
	if err := database.RecordAgentSelection(AgentSelection{
		RunID:     other.ID,
		Step:      "review",
		Reason:    "run-start",
		Agent:     "grok",
		Provider:  "grok",
		Evidence:  `{}`,
		CreatedAt: reportAt + 3,
	}); err != nil {
		t.Fatalf("record selection: %v", err)
	}

	selections, err := database.ListAgentSelections(run.ID)
	if err != nil {
		t.Fatalf("list selections: %v", err)
	}
	if len(selections) != 2 {
		t.Fatalf("got %d selections, want 2", len(selections))
	}
	first := selections[0]
	if first.Step != "review" || first.Reason != "run-start" || first.Agent != "claude" ||
		first.Provider != "claude" || first.Model != "claude-sonnet-4-5" {
		t.Fatalf("first selection = %+v", first)
	}
	if first.ReportAt == nil || *first.ReportAt != reportAt {
		t.Fatalf("first report time = %v", first.ReportAt)
	}
	if first.Evidence != `{"step":"review","candidates":[]}` {
		t.Fatalf("first evidence = %q", first.Evidence)
	}
	second := selections[1]
	if second.Agent != "codex" || second.Reason != "step-boundary" {
		t.Fatalf("second selection = %+v", second)
	}
	if second.ReportAt != nil {
		t.Fatalf("an absent report time must stay absent, got %v", *second.ReportAt)
	}
	if second.Model != "gpt-5.1-codex" {
		t.Fatalf("second model = %q", second.Model)
	}

	// A run with no recorded routing has an empty history rather than an error.
	none, err := database.ListAgentSelections("run-missing")
	if err != nil {
		t.Fatalf("list selections: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %d selections for an unknown run", len(none))
	}
}

func TestRecordAgentSelection_RequiresARun(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if err := database.RecordAgentSelection(AgentSelection{Agent: "claude"}); err == nil {
		t.Fatal("a selection without a run id must be refused")
	}
}

package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if run.Status != types.RunPending {
		t.Errorf("status = %q, want %q", run.Status, types.RunPending)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Branch != "feature" {
		t.Errorf("branch = %q, want %q", got.Branch, "feature")
	}
	if got.HeadSHA != "abc123" {
		t.Errorf("head sha = %q, want %q", got.HeadSHA, "abc123")
	}
}

func TestInsertRunWithIntent(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	intent := RunIntent{Summary: "  exact requirements\n", Source: RunIntentSourceRerun, Score: 1}

	run, err := d.InsertRunWithIntent(repo.ID, "feature", "abc123", "def456", &intent)
	if err != nil {
		t.Fatalf("insert run with intent: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Intent == nil || *got.Intent != intent.Summary {
		t.Fatalf("intent = %v, want %q", got.Intent, intent.Summary)
	}
	if got.IntentSource == nil || *got.IntentSource != intent.Source {
		t.Fatalf("intent source = %v, want %q", got.IntentSource, intent.Source)
	}
}

func TestRunCIReadinessStoresNoCIDeclaration(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	if err := d.SetRunCIReadyWithReason(run.ID, true, true); err != nil {
		t.Fatalf("set declared no-CI readiness: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.CIReadyAt == nil || !got.CIReadyNoCI {
		t.Fatalf("declared no-CI readiness = at %v, no_ci %v", got.CIReadyAt, got.CIReadyNoCI)
	}

	if err := d.SetRunCIReadyWithReason(run.ID, true, false); err != nil {
		t.Fatalf("set all-green readiness: %v", err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run after all-green readiness: %v", err)
	}
	if got.CIReadyAt == nil || got.CIReadyNoCI {
		t.Fatalf("all-green readiness = at %v, no_ci %v", got.CIReadyAt, got.CIReadyNoCI)
	}

	if err := d.SetRunCIReady(run.ID, false); err != nil {
		t.Fatalf("clear readiness: %v", err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run after clear: %v", err)
	}
	if got.CIReadyAt != nil || got.CIReadyNoCI {
		t.Fatalf("cleared readiness = at %v, no_ci %v", got.CIReadyAt, got.CIReadyNoCI)
	}
}

func TestRunAwaitingAgentSetAndClear(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// A fresh run is not parked.
	if run.AwaitingAgentSince != nil {
		t.Fatalf("new run AwaitingAgentSince = %v, want nil", *run.AwaitingAgentSince)
	}

	// Entering a gate stamps the marker with a recent timestamp.
	before := now()
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatalf("set awaiting agent: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil after SetRunAwaitingAgent, want a timestamp")
	}
	if *got.AwaitingAgentSince < before {
		t.Errorf("AwaitingAgentSince = %d, want >= %d", *got.AwaitingAgentSince, before)
	}

	// Responding clears the marker.
	if err := d.ClearRunAwaitingAgent(run.ID); err != nil {
		t.Fatalf("clear awaiting agent: %v", err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run after clear: %v", err)
	}
	if got.AwaitingAgentSince != nil {
		t.Errorf("AwaitingAgentSince = %d after clear, want nil", *got.AwaitingAgentSince)
	}
}

func TestRecoverStaleRunsClearsAwaitingAgent(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatalf("set awaiting agent: %v", err)
	}

	// Crash recovery must fail the run and drop the parked marker so a dead run
	// is never reported as still awaiting the agent.
	if _, err := d.RecoverStaleRuns("daemon restarted"); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != types.RunFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.AwaitingAgentSince != nil {
		t.Errorf("AwaitingAgentSince = %d after recovery, want nil", *got.AwaitingAgentSince)
	}
}

func TestRunGetNotFound(t *testing.T) {
	d := openTestDB(t)
	got, err := d.GetRun("nonexistent")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent run")
	}
}

func TestRunsByRepo(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	d.InsertRun(repo.ID, "feature-1", "aaa", "bbb")
	d.InsertRun(repo.ID, "feature-2", "ccc", "ddd")

	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	// newest first
	if runs[0].Branch != "feature-2" {
		t.Errorf("first run branch = %q, want feature-2", runs[0].Branch)
	}
}

func TestRunsByRepoHead(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	older, _ := d.InsertRun(repo.ID, "feature", "head-1", "base")
	d.InsertRun(repo.ID, "feature", "head-2", "base") // same branch, other head
	d.InsertRun(repo.ID, "other", "head-1", "base")   // same head, other branch
	newer, _ := d.InsertRun(repo.ID, "feature", "head-1", "base")

	runs, err := d.GetRunsByRepoHead(repo.ID, "feature", "head-1")
	if err != nil {
		t.Fatalf("get runs by repo head: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs for feature/head-1, want 2: %+v", len(runs), runs)
	}
	// newest first
	if runs[0].ID != newer.ID || runs[1].ID != older.ID {
		t.Errorf("runs = [%s %s], want [%s %s] (newest first)", runs[0].ID, runs[1].ID, newer.ID, older.ID)
	}

	none, err := d.GetRunsByRepoHead(repo.ID, "feature", "missing")
	if err != nil {
		t.Fatalf("get runs by repo head (missing): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d runs for unknown head, want 0", len(none))
	}
}

func TestActiveRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	// no active run initially
	active, err := d.GetActiveRun(repo.ID, "")
	if err != nil {
		t.Fatalf("get active run: %v", err)
	}
	if active != nil {
		t.Fatal("expected nil active run")
	}

	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	active, _ = d.GetActiveRun(repo.ID, "")
	if active == nil || active.ID != run.ID {
		t.Fatal("expected active run matching inserted run")
	}

	// after completing, no active run
	d.UpdateRunStatus(run.ID, types.RunCompleted)
	active, _ = d.GetActiveRun(repo.ID, "")
	if active != nil {
		t.Fatal("expected nil after completing run")
	}
}

func TestActiveRunStrictBranchMatch(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/branchpref", "git@github.com:user/branchpref.git", "main")

	// Create two active runs on different branches.
	runA, _ := d.InsertRun(repo.ID, "feature-a", "aaa", "000")
	runB, _ := d.InsertRun(repo.ID, "feature-b", "bbb", "000")

	// Without branch hint, newest (runB) wins.
	active, err := d.GetActiveRun(repo.ID, "")
	if err != nil {
		t.Fatalf("get active run: %v", err)
	}
	if active == nil || active.ID != runB.ID {
		t.Fatalf("expected newest run %q, got %v", runB.ID, active)
	}

	// With branch hint "feature-a", the matching run is returned.
	active, err = d.GetActiveRun(repo.ID, "feature-a")
	if err != nil {
		t.Fatalf("get active run with branch: %v", err)
	}
	if active == nil || active.ID != runA.ID {
		t.Fatalf("expected branch-matching run %q, got %q", runA.ID, active.ID)
	}

	// With branch hint "feature-b", runB is returned.
	active, err = d.GetActiveRun(repo.ID, "feature-b")
	if err != nil {
		t.Fatalf("get active run with branch: %v", err)
	}
	if active == nil || active.ID != runB.ID {
		t.Fatalf("expected branch-matching run %q, got %q", runB.ID, active.ID)
	}

	// With branch hint for a non-existent branch, return nil (strict match,
	// no fallback). This is what lets the setup wizard know a fresh run is
	// needed for the current branch.
	active, err = d.GetActiveRun(repo.ID, "feature-c")
	if err != nil {
		t.Fatalf("get active run with unknown branch: %v", err)
	}
	if active != nil {
		t.Fatalf("expected nil with no matching branch, got run %q on %q", active.ID, active.Branch)
	}
}

func TestActiveRunsAcrossRepos(t *testing.T) {
	d := openTestDB(t)
	repoA, _ := d.InsertRepo("/home/user/project-a", "git@github.com:user/project-a.git", "main")
	repoB, _ := d.InsertRepo("/home/user/project-b", "git@github.com:user/project-b.git", "main")

	pendingRun, _ := d.InsertRun(repoA.ID, "feature-a", "aaa", "000")
	runningRun, _ := d.InsertRun(repoB.ID, "feature-b", "bbb", "000")
	if err := d.UpdateRunStatus(runningRun.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	completedRun, _ := d.InsertRun(repoA.ID, "done", "ccc", "000")
	if err := d.UpdateRunStatus(completedRun.ID, types.RunCompleted); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	failedRun, _ := d.InsertRun(repoB.ID, "failed", "ddd", "000")
	if err := d.UpdateRunStatus(failedRun.ID, types.RunFailed); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	cancelledRun, _ := d.InsertRun(repoB.ID, "cancelled", "eee", "000")
	if err := d.UpdateRunStatus(cancelledRun.ID, types.RunCancelled); err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}

	runs, err := d.GetActiveRuns()
	if err != nil {
		t.Fatalf("get active runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d active runs, want 2", len(runs))
	}

	got := map[string]types.RunStatus{}
	for _, run := range runs {
		got[run.ID] = run.Status
	}
	if got[pendingRun.ID] != types.RunPending {
		t.Fatalf("pending run missing from active runs: %#v", got)
	}
	if got[runningRun.ID] != types.RunRunning {
		t.Fatalf("running run missing from active runs: %#v", got)
	}
	if _, ok := got[completedRun.ID]; ok {
		t.Fatal("completed run should not be active")
	}
	if _, ok := got[failedRun.ID]; ok {
		t.Fatal("failed run should not be active")
	}
	if _, ok := got[cancelledRun.ID]; ok {
		t.Fatal("cancelled run should not be active")
	}
}

func TestUpdateRunStatus(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunRunning {
		t.Errorf("status = %q, want %q", got.Status, types.RunRunning)
	}
}

func TestVerifiedHeadAndTerminalStatusPersistAtomically(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/atomic-head", "https://github.com/test/atomic-head", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_verified_terminal
		BEFORE UPDATE OF terminal_head_verified_at ON runs
		WHEN NEW.terminal_head_verified_at IS NOT NULL
		BEGIN
			SELECT RAISE(FAIL, 'injected verified terminal failure');
		END`); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatusWithVerifiedHead(run.ID, "cancelled", types.RunCancelled, "pipeline-head"); err == nil {
		t.Fatal("verified terminal update unexpectedly succeeded")
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.HeadSHA != "submitted" || got.TerminalHeadVerifiedAt != nil {
		t.Fatalf("failed atomic update changed run = status %s head %s verified %#v", got.Status, got.HeadSHA, got.TerminalHeadVerifiedAt)
	}
}

func TestRunPushBindingIsForwardOnlyAndLegacyRowsStayNullable(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo-sync-binding", "https://example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != "submitted" || run.LastPushedSHA != nil {
		t.Fatalf("new run provenance = %#v", run)
	}
	fingerprint := publicationTargetFingerprint(repo.PushURL())
	binding := PushBinding{HeadSHA: "pushed-1", TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature"}
	if err := d.UpdateRunPushBinding(run.ID, binding); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed-2", TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetRun(run.ID)
	if got.LastPushedSHA == nil || *got.LastPushedSHA != "pushed-2" || got.PushGeneration == nil || *got.PushGeneration != 2 {
		t.Fatalf("push binding = %#v", got)
	}
	if got.PushTargetFingerprint == nil || *got.PushTargetFingerprint != fingerprint {
		t.Fatalf("target fingerprint = %#v", got.PushTargetFingerprint)
	}
	if got.SubmittedHeadSHA == nil || *got.SubmittedHeadSHA != "submitted" {
		t.Fatalf("submitted head was mutated: %#v", got.SubmittedHeadSHA)
	}
}

func TestCustodyCASRejectsPublicationAttemptRecordedAfterSnapshot(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo-publication-fence", "https://example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	expected, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := PublicationAttempt{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: publicationTargetFingerprint(repo.PushURL()), Ref: "refs/heads/feature"}
	if err := d.RecordRunPublicationAttempt(run.ID, attempt); err != nil {
		t.Fatalf("record publication attempt: %v", err)
	}
	if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("custody CAS after publication attempt = %v, want ErrRunCustodyCAS", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CustodyReturnedAt != nil {
		t.Fatal("custody was stamped after a publication attempt")
	}
}

func TestNewRunHasExactPublicationJournalTarget(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo-publication-audit", "https://token:secret@example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if run.PublicationJournalState == nil || *run.PublicationJournalState != PublicationJournalReady {
		t.Fatalf("publication journal state = %#v, want ready", run.PublicationJournalState)
	}
	if run.PublicationJournalTargetKind == nil || *run.PublicationJournalTargetKind != "upstream" {
		t.Fatalf("publication journal target kind = %#v", run.PublicationJournalTargetKind)
	}
	if run.PublicationJournalTargetFingerprint == nil || *run.PublicationJournalTargetFingerprint == "" {
		t.Fatalf("publication journal target fingerprint = %#v", run.PublicationJournalTargetFingerprint)
	}
	if run.PublicationJournalRef == nil || *run.PublicationJournalRef != "refs/heads/feature" {
		t.Fatalf("publication journal ref = %#v", run.PublicationJournalRef)
	}
	if run.PublicationJournalTargetVersion == nil || *run.PublicationJournalTargetVersion != repo.URLVersion {
		t.Fatalf("publication journal target version = %#v, want %d", run.PublicationJournalTargetVersion, repo.URLVersion)
	}
}

func TestRunPublicationTargetsAreBoundPerTarget(t *testing.T) {
	d := openTestDB(t)
	parent := "https://github.com/example/parent.git"
	fork := "https://github.com/example/fork.git"
	repo, err := d.InsertRepoWithFork("/tmp/repo-publication-targets", parent, fork, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("publication target count = %d, want 2", len(targets))
	}
	set, err := d.GetRunPublicationTargetSet(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.State != PublicationTargetSetComplete || set.TargetCount != len(targets) || set.TargetSetHash != PublicationTargetSetHash(targets) {
		t.Fatalf("publication target set = %#v, targets = %#v", set, targets)
	}
	parentFingerprint := publicationTargetFingerprint(parent)
	forkFingerprint := publicationTargetFingerprint(fork)
	if err := d.RecordRunPublicationAttempt(run.ID, PublicationAttempt{HeadSHA: "attempted", TargetKind: "fork", TargetFingerprint: forkFingerprint, Ref: "refs/heads/feature"}); err != nil {
		t.Fatalf("record fork publication attempt: %v", err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "attempted", TargetKind: "fork", TargetFingerprint: forkFingerprint, Ref: "refs/heads/feature"}); err != nil {
		t.Fatalf("record fork publication: %v", err)
	}
	if err := d.UpdateRunPRURLForTarget(run.ID, "https://github.com/example/parent/pull/7", "upstream", parentFingerprint); err != nil {
		t.Fatalf("record parent publication identity: %v", err)
	}
	targets, err = d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byFingerprint := make(map[string]RunPublicationTarget, len(targets))
	for _, target := range targets {
		byFingerprint[target.TargetFingerprint] = target
	}
	if got := byFingerprint[forkFingerprint]; got.State != PublicationTargetPublished || got.AttemptHeadSHA != "attempted" {
		t.Fatalf("fork publication target = %#v", got)
	}
	if got := byFingerprint[parentFingerprint]; got.State != PublicationTargetNoAttempt || got.PRState != PublicationTargetPROpened || got.PRRequestIdentity != "https://github.com/example/parent/pull/7" {
		t.Fatalf("parent publication target = %#v", got)
	}
}

func TestRunPublicationEvidenceBindsCustodyCAS(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-evidence", "https://github.com/example/evidence.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("publication targets = %#v, %v", targets, err)
	}
	if err := d.ReconcileRunPublicationTargetLineage(run.ID, targets[0].TargetFingerprint, "none"); err != nil {
		t.Fatalf("reconcile publication lineage: %v", err)
	}
	targets, err = d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	set, err := d.RecordRunPublicationEvidence(run.ID, []PublicationEvidenceInput{{
		TargetFingerprint: targets[0].TargetFingerprint,
		Ref:               targets[0].Ref,
		TargetVersion:     targets[0].TargetVersion,
		RemoteHash:        "remote-a",
		ProviderHash:      "provider-a",
		Cursor:            "cursor-a",
		Since:             run.CreatedAt,
		Until:             run.UpdatedAt,
	}})
	if err != nil {
		t.Fatalf("record publication evidence: %v", err)
	}
	if set.EvidenceHash == "" || set.EvidenceGeneration == 0 {
		t.Fatalf("publication evidence snapshot = %#v", set)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	expected, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expected.PublicationEvidenceHash != set.EvidenceHash || expected.PublicationEvidenceGeneration != set.EvidenceGeneration {
		t.Fatalf("reloaded publication evidence = %q/%d, want %q/%d", expected.PublicationEvidenceHash, expected.PublicationEvidenceGeneration, set.EvidenceHash, set.EvidenceGeneration)
	}
	if _, err := d.RecordRunPublicationEvidence(run.ID, []PublicationEvidenceInput{{
		TargetFingerprint: targets[0].TargetFingerprint,
		Ref:               targets[0].Ref,
		TargetVersion:     targets[0].TargetVersion,
		RemoteHash:        "remote-b",
		ProviderHash:      "provider-b",
		Cursor:            "cursor-b",
		Since:             run.CreatedAt,
		Until:             run.UpdatedAt,
	}}); err != nil {
		t.Fatalf("record changed publication evidence: %v", err)
	}
	if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("custody CAS after evidence change = %v, want compare-and-swap failure", err)
	}
}

func TestRunPublicationTargetPushAndPRChannelsAdvanceIndependently(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-channels", "https://github.com/example/parent.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := publicationTargetFingerprint(repo.UpstreamURL)
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed", TargetKind: "upstream", TargetFingerprint: fingerprint, Ref: "refs/heads/feature"}); err != nil {
		t.Fatalf("record upstream push: %v", err)
	}
	if err := d.PrepareRunPublicationTargetAttempt(run.ID, "upstream", fingerprint, "refs/heads/feature"); err != nil {
		t.Fatalf("prepare upstream PR after push: %v", err)
	}
	if err := d.UpdateRunPRURLForTarget(run.ID, "https://github.com/example/parent/pull/8", "upstream", fingerprint); err != nil {
		t.Fatalf("record upstream PR after push: %v", err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("publication target count = %d, want 1", len(targets))
	}
	got := targets[0]
	if got.State != PublicationTargetPublished || got.AttemptHeadSHA != "pushed" {
		t.Fatalf("push channel = %#v", got)
	}
	if got.PRState != PublicationTargetPROpened || got.PRRequestIdentity != "https://github.com/example/parent/pull/8" {
		t.Fatalf("PR channel = %#v", got)
	}
}

func TestRunPublicationTargetPreparedPRBlocksUnpublishedValidation(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-prepared", "https://github.com/example/parent.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := publicationTargetFingerprint(repo.UpstreamURL)
	if err := d.PrepareRunPublicationTargetAttempt(run.ID, "upstream", fingerprint, "refs/heads/feature"); err != nil {
		t.Fatalf("prepare PR attempt: %v", err)
	}
	if err := d.ValidateRunPublicationTargetLedger(run.ID); err == nil {
		t.Fatal("prepared PR target passed unpublished ledger validation")
	}
}

func TestCustodyCASRejectsPublicationTargetChannelChanges(t *testing.T) {
	t.Run("prepared PR cannot be stamped", func(t *testing.T) {
		d := openTestDB(t)
		repo, err := d.InsertRepo("/tmp/repo-publication-custody-race", "https://github.com/example/parent.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		fingerprint := publicationTargetFingerprint(repo.UpstreamURL)
		if err := d.PrepareRunPublicationTargetAttempt(run.ID, "upstream", fingerprint, "refs/heads/feature"); err != nil {
			t.Fatal(err)
		}
		expected, err := d.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("custody CAS after prepared PR = %v, want ErrRunCustodyCAS", err)
		}
	})

	t.Run("PR preparation cannot enter an active custody transition", func(t *testing.T) {
		d := openTestDB(t)
		repo, err := d.InsertRepo("/tmp/repo-publication-custody-transition", "https://github.com/example/parent.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		expected, err := d.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.BeginRunCustodyTransition(context.Background(), expected); err != nil {
			t.Fatal(err)
		}
		fingerprint := publicationTargetFingerprint(repo.UpstreamURL)
		if err := d.PrepareRunPublicationTargetAttempt(run.ID, "upstream", fingerprint, "refs/heads/feature"); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("PR preparation during custody = %v, want ErrRunCustodyCAS", err)
		}
	})
}

func TestLegacyPublicationTargetsWithoutSubmissionGraphRemainAmbiguous(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-legacy-publication-targets", "https://github.com/example/parent.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.sql.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, pr_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'failed', 'none', ?, ?)`, "legacy-no-graph", repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40), time.Now().Unix(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRunPublicationTargets(d.sql); err != nil {
		t.Fatal(err)
	}
	set, err := d.GetRunPublicationTargetSet("legacy-no-graph")
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.State != PublicationTargetSetAmbiguous {
		t.Fatalf("legacy target set = %#v, want ambiguous", set)
	}
	targets, err := d.ListRunPublicationTargets("legacy-no-graph")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("legacy targets = %#v, want no inferred current targets", targets)
	}
}

func TestMigrationBackfillsPendingPRProvenanceForExistingLedger(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-migration", "https://github.com/example/parent.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE run_publication_targets SET pr_provenance = '' WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrateRunPublicationTargets(d.sql); err != nil {
		t.Fatal(err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].PRProvenance != PublicationTargetPRProvenanceMigrationPending {
		t.Fatalf("migrated PR provenance = %#v, want %q", targets, PublicationTargetPRProvenanceMigrationPending)
	}
}

func TestLegacyPublicationLineageReconcilesAtomically(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-lineage-reconcile", "https://github.com/example/parent.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := PublicationTargetFingerprint(repo.UpstreamURL)
	lineage := "refs/pull/7/head"
	if err := d.ReconcileRunPublicationTargetLineage(run.ID, fingerprint, lineage); err != nil {
		t.Fatalf("reconcile publication lineage: %v", err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].RequestLineage != lineage {
		t.Fatalf("reconciled targets = %#v", targets)
	}
	set, err := d.GetRunPublicationTargetSet(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || set.TargetSetHash != PublicationTargetSetHash(targets) || set.Generation == 0 {
		t.Fatalf("reconciled target set = %#v", set)
	}
}

func TestPublicationEvidenceReconcilesAllLineageAtomically(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithFork("/tmp/repo-publication-lineage-evidence", "https://github.com/example/parent.git", "https://github.com/example/fork.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil || len(targets) != 2 {
		t.Fatalf("publication targets = %#v, %v", targets, err)
	}
	lineages := make([]PublicationTargetLineageInput, 0, len(targets))
	evidence := make([]PublicationEvidenceInput, 0, len(targets))
	for _, target := range targets {
		lineage := "none"
		if target.TargetKind == "fork" {
			lineage = "refs/pull/7/head"
		}
		lineages = append(lineages, PublicationTargetLineageInput{TargetFingerprint: target.TargetFingerprint, RequestLineage: lineage})
		evidence = append(evidence, PublicationEvidenceInput{
			TargetFingerprint: target.TargetFingerprint,
			Ref:               target.Ref,
			TargetVersion:     target.TargetVersion,
			RemoteHash:        target.TargetFingerprint + "-remote",
			ProviderHash:      target.TargetFingerprint + "-provider",
			Cursor:            "audit-cutoff=1754136000;provider-date:1754136000;audit;hasNextPage=false;atomic",
			Since:             run.CreatedAt,
			Until:             run.UpdatedAt,
		})
	}
	set, err := d.RecordRunPublicationEvidenceWithLineage(run.ID, lineages, evidence)
	if err != nil {
		t.Fatalf("record publication evidence with lineage: %v", err)
	}
	if set.EvidenceHash == "" || set.EvidenceGeneration == 0 || set.TargetSetHash == "" || set.Generation == 0 {
		t.Fatalf("publication evidence target set = %#v", set)
	}
	updated, err := d.ListRunPublicationTargets(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range updated {
		if target.RequestLineage == "" || target.RequestLineage == PublicationTargetRequestLineageMigrationPending || target.PRProvenance == "" {
			t.Fatalf("publication target was not atomically reconciled: %#v", target)
		}
	}
}

func TestRunPublicationTargetStateRejectsUnknownValue(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-publication-state", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE run_publication_targets SET state = 'corrupt' WHERE run_id = ?`, run.ID); err == nil {
		t.Fatal("unknown publication target state was accepted")
	}
}

func TestPublicationAttemptJournalRequiresExactBindingToReconcile(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo-publication-journal", "https://example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	attempt := PublicationAttempt{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: publicationTargetFingerprint(repo.PushURL()), Ref: "refs/heads/feature"}
	if err := d.RecordRunPublicationAttempt(run.ID, attempt); err != nil {
		t.Fatalf("record publication attempt: %v", err)
	}
	if err := d.ReconcileRunPublicationAttempt(run.ID); !errors.Is(err, ErrRunPublicationCAS) {
		t.Fatalf("unbound publication reconciliation = %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.PublicationAttemptHeadSHA == nil {
		t.Fatal("unresolved publication journal was cleared without a binding")
	}

	if err := d.UpdateRunPushBinding(run.ID, PushBinding{
		HeadSHA: attempt.HeadSHA, TargetKind: attempt.TargetKind,
		TargetFingerprint: attempt.TargetFingerprint, Ref: attempt.Ref,
	}); err != nil {
		t.Fatalf("exact publication binding: %v", err)
	}
	got, _ = d.GetRun(run.ID)
	if got.PublicationAttemptHeadSHA != nil || got.LastPushedSHA == nil || *got.LastPushedSHA != attempt.HeadSHA {
		t.Fatalf("reconciled publication journal = %#v", got)
	}
}

func TestReconcilePublicationAttemptClearsOnlyMatchingExistingBinding(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/repo-publication-reconcile", "https://example.com/repo.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	attempt := PublicationAttempt{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: publicationTargetFingerprint(repo.PushURL()), Ref: "refs/heads/feature"}
	if err := d.RecordRunPublicationAttempt(run.ID, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE runs SET last_pushed_sha = ?, push_target_kind = ?, push_target_fingerprint = ?, push_ref = ? WHERE id = ?`, attempt.HeadSHA, attempt.TargetKind, attempt.TargetFingerprint, attempt.Ref, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.ReconcileRunPublicationAttempt(run.ID); err != nil {
		t.Fatalf("reconcile matching publication binding: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.PublicationAttemptHeadSHA != nil {
		t.Fatalf("matching publication journal remained: %#v", got)
	}
}

func TestUpdateRunPRURL(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	prURL := "https://github.com/user/project/pull/1"
	if err := d.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatalf("update pr url: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.PRURL == nil || *got.PRURL != prURL {
		t.Errorf("pr url = %v, want %q", got.PRURL, prURL)
	}
}

func TestUpdateRunPRStateFinalizesActiveTerminalOutcomes(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/home/user/pr-terminal-"+state, "git@github.com:user/project.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunPushActive(run.ID, true); err != nil {
				t.Fatal(err)
			}
			ci, _ := d.InsertStepResult(run.ID, types.StepCI)
			if err := d.StartStep(ci.ID); err != nil {
				t.Fatal(err)
			}

			if err := d.UpdateRunPRState(run.ID, state); err != nil {
				t.Fatal(err)
			}

			got, _ := d.GetRun(run.ID)
			if got.Status != types.RunCompleted || got.PRState == nil || *got.PRState != state {
				t.Fatalf("terminal PR run = status %s pr_state %v, want completed/%s", got.Status, got.PRState, state)
			}
			if got.AwaitingAgentSince != nil || got.PushActive {
				t.Fatalf("terminal PR run retained active markers: awaiting=%v push_active=%t", got.AwaitingAgentSince, got.PushActive)
			}
			parkedMS := got.ParkedMS
			if err := d.CompleteRunAwaitingAgent(run.ID, 1234); err != nil {
				t.Fatal(err)
			}
			got, _ = d.GetRun(run.ID)
			if got.ParkedMS != parkedMS {
				t.Fatalf("duplicate gate completion changed parked_ms from %d to %d", parkedMS, got.ParkedMS)
			}
			gotCI, _ := d.GetStepResult(ci.ID)
			if gotCI.Status != types.StepStatusCompleted {
				t.Fatalf("CI status = %s, want completed", gotCI.Status)
			}
			active, err := d.GetActiveRuns()
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatalf("terminal PR remains active: %+v", active)
			}
		})
	}
}

func TestUpdateRunPRStateKeepsOpenRunActive(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/pr-open", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunRunning || got.PRState == nil || *got.PRState != "open" {
		t.Fatalf("open PR run = status %s pr_state %v, want running/open", got.Status, got.PRState)
	}
}

func TestUpdateRunPRStateIgnoresDuplicateAndDelayedRegressions(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/pr-notifications", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"merged", "merged", "open", "closed"} {
		if err := d.UpdateRunPRState(run.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/user/project/pull/1"); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunCompleted || got.PRState == nil || *got.PRState != "merged" {
		t.Fatalf("duplicate/delayed PR observations regressed run: status=%s pr_state=%v", got.Status, got.PRState)
	}
	active, _ := d.GetActiveRuns()
	if len(active) != 0 {
		t.Fatalf("duplicate/delayed PR observations reactivated run: %+v", active)
	}
}

func TestUpdateRunPRStateDoesNotRewriteAlreadyTerminalStatus(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunCompleted, types.RunFailed, types.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/home/user/pr-idempotent-"+string(status), "git@github.com:user/project.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
			if err := d.UpdateRunErrorStatus(run.ID, "original terminal outcome", status); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunPRState(run.ID, "closed"); err != nil {
				t.Fatal(err)
			}
			got, _ := d.GetRun(run.ID)
			if got.Status != status {
				t.Fatalf("status = %s, want preserved %s", got.Status, status)
			}
			if got.Error == nil || *got.Error != "original terminal outcome" {
				t.Fatalf("terminal error changed: %v", got.Error)
			}
		})
	}
}

func TestReconcileTerminalPRRunsFinalizesLegacyActiveRows(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/home/user/pr-recovery-"+state, "git@github.com:user/project.git", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			ci, _ := d.InsertStepResult(run.ID, types.StepCI)
			if err := d.StartStep(ci.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			// Simulate a row written by an older daemon after it observed a terminal
			// PR but before its separate run-status finalization write.
			if _, err := d.sql.Exec(`UPDATE runs SET pr_state = ? WHERE id = ?`, state, run.ID); err != nil {
				t.Fatal(err)
			}

			count, err := d.ReconcileTerminalPRRuns()
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("reconciled count = %d, want 1", count)
			}
			got, _ := d.GetRun(run.ID)
			if got.Status != types.RunCompleted || got.AwaitingAgentSince != nil {
				t.Fatalf("reconciled run = status %s awaiting %v", got.Status, got.AwaitingAgentSince)
			}
			gotCI, _ := d.GetStepResult(ci.ID)
			if gotCI.Status != types.StepStatusCompleted {
				t.Fatalf("reconciled CI status = %s, want completed", gotCI.Status)
			}
			count, err = d.ReconcileTerminalPRRuns()
			if err != nil || count != 0 {
				t.Fatalf("idempotent reconciliation = count %d err %v, want 0/nil", count, err)
			}
		})
	}
}

func TestUpdateRunReviewApprovedHeadSHAReplacesAuthority(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "mutable", "base")
	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("new run inferred review authority: %#v", run.ReviewApprovedHeadSHA)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "reviewed-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "reviewed-2"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != "reviewed-2" {
		t.Fatalf("review-approved head = %#v, want reviewed-2", got.ReviewApprovedHeadSHA)
	}
	if got.HeadSHA != "mutable" {
		t.Fatalf("review authority update mutated run head to %s", got.HeadSHA)
	}
}

func TestUpdateRunHeadSHA(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunHeadSHA(run.ID, "xyz"); err != nil {
		t.Fatalf("update head sha: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.HeadSHA != "xyz" {
		t.Errorf("head sha = %q, want %q", got.HeadSHA, "xyz")
	}
}

func TestUpdateRunError(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	if err := d.UpdateRunError(run.ID, "something broke"); err != nil {
		t.Fatalf("update error: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Error == nil || *got.Error != "something broke" {
		t.Errorf("error = %v, want %q", got.Error, "something broke")
	}
	if got.Status != types.RunFailed {
		t.Errorf("status = %q, want %q", got.Status, types.RunFailed)
	}
}

func TestCascadeDeleteRepo(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)

	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	gotRun, _ := d.GetRun(run.ID)
	if gotRun != nil {
		t.Fatal("expected run to be cascade deleted")
	}
	gotStep, _ := d.GetStepResult(step.ID)
	if gotStep != nil {
		t.Fatal("expected step to be cascade deleted")
	}
}

func TestRecoverStaleRunsMarksRunsFailed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	// Create runs in various statuses.
	pendingRun, _ := d.InsertRun(repo.ID, "feat-a", "aaa", "bbb")
	runningRun, _ := d.InsertRun(repo.ID, "feat-b", "ccc", "ddd")
	d.UpdateRunStatus(runningRun.ID, types.RunRunning)
	completedRun, _ := d.InsertRun(repo.ID, "feat-c", "eee", "fff")
	d.UpdateRunStatus(completedRun.ID, types.RunCompleted)

	count, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	if count != 2 {
		t.Errorf("recovered count = %d, want 2", count)
	}

	// Pending and running should be failed.
	got, _ := d.GetRun(pendingRun.ID)
	if got.Status != types.RunFailed {
		t.Errorf("pending run status = %q, want %q", got.Status, types.RunFailed)
	}
	if got.Error == nil || *got.Error != "daemon crashed" {
		t.Errorf("pending run error = %v, want %q", got.Error, "daemon crashed")
	}

	got, _ = d.GetRun(runningRun.ID)
	if got.Status != types.RunFailed {
		t.Errorf("running run status = %q, want %q", got.Status, types.RunFailed)
	}

	// Completed should be untouched.
	got, _ = d.GetRun(completedRun.ID)
	if got.Status != types.RunCompleted {
		t.Errorf("completed run status = %q, want %q", got.Status, types.RunCompleted)
	}
}

func TestRecoverStaleRunsExceptPreservesOnlyValidatedRuns(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/recovery-project", "git@github.com:user/recovery-project.git", "main")
	preserved, _ := d.InsertRun(repo.ID, "feat-a", "aaa", "bbb")
	stale, _ := d.InsertRun(repo.ID, "feat-b", "ccc", "ddd")
	if err := d.UpdateRunStatus(preserved.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(stale.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	preservedStep, _ := d.InsertStepResult(preserved.ID, types.StepReview)
	staleStep, _ := d.InsertStepResult(stale.ID, types.StepReview)
	if err := d.StartStep(preservedStep.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(staleStep.ID); err != nil {
		t.Fatal(err)
	}

	count, err := d.RecoverStaleRunsExcept("daemon crashed", map[string]struct{}{preserved.ID: {}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recovered count = %d, want 1", count)
	}
	gotPreserved, _ := d.GetRun(preserved.ID)
	gotStale, _ := d.GetRun(stale.ID)
	if gotPreserved.Status != types.RunRunning || gotStale.Status != types.RunFailed {
		t.Fatalf("run statuses = preserved %s stale %s, want running and failed", gotPreserved.Status, gotStale.Status)
	}
	gotPreservedStep, _ := d.GetStepResult(preservedStep.ID)
	gotStaleStep, _ := d.GetStepResult(staleStep.ID)
	if gotPreservedStep.Status != types.StepStatusRunning || gotStaleStep.Status != types.StepStatusFailed {
		t.Fatalf("step statuses = preserved %s stale %s, want running and failed", gotPreservedStep.Status, gotStaleStep.Status)
	}
}

func TestRecoverStaleRunsMarksStepsFailed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project2", "git@github.com:user/project2.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")

	// Create steps in various statuses.
	runningStep, _ := d.InsertStepResult(run.ID, types.StepReview)
	d.StartStep(runningStep.ID)
	awaitingStep, _ := d.InsertStepResult(run.ID, types.StepTest)
	d.UpdateStepStatus(awaitingStep.ID, types.StepStatusAwaitingApproval)
	fixingStep, _ := d.InsertStepResult(run.ID, types.StepLint)
	d.UpdateStepStatus(fixingStep.ID, types.StepStatusFixing)
	completedStep, _ := d.InsertStepResult(run.ID, types.StepPush)
	d.CompleteStep(completedStep.ID, 0, 100, "/tmp/log")
	pendingStep, _ := d.InsertStepResult(run.ID, types.StepPR)

	_, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}

	// Running, awaiting_approval, fixing should be failed.
	for _, tc := range []struct {
		id   string
		name string
		want types.StepStatus
	}{
		{runningStep.ID, "running", types.StepStatusFailed},
		{awaitingStep.ID, "awaiting", types.StepStatusFailed},
		{fixingStep.ID, "fixing", types.StepStatusFailed},
		{completedStep.ID, "completed", types.StepStatusCompleted},
		{pendingStep.ID, "pending", types.StepStatusPending},
	} {
		got, _ := d.GetStepResult(tc.id)
		if got.Status != tc.want {
			t.Errorf("step %s: status = %q, want %q", tc.name, got.Status, tc.want)
		}
	}
}

func TestRecoverStaleRunsNoStaleRuns(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project3", "git@github.com:user/project3.git", "main")

	// Only completed runs.
	run, _ := d.InsertRun(repo.ID, "feat", "abc", "def")
	d.UpdateRunStatus(run.ID, types.RunCompleted)

	count, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if count != 0 {
		t.Errorf("recovered count = %d, want 0", count)
	}
}

func TestSetRunCustodyReturnedCAS(t *testing.T) {
	t.Run("rejects missing publication evidence", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-no-evidence", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", strings.Repeat("a", 40), strings.Repeat("b", 40))
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		expected, _ := d.GetRun(run.ID)
		if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("custody CAS without evidence = %v, want ErrRunCustodyCAS", err)
		}
	})

	t.Run("stamps unchanged newest terminal run", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-success", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
		if err := d.UpdateRunHeadSHA(run.ID, "preserved"); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		seedTestPublicationEvidence(t, d, run)
		expected, _ := d.GetRun(run.ID)

		if err := d.SetRunCustodyReturnedCAS(expected); err != nil {
			t.Fatalf("custody CAS: %v", err)
		}
		got, _ := d.GetRun(run.ID)
		if got.CustodyReturnedAt == nil {
			t.Fatal("custody stamp missing after successful CAS")
		}
		if err := d.RollbackRunCustodyStamp(context.Background(), expected, ""); err != nil {
			t.Fatalf("rollback custody CAS: %v", err)
		}
		got, _ = d.GetRun(run.ID)
		if got.CustodyReturnedAt != nil {
			t.Fatal("custody stamp survived an authority rollback")
		}
	})

	t.Run("marks an unrollbackable stamp invalid", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-invalid", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
		if err := d.UpdateRunHeadSHA(run.ID, "preserved"); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		seedTestPublicationEvidence(t, d, run)
		expected, _ := d.GetRun(run.ID)
		if err := d.SetRunCustodyReturnedCAS(expected); err != nil {
			t.Fatal(err)
		}
		if err := d.MarkRunCustodyInvalid(context.Background(), run.ID, "", "gate authority was lost"); err != nil {
			t.Fatalf("mark custody invalid: %v", err)
		}
		got, _ := d.GetRun(run.ID)
		if got.CustodyReturnedAt != nil || got.CustodyTransitionPhase == nil || *got.CustodyTransitionPhase != CustodyPhaseInvalid {
			t.Fatalf("invalid custody state = %#v", got)
		}
		if err := d.ClearInvalidRunCustody(context.Background(), run.ID); err != nil {
			t.Fatalf("clear invalid custody: %v", err)
		}
		got, _ = d.GetRun(run.ID)
		if got.CustodyReturnedAt != nil || got.CustodyTransitionPhase != nil || got.Error != nil {
			t.Fatalf("reconciled custody state = %#v", got)
		}
	})

	t.Run("rejects active run", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-active", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
		if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		expected, _ := d.GetRun(run.ID)
		if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("active custody CAS error = %v", err)
		}
	})

	t.Run("rejects newer owner", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-newer", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		expected, _ := d.GetRun(run.ID)
		if _, err := d.InsertRun(repo.ID, "feat", "newer", "base"); err != nil {
			t.Fatal(err)
		}
		if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("newer-owner custody CAS error = %v", err)
		}
	})

	t.Run("rejects publication written after snapshot", func(t *testing.T) {
		d := openTestDB(t)
		repo, _ := d.InsertRepo("/home/user/custody-published", "git@github.com:user/custody.git", "main")
		run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
		if err := d.UpdateRunHeadSHA(run.ID, "preserved"); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
			t.Fatal(err)
		}
		expected, _ := d.GetRun(run.ID)
		if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "submitted", TargetKind: "upstream", TargetFingerprint: publicationTargetFingerprint(repo.PushURL()), Ref: "refs/heads/feat"}); err != nil {
			t.Fatal(err)
		}
		if err := d.SetRunCustodyReturnedCAS(expected); !errors.Is(err, ErrRunCustodyCAS) {
			t.Fatalf("publication-race custody CAS error = %v", err)
		}
	})
}

func TestRunCustodyTransitionOwnership(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/custody-transition", "git@github.com:user/custody.git", "main")
	run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
	if err := d.UpdateRunHeadSHA(run.ID, "preserved"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	seedTestPublicationEvidence(t, d, run)
	expected, _ := d.GetRun(run.ID)

	transition, err := d.BeginRunCustodyTransition(context.Background(), expected)
	if err != nil || transition == nil || transition.Token() == "" {
		t.Fatalf("begin custody transition = %#v, %v", transition, err)
	}
	if err := transition.Release(); err != nil {
		t.Fatalf("release custody transition: %v", err)
	}
	expected, _ = d.GetRun(run.ID)
	if expected.CustodyTransitionToken != nil {
		t.Fatal("released custody transition left an owner token")
	}

	transition, err = d.BeginRunCustodyTransition(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.Advance(context.Background(), CustodyPhasePreparing, CustodyPhaseStaged); err != nil {
		t.Fatalf("stage custody transition: %v", err)
	}
	if err := transition.Advance(context.Background(), CustodyPhaseStaged, CustodyPhaseGateMoved); err != nil {
		t.Fatalf("move custody transition: %v", err)
	}
	if err := transition.Complete(context.Background(), expected); err != nil {
		t.Fatalf("complete custody transition: %v", err)
	}
	if err := transition.Complete(context.Background(), expected); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("repeat complete custody transition = %v", err)
	}
	stamped, _ := d.GetRun(run.ID)
	if stamped.CustodyReturnedAt == nil || stamped.CustodyTransitionToken == nil {
		t.Fatalf("stamped custody transition lost its cleanup owner: %#v", stamped)
	}
	if err := transition.ReleaseStamped(context.Background(), run.ID); err != nil {
		t.Fatalf("release stamped custody transition: %v", err)
	}
	got, _ := d.GetRun(run.ID)
	if got.CustodyReturnedAt == nil || got.CustodyTransitionToken != nil {
		t.Fatalf("completed custody transition = %#v", got)
	}
}

func seedTestPublicationEvidence(t *testing.T, d *DB, run *Run) {
	t.Helper()
	targets, err := d.ListRunPublicationTargets(run.ID)
	if err != nil || len(targets) == 0 {
		t.Fatalf("publication targets = %#v, %v", targets, err)
	}
	inputs := make([]PublicationEvidenceInput, 0, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.RequestLineage) == "" || target.RequestLineage == PublicationTargetRequestLineageMigrationPending {
			if err := d.ReconcileRunPublicationTargetLineage(run.ID, target.TargetFingerprint, "none"); err != nil {
				t.Fatalf("reconcile test publication lineage: %v", err)
			}
		}
		inputs = append(inputs, PublicationEvidenceInput{
			TargetFingerprint: target.TargetFingerprint,
			Ref:               target.Ref,
			TargetVersion:     target.TargetVersion,
			RemoteHash:        "test-remote-evidence",
			ProviderHash:      "test-provider-evidence",
			Cursor:            "audit-cutoff=1754136000;provider-date:1754136000;audit;hasNextPage=false;test-complete-cursor",
			Since:             run.CreatedAt,
			Until:             run.UpdatedAt,
		})
	}
	if _, err := d.RecordRunPublicationEvidence(run.ID, inputs); err != nil {
		t.Fatalf("record test publication evidence: %v", err)
	}
}

func TestCustodyTransitionFencesPublicationWriters(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/custody-fence", "git@github.com:user/custody-fence.git", "main")
	run, _ := d.InsertRun(repo.ID, "feat", "submitted", "base")
	if err := d.UpdateRunHeadSHA(run.ID, "preserved"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	expected, _ := d.GetRun(run.ID)
	transition, err := d.BeginRunCustodyTransition(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunPushActive(run.ID, true); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("push entry during custody = %v", err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "preserved", TargetKind: "upstream", TargetFingerprint: publicationTargetFingerprint(repo.PushURL()), Ref: "refs/heads/feat"}); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("push binding during custody = %v", err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "late"); !errors.Is(err, ErrRunCustodyCAS) {
		t.Fatalf("head update during custody = %v", err)
	}
	unrelated, _ := d.InsertRun(repo.ID, "other", "other", "base")
	if err := d.SetRunPushActive(unrelated.ID, true); err != nil {
		t.Fatalf("unrelated run write blocked by custody: %v", err)
	}
	if err := transition.Release(); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetRun(run.ID); got.CustodyTransitionToken != nil || got.CustodyTransitionPhase != nil {
		t.Fatalf("released custody transition = %#v", got)
	}
}

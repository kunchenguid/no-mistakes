package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fakePRFactsReader struct {
	read            scm.PRFacts
	list            []scm.PRFacts
	readCalls       int
	listCalls       int
	requestedRepo   string
	requestedBranch string
	retargets       []string
}

func (f *fakePRFactsReader) ReadPRFacts(_ context.Context, _ *scm.PR) (scm.PRFacts, error) {
	f.readCalls++
	return f.read, nil
}

func (f *fakePRFactsReader) FindOpenPRFacts(_ context.Context, repo, branch string) ([]scm.PRFacts, error) {
	f.listCalls++
	f.requestedRepo, f.requestedBranch = repo, branch
	return f.list, nil
}

func (f *fakePRFactsReader) SetPRBaseBranch(_ context.Context, _ *scm.PR, branch string) error {
	f.retargets = append(f.retargets, branch)
	f.read.BaseBranch = branch
	return nil
}

func selectionFixture(t *testing.T) (*pipeline.StepContext, scm.PRFacts) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Config.PR.BaseBranch = "configured"
	sctx.Run.PRBaseBranch = strptr("stale")
	return sctx, scm.PRFacts{
		PR:    scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"},
		State: scm.PRStateOpen, SourceRepository: "test/repo", SourceBranch: "feature",
		HeadSHA: head, BaseBranch: "develop",
	}
}

func TestResolvePRTarget_RecordedUsesExactLivePRBase(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	reader := &fakePRFactsReader{read: facts}
	got, err := resolvePRTargetWithReader(sctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != facts.PR.URL || got.TargetBranch != "develop" || got.ForgeHeadSHA != facts.HeadSHA || got.SourceRepo != "test/repo" || got.SourceBranch != "feature" {
		t.Fatalf("selection = %+v", got)
	}
	if reader.readCalls != 1 || reader.listCalls != 0 {
		t.Fatalf("read/list calls = %d/%d", reader.readCalls, reader.listCalls)
	}
}

func TestResolvePRTarget_GitHubSourceRepositoryCaseDoesNotChangeIdentity(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	facts.SourceRepository = "Test/Repo"
	got, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{read: facts})
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != facts.PR.URL || got.SourceRepo != "test/repo" {
		t.Fatalf("selection = %+v", got)
	}
}

func TestResolvePRTarget_DiscoversUnrecordedExactHead(t *testing.T) {
	sctx, facts := selectionFixture(t)
	reader := &fakePRFactsReader{list: []scm.PRFacts{facts}}
	got, err := resolvePRTargetWithReader(sctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != facts.PR.URL || got.TargetBranch != "develop" {
		t.Fatalf("selection = %+v", got)
	}
	if sctx.Run.PRURL != nil {
		t.Fatal("discovery persisted or mutated the run PR URL")
	}
	if reader.requestedRepo != "test/repo" || reader.requestedBranch != "feature" {
		t.Fatalf("discovery query = %q/%q", reader.requestedRepo, reader.requestedBranch)
	}
}

func TestResolvePRTarget_AmbiguousDiscoveryFails(t *testing.T) {
	sctx, facts := selectionFixture(t)
	second := facts
	second.PR.URL = "https://github.com/test/repo/pull/43"
	second.PR.Number = "43"
	_, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{list: []scm.PRFacts{facts, second}})
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolvePRTarget_RejectsForeignAndClosedRecordedPR(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*scm.PRFacts)
	}{
		{"foreign repository", func(f *scm.PRFacts) { f.SourceRepository = "other/repo" }},
		{"foreign branch", func(f *scm.PRFacts) { f.SourceBranch = "other" }},
		{"closed", func(f *scm.PRFacts) { f.State = scm.PRStateClosed }},
	} {
		t.Run(change.name, func(t *testing.T) {
			sctx, facts := selectionFixture(t)
			sctx.Run.PRURL = &facts.PR.URL
			change.apply(&facts)
			if _, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{read: facts}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestResolvePRTarget_UsesLiveBaseBeforeExistingPRHeadIsPushed(t *testing.T) {
	sctx, facts := selectionFixture(t)
	facts.HeadSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD^")
	got, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{list: []scm.PRFacts{facts}})
	if err != nil || got.TargetBranch != "develop" || got.PRURL != "" {
		t.Fatalf("prospective comparison = %+v, %v", got, err)
	}
}

func TestResolvePRTarget_RejectsDivergentDiscoveredPRHead(t *testing.T) {
	sctx, facts := selectionFixture(t)
	facts.HeadSHA = strings.Repeat("0", 40)
	_, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{list: []scm.PRFacts{facts}})
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("divergent PR head error = %v", err)
	}
}

func TestResolvePRTarget_RejectsDivergentRecordedPRHead(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	facts.HeadSHA = strings.Repeat("0", 40)
	_, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{read: facts})
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("divergent recorded PR head error = %v", err)
	}
}

func TestResolvePRTarget_NoPRUsesProspectiveTarget(t *testing.T) {
	sctx, _ := selectionFixture(t)
	got, err := resolvePRTargetWithReader(sctx, &fakePRFactsReader{})
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != "" || got.TargetBranch != "stale" {
		t.Fatalf("selection = %+v", got)
	}
}

func TestResolvePRTarget_AzureSourceIdentityIsCanonicalAndCredentialFree(t *testing.T) {
	sctx, _ := selectionFixture(t)
	sctx.Repo.UpstreamURL = "https://user:secret@dev.azure.com/example/project/_git/repo"
	reader := &fakePRFactsReader{}
	got, err := resolvePRTargetWithReader(sctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://dev.azure.com/example/project/_git/repo"
	if got.SourceRepo != want || reader.requestedRepo != want {
		t.Fatalf("Azure source identity = %q, query = %q, want %q", got.SourceRepo, reader.requestedRepo, want)
	}
}

func TestCurrentPRTargetBranch_UsesPinnedSelection(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	sctx.PRTarget = &pipeline.PRTargetSelection{PRURL: facts.PR.URL, TargetBranch: "live-target"}
	got, err := currentPRTargetBranch(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "live-target" {
		t.Fatalf("target = %q, want live-target", got)
	}
}

func TestResolvePRTarget_LocalRepositoryUsesProspectiveTarget(t *testing.T) {
	sctx, _ := selectionFixture(t)
	sctx.Repo.UpstreamURL = "file:///tmp/no-mistakes-local.git"
	sctx.Run.PRURL = nil
	sctx.Run.PRBaseBranch = nil
	sctx.Config.PR.BaseBranch = "develop"

	selected, err := ResolvePRTarget(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if selected.PRURL != "" || selected.TargetBranch != "develop" {
		t.Fatalf("local target = %+v, want prospective develop without a PR", selected)
	}
	ensureLocalBranch(t, sctx.WorkDir, "develop", sctx.Run.BaseSHA)
	decision, err := GuardPRContext(sctx, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Target.TargetBranch != "develop" {
		t.Fatalf("guarded local target = %+v, want develop", decision.Target)
	}
}

func TestFreshOwnedPRRetargetIsReadBackAndConsumed(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	sctx.Run.PRBaseBranch = strptr("release")
	sctx.Run.PRBaseBranchRequested = true
	reader := &fakePRFactsReader{read: facts}
	selected, err := resolveAndApplyPRTarget(sctx, reader, reader)
	if err != nil {
		t.Fatal(err)
	}
	if selected.TargetBranch != "release" || len(reader.retargets) != 1 || reader.retargets[0] != "release" || sctx.Run.PRBaseBranchRequested {
		t.Fatalf("selection=%+v retargets=%v requested=%v", selected, reader.retargets, sctx.Run.PRBaseBranchRequested)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || stored.PRBaseBranchRequested {
		t.Fatalf("retarget request remained: run=%+v err=%v", stored, err)
	}
}

func TestFreshRetargetWaitsForUnpublishedPRToAttach(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRBaseBranch = strptr("release")
	sctx.Run.PRBaseBranchRequested = true
	facts.HeadSHA = sctx.Run.BaseSHA
	reader := &fakePRFactsReader{list: []scm.PRFacts{facts}}

	selection, err := resolveAndApplyPRTarget(sctx, reader, reader)
	if err != nil {
		t.Fatal(err)
	}
	if selection.PRURL != "" || selection.TargetBranch != "develop" || len(reader.retargets) != 0 || !sctx.Run.PRBaseBranchRequested {
		t.Fatalf("unpublished selection=%+v retargets=%v requested=%v", selection, reader.retargets, sctx.Run.PRBaseBranchRequested)
	}

	sctx.Run.PRURL = &facts.PR.URL
	facts.HeadSHA = sctx.Run.HeadSHA
	reader.read = facts
	selection, err = resolveAndApplyPRTarget(sctx, reader, reader)
	if err != nil {
		t.Fatal(err)
	}
	if selection.TargetBranch != "release" || len(reader.retargets) != 1 || reader.retargets[0] != "release" || sctx.Run.PRBaseBranchRequested {
		t.Fatalf("attached selection=%+v retargets=%v requested=%v", selection, reader.retargets, sctx.Run.PRBaseBranchRequested)
	}
}

func TestInheritedOrUnownedPRBaseNeverRetargets(t *testing.T) {
	for _, owned := range []bool{false, true} {
		sctx, facts := selectionFixture(t)
		sctx.Run.PRBaseBranch = strptr("release")
		sctx.Run.PRBaseBranchRequested = !owned
		reader := &fakePRFactsReader{read: facts, list: []scm.PRFacts{facts}}
		if owned {
			sctx.Run.PRURL = &facts.PR.URL
		}
		selected, err := resolveAndApplyPRTarget(sctx, reader, reader)
		if err != nil {
			t.Fatal(err)
		}
		if selected.TargetBranch != "develop" || len(reader.retargets) != 0 {
			t.Fatalf("owned=%v selection=%+v retargets=%v", owned, selected, reader.retargets)
		}
	}
}

func TestFreshRetargetRejectsMovedHeadAndUnsupportedProvider(t *testing.T) {
	for _, tc := range []struct {
		name       string
		moveHead   bool
		retargeter bool
	}{
		{name: "head moved", moveHead: true, retargeter: true},
		{name: "provider cannot retarget", retargeter: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, facts := selectionFixture(t)
			sctx.Run.PRURL = &facts.PR.URL
			sctx.Run.PRBaseBranch = strptr("release")
			sctx.Run.PRBaseBranchRequested = true
			if tc.moveHead {
				facts.HeadSHA = strings.Repeat("9", 40)
			}
			reader := &fakePRFactsReader{read: facts}
			var retargeter scm.PRBaseRetargeter
			if tc.retargeter {
				retargeter = reader
			}
			if _, err := resolveAndApplyPRTarget(sctx, reader, retargeter); err == nil {
				t.Fatal("expected fresh retarget to fail closed")
			}
			if len(reader.retargets) != 0 || !sctx.Run.PRBaseBranchRequested {
				t.Fatalf("unsafe mutation or consumed request: retargets=%v requested=%v", reader.retargets, sctx.Run.PRBaseBranchRequested)
			}
		})
	}
}

package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type mutationComparisonHost struct {
	scm.Host
	facts  scm.PRFacts
	body   string
	writes int
	reads  int
	onRead func(*mutationComparisonHost)
	err    error
}

func (h *mutationComparisonHost) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *mutationComparisonHost) ReadPRFacts(context.Context, *scm.PR) (scm.PRFacts, error) {
	return h.facts, h.err
}

func (h *mutationComparisonHost) FindOpenPRFacts(context.Context, string, string) ([]scm.PRFacts, error) {
	return nil, nil
}

func (h *mutationComparisonHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	h.reads++
	if h.onRead != nil {
		h.onRead(h)
	}
	return scm.PRContent{Body: h.body}, nil
}

func (h *mutationComparisonHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.writes++
	h.body = content.Body
	return pr, nil
}

func mutationComparisonFixture(t *testing.T) (*pipeline.StepContext, *mutationComparisonHost, *scm.PR) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	url := "https://github.com/test/repo/pull/42"
	selection := pipeline.PRTargetSelection{PRURL: url, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRURL = &url
	pr := prFromOwnedURL(url)
	host := &mutationComparisonHost{facts: scm.PRFacts{PR: *pr, State: scm.PRStateOpen, SourceRepository: "test/repo", SourceBranch: "feature", HeadSHA: head, BaseBranch: "main"}}
	return sctx, host, pr
}

func TestCIMonitorReadinessRequiresFreshExactComparison(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *pipeline.StepContext, *mutationComparisonHost)
	}{
		{"retargeted", func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) { h.facts.BaseBranch = "develop" }},
		{"source moved", func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) {
			h.facts.HeadSHA = strings.Repeat("a", 40)
		}},
		{"unreadable", func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) {
			h.err = errors.New("provider unavailable")
		}},
		{"target advanced", func(t *testing.T, sctx *pipeline.StepContext, _ *mutationComparisonHost) {
			gitCmd(t, sctx.WorkDir, "checkout", "main")
			gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "advance target")
			gitCmd(t, sctx.WorkDir, "checkout", "feature")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, host, _ := mutationComparisonFixture(t)
			sctx.Log = func(string) {}
			checks := []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, HeadSHA: sctx.Run.HeadSHA}}
			if got := logVerifiedCIMonitorStatus(sctx, host, checks, ciChecksPassedMsg, ""); got != ciChecksPassedMsg {
				t.Fatalf("current comparison did not publish readiness: %q", got)
			}
			current, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || current.CIReadyAt == nil {
				t.Fatalf("current comparison readiness = %+v, %v", current, err)
			}
			tc.mutate(t, sctx, host)
			if got := logVerifiedCIMonitorStatus(sctx, host, checks, ciChecksPassedMsg, ciChecksPassedMsg); got != "" {
				t.Fatalf("stale comparison retained green monitor state: %q", got)
			}
			current, err = sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || current.CIReadyAt != nil {
				t.Fatalf("stale comparison kept readiness = %+v, %v", current, err)
			}
		})
	}
}

func TestCIMonitorReadinessRequiresChecksOnReceiptHead(t *testing.T) {
	for _, checkedHead := range []string{"", strings.Repeat("a", 40)} {
		sctx, host, _ := mutationComparisonFixture(t)
		checks := []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass, HeadSHA: sctx.Run.HeadSHA}}
		if got := logVerifiedCIMonitorStatus(sctx, host, checks, ciChecksPassedMsg, ""); got != ciChecksPassedMsg {
			t.Fatalf("current check did not publish readiness: %q", got)
		}
		checks[0].HeadSHA = checkedHead
		if got := logVerifiedCIMonitorStatus(sctx, host, checks, ciChecksPassedMsg, ciChecksPassedMsg); got != "" {
			t.Fatalf("check on %q retained readiness: %q", checkedHead, got)
		}
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || run.CIReadyAt != nil {
			t.Fatalf("stale check retained durable readiness: %+v, %v", run, err)
		}
	}
}

func TestPRMutationComparisonRejectsChangedLiveEvidence(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*mutationComparisonHost)
	}{
		{"retarget", func(h *mutationComparisonHost) { h.facts.BaseBranch = "develop" }},
		{"source head", func(h *mutationComparisonHost) { h.facts.HeadSHA = strings.Repeat("a", 40) }},
		{"source repository", func(h *mutationComparisonHost) { h.facts.SourceRepository = "other/repo" }},
		{"closed", func(h *mutationComparisonHost) { h.facts.State = scm.PRStateClosed }},
		{"unavailable", func(h *mutationComparisonHost) { h.err = errors.New("unavailable") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			sctx, host, pr := mutationComparisonFixture(t)
			content, appendix := ownedFixture(t)
			host.body = content.Body
			host.onRead = func(h *mutationComparisonHost) { change.mutate(h) }
			err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
				return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
			})
			if err == nil || host.writes != 0 {
				t.Fatalf("stale PR update: err=%v writes=%d", err, host.writes)
			}
		})
	}
}

func TestPRMutationComparisonAcceptsCurrentOwnedAndProposedAttestation(t *testing.T) {
	sctx, host, pr := mutationComparisonFixture(t)
	content, appendix := ownedFixture(t)
	host.body = content.Body
	if err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
		return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
	}); err != nil || host.writes != 1 {
		t.Fatalf("current owned update: %v, writes=%d", err, host.writes)
	}
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "repair")
	proposed := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if err := updateOwnedPR(sctx, host, pr, scm.PRContent{Body: host.body}, "", "", appendix+"\nAnother fact", 0, func() error {
		return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
	}); err == nil || host.writes != 1 {
		t.Fatalf("ordinary PR update accepted moved worktree head: %v, writes=%d", err, host.writes)
	}
	host.body = compliantPipelineBody(t, host.facts.HeadSHA)
	if err := restampPRAttestationWithSteps(sctx.Ctx, host, pr, proposed, nil, nil, pipelineAttestationPolicy{}, func() error {
		return verifyPRMutationComparison(sctx, host, pr, proposed, true)
	}); err != nil || host.writes != 2 {
		t.Fatalf("proposed head restamp: %v, writes=%d", err, host.writes)
	}
}

func TestPRMutationComparisonRejectsDirtyOrMovedRunBeforeProposedWrite(t *testing.T) {
	for _, mode := range []string{"dirty worktree", "durable run moved"} {
		t.Run(mode, func(t *testing.T) {
			sctx, host, pr := mutationComparisonFixture(t)
			gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "proposed")
			proposed := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
			switch mode {
			case "dirty worktree":
				if err := os.WriteFile(filepath.Join(sctx.WorkDir, "uncommitted.txt"), []byte("dirty"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "durable run moved":
				if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, proposed); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyPRMutationComparison(sctx, host, pr, proposed, true); err == nil {
				t.Fatalf("accepted %s before proposed attestation", mode)
			}
		})
	}
}

func bindPublicationComparison(t *testing.T, sctx *pipeline.StepContext, head string) {
	t.Helper()
	selection := pipeline.PRTargetSelection{
		PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo",
		SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main",
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}
}

func publicationAttestationFixture(t *testing.T) (*ciRepairFixture, string) {
	t.Helper()
	f := newCIRepairFixture(t, false, writeCIFix)
	bindPublicationComparison(t, f.sctx, f.headSHA)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, f.headSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	f.sctx.Env = append(f.sctx.Env,
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_LOG="+logFile,
	)
	return f, logFile
}

func TestPushStep_AttestsCommittedDescendantWithRecordedHeadUnadvanced(t *testing.T) {
	f, logFile := publicationAttestationFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "agent-change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PushStep{}).Execute(f.sctx); err != nil {
		t.Fatalf("push committed descendant: %v; log: %s", err, f.log())
	}
	proposed := f.localHead(t)
	if proposed == f.headSHA || f.remoteHead(t) != proposed {
		t.Fatalf("push did not publish descendant: old=%s proposed=%s remote=%s", f.headSHA, proposed, f.remoteHead(t))
	}
	if attestation := parsePipelineAttestationForTest(t, readFakeGHBodyArg(t, logFile)); attestation.HeadSHA != proposed {
		t.Fatalf("attestation head = %s, want %s", attestation.HeadSHA, proposed)
	}
	stored, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil || stored.HeadSHA != proposed {
		t.Fatalf("published run head = %+v, %v", stored, err)
	}
}

func TestCIRepair_AttestsCommittedDescendantWithRecordedHeadUnadvanced(t *testing.T) {
	f, logFile := publicationAttestationFixture(t)
	writeCIFix(f.dir)
	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil || !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("publish CI repair = %+v, %v; log: %s", repair, err, f.log())
	}
	proposed := f.localHead(t)
	if proposed == f.headSHA || f.remoteHead(t) != proposed {
		t.Fatalf("repair did not publish descendant: old=%s proposed=%s remote=%s", f.headSHA, proposed, f.remoteHead(t))
	}
	if attestation := parsePipelineAttestationForTest(t, readFakeGHBodyArg(t, logFile)); attestation.HeadSHA != proposed {
		t.Fatalf("attestation head = %s, want %s", attestation.HeadSHA, proposed)
	}
	stored, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil || stored.HeadSHA != proposed {
		t.Fatalf("published run head = %+v, %v", stored, err)
	}
}

func TestPushStep_ChangedPRComparisonRefusesAttestationAndPush(t *testing.T) {
	for _, change := range []struct {
		name       string
		facts      func(string) string
		moveTarget bool
	}{
		{name: "retarget", facts: func(head string) string { return mutationPRFactsJSON("open", head, "develop") }},
		{name: "source moved", facts: func(head string) string { return mutationPRFactsJSON("open", strings.Repeat("a", 40), "main") }},
		{name: "closed", facts: func(head string) string { return mutationPRFactsJSON("closed", head, "main") }},
		{name: "unreadable", facts: func(string) string { return "{" }},
		{name: "target advanced", moveTarget: true},
	} {
		t.Run(change.name, func(t *testing.T) {
			f, logFile := publicationAttestationFixture(t)
			if change.facts != nil {
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_FACTS_JSON="+change.facts(f.headSHA))
			}
			if change.moveTarget {
				gitCmd(t, f.dir, "checkout", "main")
				gitCmd(t, f.dir, "commit", "--allow-empty", "-m", "advance target")
				gitCmd(t, f.dir, "push", "origin", "main")
				gitCmd(t, f.dir, "checkout", "feature")
			}
			if err := os.WriteFile(filepath.Join(f.dir, "agent-change.txt"), []byte("change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := (&PushStep{}).Execute(f.sctx); err == nil {
				t.Fatal("accepted changed PR comparison before push")
			}
			if remote := f.remoteHead(t); remote != f.headSHA {
				t.Fatalf("pushed before comparison refusal: %s", remote)
			}
			log, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), "pr edit") {
				t.Fatalf("mutated PR before comparison refusal: %s", log)
			}
		})
	}
}

func mutationPRFactsJSON(state, head, base string) string {
	return `{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"` + state + `","merged":false,"head":{"ref":"feature","sha":"` + head + `","repo":{"full_name":"test/repo"}},"base":{"ref":"` + base + `"}}`
}

func TestPRMutationComparisonRejectsTargetAdvanceDuringDraft(t *testing.T) {
	sctx, host, pr := mutationComparisonFixture(t)
	content, appendix := ownedFixture(t)
	host.body = content.Body
	host.onRead = func(h *mutationComparisonHost) {
		if h.reads != 1 {
			return
		}
		gitCmd(t, sctx.WorkDir, "checkout", "main")
		gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "advance target")
		gitCmd(t, sctx.WorkDir, "checkout", "feature")
	}
	err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
		return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
	})
	if err == nil || host.writes != 0 {
		t.Fatalf("target advance was published: err=%v writes=%d", err, host.writes)
	}
}

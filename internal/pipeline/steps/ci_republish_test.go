package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type ciRepublishAgent struct {
	fix            func(string) error
	verify         func(string) error
	fixerCalls     int
	verifierCalls  int
	verifierOutput string
}

func (a *ciRepublishAgent) Name() string { return "ci-republish-test" }

func (a *ciRepublishAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if strings.Contains(opts.Prompt, "independently verifying a CI-repair patch") {
		a.verifierCalls++
		if a.verify != nil {
			if err := a.verify(opts.CWD); err != nil {
				return nil, err
			}
		}
		output := a.verifierOutput
		if output == "" {
			output = `{"findings":[],"summary":"candidate verified"}`
		}
		return &agent.Result{Output: json.RawMessage(output)}, nil
	}
	a.fixerCalls++
	if a.fix != nil {
		if err := a.fix(opts.CWD); err != nil {
			return nil, err
		}
	}
	return &agent.Result{}, nil
}

func TestCIStep_AutoFixRejectsVerifierHeadMutation(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	ag := &ciRepublishAgent{
		fix: func(cwd string) error {
			return os.WriteFile(filepath.Join(cwd, "ci-fix.txt"), []byte("fixed\n"), 0o644)
		},
		verify: func(cwd string) error {
			gitCmd(t, cwd, "commit", "-m", "verifier mutation")
			return nil
		},
	}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})

	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err == nil || !strings.Contains(err.Error(), "candidate HEAD changed") {
		t.Fatalf("autoFixCI error = %v, want verifier HEAD mutation rejection", err)
	}
	if pushed {
		t.Fatal("verifier-mutated candidate was republished")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != originalHead {
		t.Fatalf("remote SHA = %s, want original %s", got, originalHead)
	}
}

func (a *ciRepublishAgent) Close() error { return nil }

func setupCIRepublish(t *testing.T) (upstream, dir, baseSHA, headSHA string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")
	return upstream, dir, baseSHA, headSHA
}

func republishContext(t *testing.T, ag agent.Agent, upstream, dir, baseSHA, headSHA string, commands config.Commands) (*CIStep, *pipeline.StepContext, scm.Host, *scm.PR) {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, commands)
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Env = fakeCIGH(t, "OPEN", `[]`)
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	return &CIStep{}, sctx, host, &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
}

// TestCIStep_VerifyCIPatchGatesRejection proves the CI patch verifier fails
// closed on a blocking verdict and passes on a clean one, before any commit.
func TestCIStep_VerifyCIPatchGatesRejection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reject := &mockAgent{
		name:             "test",
		ciVerifierOutput: `{"findings":[{"severity":"error","file":"main.go","description":"regression","action":"auto-fix"}],"summary":"unsafe CI patch"}`,
	}
	sctx := newTestContextWithDBRecords(t, reject, dir, "base", "head", config.Commands{})
	step := &CIStep{}
	if err := step.verifyCIPatch(sctx, "base"); err == nil {
		t.Fatal("expected the CI patch verifier to fail closed on a blocking verdict")
	}
	clean := &mockAgent{name: "test"}
	sctxClean := newTestContextWithDBRecords(t, clean, dir, "base", "head", config.Commands{})
	if err := step.verifyCIPatch(sctxClean, "base"); err != nil {
		t.Fatalf("expected a clean verifier verdict to pass, got %v", err)
	}
	inconclusive := &mockAgent{name: "test", ciVerifierOutput: `{"findings":[]}`}
	sctxInconclusive := newTestContextWithDBRecords(t, inconclusive, dir, "base", "head", config.Commands{})
	if err := step.verifyCIPatch(sctxInconclusive, "base"); err == nil {
		t.Fatal("expected a verifier verdict without an explicit summary to fail closed")
	}
}

func TestCIStep_AutoFixVerifiesCleanChangedHeadBeforeRepublish(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "base-update.txt"), []byte("new base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "advance base")
	baseTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	checkMarker := filepath.Join(t.TempDir(), "checked")
	ag := &ciRepublishAgent{fix: func(cwd string) error {
		cmd := exec.Command("git", "rebase", baseTip)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
		return cmd.Run()
	}}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{
		Test: "printf checked > " + checkMarker,
	})
	sctx.Repo.DefaultBranch = "main"

	pushed, err := step.autoFixCI(sctx, host, pr, nil, true)
	if err != nil {
		t.Fatalf("autoFixCI: %v", err)
	}
	if !pushed {
		t.Fatal("expected rebased candidate to be republished")
	}
	if _, err := os.Stat(checkMarker); err != nil {
		t.Fatalf("deterministic check did not run for clean changed HEAD: %v", err)
	}
	if ag.verifierCalls != 1 {
		t.Fatalf("strong verifier calls = %d, want 1", ag.verifierCalls)
	}
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if newHead == originalHead {
		t.Fatal("rebase did not change HEAD")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != newHead {
		t.Fatalf("remote SHA = %s, want verified rebased SHA %s", got, newHead)
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil || seal.SHA != newHead {
		t.Fatalf("ci_republish seal = %+v, want exact pushed SHA %s", seal, newHead)
	}
}

func TestCIStep_AutoFixSealsBeforeRemoteChanges(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	ag := &ciRepublishAgent{fix: func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, "ci-fix.txt"), []byte("fixed\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})
	step.sealCIRepublish = func(*pipeline.StepContext, string) error {
		return nil
	}

	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want durable seal failure", pushed)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != originalHead {
		t.Fatalf("remote moved to %s before durable seal succeeded; want %s", got, originalHead)
	}
	if seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish"); err != nil {
		t.Fatal(err)
	} else if seal != nil {
		t.Fatalf("injected seal failure still recorded %+v", seal)
	}
}

func TestCIStep_AutoFixUpToDateCandidateEnsuresSeal(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	if err := os.WriteFile(filepath.Join(dir, "already-pushed.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "already pushed candidate")
	candidateSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	ag := &ciRepublishAgent{}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})
	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Fatal("up-to-date candidate should not report a transport")
	}
	if ag.verifierCalls != 1 {
		t.Fatalf("strong verifier calls = %d, want 1 for changed HEAD", ag.verifierCalls)
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil || seal.SHA != candidateSHA {
		t.Fatalf("ci_republish seal = %+v, want up-to-date candidate %s", seal, candidateSHA)
	}
	firstSealID := seal.ID
	if pushed, err := step.pushUpdatedHeadSHA(sctx, candidateSHA); err != nil {
		t.Fatalf("idempotent up-to-date reconciliation: %v", err)
	} else if pushed {
		t.Fatal("idempotent up-to-date reconciliation unexpectedly transported")
	}
	seal, err = sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil || seal.ID != firstSealID {
		t.Fatalf("idempotent seal changed from %s to %+v", firstSealID, seal)
	}
}

func TestCIStep_MergeConflictRepairMustIncorporateBase(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "base-update.txt"), []byte("new base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "advance base")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &ciRepublishAgent{fix: func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, "unrelated.txt"), []byte("not a rebase\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})
	sctx.Repo.DefaultBranch = "main"
	if pushed, err := step.autoFixCI(sctx, host, pr, nil, true); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want conflict-state validation failure", pushed)
	}
	if ag.verifierCalls != 0 {
		t.Fatalf("strong verifier ran %d times before conflict-state validation", ag.verifierCalls)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != originalHead {
		t.Fatalf("remote changed to %s after invalid conflict repair; want %s", got, originalHead)
	}
}

type ciPurposeRecordingInvoker struct {
	cwd      string
	purposes []types.Purpose
}

func (i *ciPurposeRecordingInvoker) Invoke(_ context.Context, req agent.InvocationRequest) (*agent.Result, error) {
	i.purposes = append(i.purposes, req.Purpose)
	switch req.Purpose {
	case types.PurposeUnstructuredCIRepair:
		if err := os.WriteFile(filepath.Join(i.cwd, "journaled-ci-fix.txt"), []byte("fixed\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	case types.PurposeEscalatedAggregateVerification:
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"CI candidate independently verified"}`)}, nil
	default:
		return nil, fmt.Errorf("unexpected CI republish purpose %q", req.Purpose)
	}
}

func TestCIStep_AutoFixUsesDedicatedVerifierWithoutOrdinaryVerifyReentry(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	step, sctx, host, pr := republishContext(t, &mockAgent{name: "unused"}, upstream, dir, baseSHA, originalHead, config.Commands{})
	invoker := &ciPurposeRecordingInvoker{cwd: dir}
	sctx.Invoker = invoker

	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected CI repair to be republished")
	}
	if len(invoker.purposes) != 2 {
		t.Fatalf("CI republish purposes = %v, want repair plus dedicated verifier", invoker.purposes)
	}
	if invoker.purposes[0] != types.PurposeUnstructuredCIRepair || invoker.purposes[1] != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("CI republish purposes = %v, want repair then escalated verifier", invoker.purposes)
	}
	for _, purpose := range invoker.purposes {
		if purpose == types.PurposeNormalAggregateVerification {
			t.Fatalf("CI republish re-entered ordinary Verify purpose: %v", invoker.purposes)
		}
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if seal == nil || seal.SHA != remoteSHA {
		t.Fatalf("seal = %+v, remote SHA = %s; want exact dedicated-verifier candidate", seal, remoteSHA)
	}
}

func TestCIStep_FailedPushRetainsDurableVerifiedSealForRetry(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	ag := &ciRepublishAgent{fix: func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, "retryable-ci-fix.txt"), []byte("fixed\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})
	hook := filepath.Join(upstream, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want rejected transport", pushed)
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil {
		t.Fatal("failed push lost the verified CI republish seal")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != originalHead {
		t.Fatalf("rejected push moved remote to %s; want %s", got, originalHead)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	pushed, err := (&CIStep{}).pushUpdatedHeadSHA(sctx, seal.SHA)
	if err != nil {
		t.Fatalf("retry sealed candidate: %v", err)
	}
	if !pushed {
		t.Fatal("retry did not transport the sealed candidate")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != seal.SHA {
		t.Fatalf("retried remote SHA = %s, want sealed SHA %s", got, seal.SHA)
	}
	retriedSeal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if retriedSeal == nil || retriedSeal.ID != seal.ID {
		t.Fatalf("retry replaced durable seal %+v with %+v", seal, retriedSeal)
	}
}

func TestCIStep_TransportsExactSealedSHAWhenHEADMovesAfterSeal(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	ag := &ciRepublishAgent{fix: func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, "sealed-ci-fix.txt"), []byte("fixed\n"), 0o644)
	}}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})
	var sealedSHA, movedHeadSHA string
	step.sealCIRepublish = func(sctx *pipeline.StepContext, sha string) error {
		if err := ensureCIRepublishSeal(sctx, sha); err != nil {
			return err
		}
		sealedSHA = sha
		if err := os.WriteFile(filepath.Join(sctx.WorkDir, "after-seal.txt"), []byte("must not publish\n"), 0o644); err != nil {
			return err
		}
		gitCmd(t, sctx.WorkDir, "add", "after-seal.txt")
		gitCmd(t, sctx.WorkDir, "commit", "-m", "move HEAD after seal")
		movedHeadSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
		return nil
	}

	pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected exact sealed candidate transport")
	}
	if sealedSHA == "" || movedHeadSHA == "" || sealedSHA == movedHeadSHA {
		t.Fatalf("test race was not established: sealed=%s moved=%s", sealedSHA, movedHeadSHA)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != sealedSHA {
		t.Fatalf("remote transported %s, want exact sealed SHA %s (moved HEAD was %s)", got, sealedSHA, movedHeadSHA)
	}
}

func TestCIStep_RejectedCleanChangedHeadRestoresRecordedCandidate(t *testing.T) {
	upstream, dir, baseSHA, originalHead := setupCIRepublish(t)
	ag := &ciRepublishAgent{
		fix: func(cwd string) error {
			if err := os.WriteFile(filepath.Join(cwd, "agent-commit.txt"), []byte("unverified\n"), 0o644); err != nil {
				return err
			}
			cmd := exec.Command("git", "add", "agent-commit.txt")
			cmd.Dir = cwd
			if err := cmd.Run(); err != nil {
				return err
			}
			cmd = exec.Command("git", "commit", "-m", "agent clean-head candidate")
			cmd.Dir = cwd
			return cmd.Run()
		},
		verifierOutput: `{"findings":[{"severity":"error","description":"unsafe candidate","action":"auto-fix"}],"summary":"rejected"}`,
	}
	step, sctx, host, pr := republishContext(t, ag, upstream, dir, baseSHA, originalHead, config.Commands{})

	if pushed, err := step.autoFixCI(sctx, host, pr, []string{"test"}, false); err == nil {
		t.Fatalf("autoFixCI = pushed %v, nil error; want verifier rejection", pushed)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != originalHead {
		t.Fatalf("rejected clean changed HEAD remained at %s; want recorded candidate %s", got, originalHead)
	}
	if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("rejected clean changed candidate left worktree changes: %q", got)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != originalHead {
		t.Fatalf("rejected candidate moved remote to %s; want %s", got, originalHead)
	}
}

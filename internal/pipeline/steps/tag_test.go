package steps

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

const (
	tagRunSuccess = `[{"id":7,"workflow_id":3,"name":"release","status":"completed","conclusion":"success","html_url":"https://github.com/up/repo/actions/runs/7","path":".github/workflows/release.yml"}]`
	tagRunFailure = `[{"id":7,"workflow_id":3,"name":"release","status":"completed","conclusion":"failure","html_url":"https://github.com/up/repo/actions/runs/7","path":".github/workflows/release.yml"}]`
	tagRunPending = `[{"id":7,"workflow_id":3,"name":"release","status":"in_progress","conclusion":"","html_url":"https://github.com/up/repo/actions/runs/7","path":".github/workflows/release.yml"}]`
	// tagOnlyWorkflow mirrors the real skills release.yml trigger.
	tagOnlyWorkflow = "name: Release\non:\n  push:\n    tags: [\"v*.*.*\"]\n"
)

// tagGH links the portable compiled fake gh (gh.exe on Windows) in ci-gh mode.
func tagGH(t *testing.T, vars map[string]string) (env []string, logFile string) {
	t.Helper()
	bin := stepstest.FakeCLIBinDir(t)
	stepstest.LinkFakeCLI(t, bin, "gh")
	logFile = t.TempDir() + string(os.PathSeparator) + "gh.log"
	all := map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_LOG": logFile, "FAKE_CLI_WORKFLOW_FILE": tagOnlyWorkflow}
	for k, v := range vars {
		all[k] = v
	}
	return stepstest.FakeCLIEnv(bin, all), logFile
}

type tagRepo struct {
	sctx     *pipeline.StepContext
	dir      string
	upstream string
	fork     string
}

// newTagRepo makes a disposable clone whose GitHub-shaped upstream (and fork)
// URLs are rewritten by git to local bare repositories, so pushes and
// ls-remote are real while gh sees github.com slugs.
func newTagRepo(t *testing.T, annotated bool, fork bool) *tagRepo {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	r := &tagRepo{dir: dir, upstream: t.TempDir()}
	gitCmd(t, r.upstream, "init", "--bare", "-q")
	gitCmd(t, dir, "remote", "add", "origin", "https://github.com/up/repo")
	gitCmd(t, dir, "config", "url."+r.upstream+".insteadOf", "https://github.com/up/repo")
	if annotated {
		gitCmd(t, dir, "tag", "-a", "v1", "-m", "release notes", headSHA)
	} else {
		gitCmd(t, dir, "tag", "v1", headSHA)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/up/repo"
	if fork {
		r.fork = t.TempDir()
		gitCmd(t, r.fork, "init", "--bare", "-q")
		gitCmd(t, dir, "config", "url."+r.fork+".insteadOf", "https://github.com/fork/repo")
		sctx.Repo.ForkURL = "https://github.com/fork/repo"
	}
	sctx.Run.Branch = "refs/tags/v1"
	sctx.Config.CITimeout = time.Minute
	r.sctx = sctx
	return r
}

func (r *tagRepo) pushTagDirect(t *testing.T, target string) {
	t.Helper()
	gitCmd(t, r.dir, "push", "-q", target, "refs/tags/v1")
}

// fakeClockCI advances its clock on every poll wait, so the absolute tag
// timeout is reached deterministically.
func fakeClockCI() *CIStep {
	now := time.Unix(0, 0)
	return (&CIStep{}).
		SetNow(func() time.Time { return now }).
		SetWaitForNextPoll(func(context.Context, time.Duration) error { now = now.Add(30 * time.Second); return nil })
}

func TestTagPublicationJourneyPreservesAnnotatedTagAndPasses(t *testing.T) {
	for _, tc := range []struct{ annotated, gateHasTag bool }{{true, false}, {true, true}, {false, false}} {
		annotated := tc.annotated
		r := newTagRepo(t, annotated, false)
		local := gitCmd(t, r.dir, "rev-parse", "refs/tags/v1")
		gate := setupGateMirror(t, r.sctx)
		if tc.gateHasTag {
			gitCmd(t, r.dir, "push", "-q", gate, "refs/tags/v1")
		}
		recordReviewApproval(t, r.sctx, r.sctx.Run.HeadSHA)
		r.sctx.Env, _ = tagGH(t, map[string]string{"FAKE_CLI_WORKFLOW_RUNS": tagRunSuccess})
		// The second push is a retry/rerun over an already published tag.
		for attempt := 1; attempt <= 2; attempt++ {
			if _, err := (&PushStep{}).Execute(r.sctx); err != nil {
				t.Fatalf("%+v push %d: %v", tc, attempt, err)
			}
			for name, repo := range map[string]string{"upstream": r.upstream, "gate": gate} {
				if got := gitCmd(t, repo, "rev-parse", "refs/tags/v1"); got != local {
					t.Fatalf("%+v push %d: %s tag object %s, want local %s", tc, attempt, name, got, local)
				}
			}
		}
		pr, err := (&PRStep{}).Execute(r.sctx)
		if err != nil || pr.Skipped || pr.PRURL != "" {
			t.Fatalf("annotated=%v PR: outcome=%+v err=%v, want completed without PR", annotated, pr, err)
		}
		ci, err := fakeClockCI().Execute(r.sctx)
		if err != nil || ci.Skipped || ci.NeedsApproval {
			t.Fatalf("annotated=%v CI: outcome=%+v err=%v, want verified pass", annotated, ci, err)
		}
	}
}

func TestTagPushRefusesToMoveDifferentRemoteTag(t *testing.T) {
	r := newTagRepo(t, true, false)
	other := t.TempDir()
	gitCmd(t, other, "clone", "-q", r.upstream, ".")
	gitCmd(t, r.dir, "push", "-q", r.upstream, r.sctx.Run.BaseSHA+":refs/heads/main")
	gitCmd(t, other, "fetch", "-q", "origin", "main")
	gitCmd(t, other, "tag", "-a", "v1", "-m", "someone else", "FETCH_HEAD")
	gitCmd(t, other, "push", "-q", "origin", "refs/tags/v1")
	before := gitCmd(t, r.upstream, "rev-parse", "refs/tags/v1")
	setupGateMirror(t, r.sctx)
	recordReviewApproval(t, r.sctx, r.sctx.Run.HeadSHA)
	_, err := (&PushStep{}).Execute(r.sctx)
	if err == nil || !strings.Contains(err.Error(), "never moved") {
		t.Fatalf("push over a different tag: err=%v, want refusal", err)
	}
	if after := gitCmd(t, r.upstream, "rev-parse", "refs/tags/v1"); after != before {
		t.Fatalf("remote tag moved from %s to %s", before, after)
	}
}

func TestTagCIGatesOnFailedMissingPendingAndUnavailable(t *testing.T) {
	cases := map[string]map[string]string{
		"failed":      {"FAKE_CLI_WORKFLOW_RUNS": tagRunFailure},
		"missing":     {"FAKE_CLI_WORKFLOW_RUNS": "[]"},
		"pending":     {"FAKE_CLI_WORKFLOW_RUNS": tagRunPending},
		"unavailable": {"FAKE_CLI_AUTH_ERR": "not logged in"},
	}
	for name, vars := range cases {
		r := newTagRepo(t, true, false)
		r.pushTagDirect(t, "origin")
		var logFile string
		r.sctx.Env, logFile = tagGH(t, vars)
		out, err := fakeClockCI().Execute(r.sctx)
		if err != nil || out.Skipped || !out.NeedsApproval {
			t.Fatalf("%s: outcome=%+v err=%v, want approval gate", name, out, err)
		}
		if name != "unavailable" {
			if log := readFile(t, logFile); !strings.Contains(log, "branch=v1") || !strings.Contains(log, "event=push") || strings.Contains(log, "pr create") {
				t.Fatalf("%s: gh calls not tag-scoped:\n%s", name, log)
			}
		}
		if name == "pending" {
			// Retrying the gate once the run finishes green delivers the tag.
			r.sctx.Env, _ = tagGH(t, map[string]string{"FAKE_CLI_WORKFLOW_RUNS": tagRunSuccess})
			r.sctx.Fixing = true
			out, err = fakeClockCI().Execute(r.sctx)
			if err != nil || out.Skipped || out.NeedsApproval {
				t.Fatalf("retry after pending: outcome=%+v err=%v, want pass", out, err)
			}
		}
	}
}

func TestTagCIReadsRunsFromThePushTarget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		upstream, forkRuns string
		wantGate           bool
	}{
		{"upstream green, fork failed", tagRunSuccess, tagRunFailure, true},
		{"upstream failed, fork green", tagRunFailure, tagRunSuccess, false},
	} {
		r := newTagRepo(t, true, true)
		r.pushTagDirect(t, "https://github.com/fork/repo")
		r.sctx.Env, _ = tagGH(t, map[string]string{
			"FAKE_CLI_WORKFLOW_RUNS":      tc.upstream,
			"FAKE_CLI_WORKFLOW_RUNS_REPO": "fork/repo",
			"FAKE_CLI_REPO_WORKFLOW_RUNS": tc.forkRuns,
		})
		out, err := fakeClockCI().Execute(r.sctx)
		if err != nil || out.Skipped || out.NeedsApproval != tc.wantGate {
			t.Fatalf("%s: outcome=%+v err=%v, want gate=%v", tc.name, out, err, tc.wantGate)
		}
	}
}

func TestTagPRRejectsMovedRemoteTag(t *testing.T) {
	r := newTagRepo(t, true, false)
	gitCmd(t, r.dir, "push", "-q", "-f", "origin", r.sctx.Run.BaseSHA+":refs/tags/v1")
	if _, err := (&PRStep{}).Execute(r.sctx); err == nil || !strings.Contains(err.Error(), "peels to") {
		t.Fatalf("moved tag: err=%v, want identity mismatch", err)
	}
}

func TestTagPushRefusesToMoveDifferentGateMirrorTag(t *testing.T) {
	r := newTagRepo(t, true, false)
	r.sctx.Env, _ = tagGH(t, nil)
	gate := setupGateMirror(t, r.sctx)
	gitCmd(t, r.dir, "push", "-q", gate, r.sctx.Run.HeadSHA+":refs/tags/v1")
	recordReviewApproval(t, r.sctx, r.sctx.Run.HeadSHA)
	_, err := (&PushStep{}).Execute(r.sctx)
	if err == nil || !strings.Contains(err.Error(), "never moved") {
		t.Fatalf("gate mirror holds a different tag object: err=%v, want refusal", err)
	}
	if got := gitCmd(t, gate, "rev-parse", "refs/tags/v1"); got != r.sctx.Run.HeadSHA {
		t.Fatalf("gate mirror tag moved to %s", got)
	}
	if out, err := exec.Command("git", "-C", r.upstream, "rev-parse", "--verify", "--quiet", "refs/tags/v1").CombinedOutput(); err == nil {
		t.Fatalf("upstream received the tag before the gate conflict was refused: %s", out)
	}
}

// A same-named branch push reports the same head_branch and event as the tag
// push, so only a workflow that tag pushes alone can trigger counts.
func TestTagCIGatesWhenRunCouldComeFromSameNamedBranch(t *testing.T) {
	for name, workflow := range map[string]string{
		"unfiltered push":   "on: push\n",
		"branches and tags": "on:\n  push:\n    branches: [\"*\"]\n    tags: [\"v*\"]\n",
		"paths only":        "on:\n  push:\n    paths: [\"src/**\"]\n",
	} {
		r := newTagRepo(t, true, false)
		r.pushTagDirect(t, "origin")
		r.sctx.Env, _ = tagGH(t, map[string]string{"FAKE_CLI_WORKFLOW_RUNS": tagRunSuccess, "FAKE_CLI_WORKFLOW_FILE": workflow})
		out, err := fakeClockCI().Execute(r.sctx)
		if err != nil || out.Skipped || !out.NeedsApproval || !strings.Contains(out.Findings, "branch push") {
			t.Fatalf("%s: outcome=%+v err=%v, want ambiguous-provenance gate", name, out, err)
		}
	}
}

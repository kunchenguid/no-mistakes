package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// fakeTagGH answers auth and the workflow-runs listing with runsJSON; any
// other gh call (pr list/create) fails, as GitHub rejects a tag as PR head.
func fakeTagGH(t *testing.T, runsJSON string) (env []string, logFile string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(bin, "gh.log")
	runs := filepath.Join(bin, "runs.json")
	if err := os.WriteFile(runs, []byte(runsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + logFile + "'\n" +
		"case \"$1 $2\" in\n" +
		"'auth status') exit 0;;\n" +
		"'api '*) case \"$*\" in *actions/runs*) cat '" + runs + "'; exit 0;; esac;;\n" +
		"esac\necho 'Head ref must be a branch' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}, logFile
}

func tagFixture(t *testing.T, runsJSON string, annotated bool) (*pipeline.StepContext, func() string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "-q")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	if annotated {
		gitCmd(t, dir, "tag", "-a", "v1", "-m", "release", headSHA)
	} else {
		gitCmd(t, dir, "tag", "v1", headSHA)
	}
	gitCmd(t, dir, "push", "-q", "origin", "refs/tags/v1")
	env, logFile := fakeTagGH(t, runsJSON)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/tags/v1"
	readLog := func() string { b, _ := os.ReadFile(logFile); return string(b) }
	return sctx, readLog, dir
}

func TestTagPublicationFinishesWithoutPRAndGatesOnTagRuns(t *testing.T) {
	for _, annotated := range []bool{true, false} {
		sctx, readLog, _ := tagFixture(t, tagRunSuccess, annotated)
		out, err := (&PRStep{}).Execute(sctx)
		if err != nil {
			t.Fatalf("annotated=%v: PR step on a tag: %v", annotated, err)
		}
		if !out.Skipped || !strings.Contains(out.SkipReason, "tag") {
			t.Fatalf("annotated=%v: PR outcome = %+v, want explicit tag skip", annotated, out)
		}
		out, err = (&CIStep{}).SetPollIntervalOverride(1).Execute(sctx)
		if err != nil {
			t.Fatalf("annotated=%v: CI step on a tag: %v", annotated, err)
		}
		if out.Skipped || out.NeedsApproval {
			t.Fatalf("annotated=%v: CI outcome = %+v, want verified pass", annotated, out)
		}
		if log := readLog(); !strings.Contains(log, "branch=v1") || !strings.Contains(log, "event=push") || strings.Contains(log, "pr ") {
			t.Fatalf("annotated=%v: gh calls not tag-scoped:\n%s", annotated, log)
		}
	}
}

func TestTagPublicationRejectsFailedRunAndMovedTag(t *testing.T) {
	sctx, _, dir := tagFixture(t, tagRunFailure, true)
	out, err := (&CIStep{}).SetPollIntervalOverride(1).Execute(sctx)
	if err != nil || !out.NeedsApproval {
		t.Fatalf("failed tag workflow: outcome=%+v err=%v, want approval gate", out, err)
	}
	gitCmd(t, dir, "push", "-q", "-f", "origin", sctx.Run.BaseSHA+":refs/tags/v1")
	if _, err := (&PRStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "peels to") {
		t.Fatalf("moved tag: err=%v, want identity mismatch", err)
	}
}

const tagRunSuccess = `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":3,"name":"release","status":"completed","conclusion":"success","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`
const tagRunFailure = `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":3,"name":"release","status":"completed","conclusion":"failure","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`

func TestTagPublicationGatesWhenHostUnavailable(t *testing.T) {
	sctx, _, _ := tagFixture(t, tagRunSuccess, false)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sctx.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := (&CIStep{}).Execute(sctx)
	if err != nil || out.Skipped || !out.NeedsApproval {
		t.Fatalf("unavailable host on tag: outcome=%+v err=%v, want approval gate", out, err)
	}
}

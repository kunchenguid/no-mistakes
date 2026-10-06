package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Keep process-heavy repair fixtures serial on macOS; elsewhere their isolated
// Git repos, databases and per-command environments can share the test parallel
// budget instead of extending the Windows shard's serial critical path.
func TestCIStepLateFindingRevalidatesEvenWhenOrdinaryRepairsPublish(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, mode := range []string{"repaired", "fix-error", "no-change", "no-code-needed", "closed", "no-code-needed-with-files", "no-code-needed-with-commit", "empty-agent-commit"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if mode == "fix-error" {
					return nil, errors.New("controlled repair failure")
				}
				if mode == "no-change" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"claimed fix without changes","code_change_needed":true}`)}, nil
				}
				if mode == "no-code-needed" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"no code needed","code_change_needed":false}`)}, nil
				}

				if mode == "empty-agent-commit" {
					gitCmd(t, opts.CWD, "commit", "--allow-empty", "-m", "empty repair")
					return &agent.Result{Output: json.RawMessage(`{"summary":"empty repair","code_change_needed":true}`)}, nil
				}
				if err := os.WriteFile(filepath.Join(opts.CWD, "late-fix.txt"), []byte("new requirement\n"), 0o644); err != nil {
					return nil, err
				}
				if mode == "no-code-needed-with-commit" {
					gitCmd(t, opts.CWD, "add", "late-fix.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "declined repair")
					return &agent.Result{Output: json.RawMessage(`{"summary":"declined repair despite commit","code_change_needed":false}`)}, nil
				}
				if mode == "no-code-needed-with-files" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"declined repair despite edits","code_change_needed":false}`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"apply new requirement","code_change_needed":true}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.CI.RevalidateRepairs = false
			gitCmd(t, dir, "push", "origin", "feature")
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			state := "OPEN"
			if mode == "closed" {
				state = "CLOSED"
			}
			sctx.Env = fakeCIGH(t, state, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			recordReviewApproval(t, sctx, head)
			ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.StartStep(ci.ID); err != nil {
				t.Fatal(err)
			}
			raw := `{"findings":[{"id":"late-1","description":"new requirement","severity":"error","action":"ask-user","category":"ci-late-finding"}]}`
			if err := sctx.DB.AdmitLateCIFindings(sctx.Run.ID, ci.ID, head, raw); err != nil {
				t.Fatal(err)
			}
			sctx.Fixing = true
			sctx.PreviousFindings = raw
			before := gitCmd(t, dir, "rev-parse", "origin/feature")
			outcome, err := (&CIStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "closed" {
				if len(ag.calls) != 0 || !outcome.SkipRemaining || outcome.RestartFrom != "" {
					t.Fatalf("closed PR entered fixer: outcome=%+v calls=%d", outcome, len(ag.calls))
				}
				persisted, _ := sctx.DB.GetRun(sctx.Run.ID)
				if persisted.PRState == nil || *persisted.PRState != "closed" || persisted.Status != types.RunCompleted {
					t.Fatalf("terminal lifecycle lost: %+v", persisted)
				}
				return
			}
			if mode != "repaired" {
				if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "late-1") || !strings.Contains(outcome.Findings, "ci-late-finding") || outcome.RestartFrom != "" || len(ag.calls) != 1 {
					t.Fatalf("unresolved amendment escaped: outcome=%+v calls=%d", outcome, len(ag.calls))
				}
				persisted, _ := sctx.DB.GetRun(sctx.Run.ID)
				if persisted.CIReadyAt != nil || persisted.ReviewApprovedHeadSHA != nil || persisted.HeadSHA != head {
					t.Fatal("unresolved amendment regained readiness or changed head")
				}
				if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
					t.Fatal("unresolved amendment published")
				}
				return
			}
			if outcome.RestartFrom != types.StepReview || len(ag.calls) != 1 {
				t.Fatalf("outcome=%+v calls=%d", outcome, len(ag.calls))
			}
			if sctx.Run.HeadSHA == head {
				t.Fatal("repair did not create amended bytes")
			}
			if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
				t.Fatal("amendment published before revalidation")
			}
			persisted, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.ReviewApprovedHeadSHA != nil || persisted.LastPushedSHA == nil || *persisted.LastPushedSHA != head {
				t.Fatal("repair regained old authority or changed publication binding")
			}

			for _, terminal := range []string{"CLOSED", "MERGED"} {
				recordReviewApproval(t, sctx, sctx.Run.HeadSHA)
				sctx.Fixing = false
				sctx.Env = fakeCIGH(t, terminal, `[]`)
				if _, err := (&PushStep{}).Execute(sctx); err == nil {
					t.Fatalf("published amended head after PR became %s", terminal)
				}
				if got := gitCmd(t, dir, "rev-parse", "origin/feature"); got != before {
					t.Fatal("terminal PR allowed publication")
				}
			}
		})
	}
}

func TestLateCIAdmissionRequiresLiveOpenOwnedPR(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, state := range []string{"OPEN", "CLOSED", "MERGED", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Env = fakeCIGH(t, state, `[]`)
			if err := (&CIStep{}).VerifyLateCIAdmission(sctx); (err == nil) != (state == "OPEN") {
				t.Fatalf("state=%s admission=%v", state, err)
			}
		})
	}
}

func TestTerminalOwnedPRNeverBindsReplacement(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, state := range []scm.PRState{scm.PRStateClosed, scm.PRStateMerged} {
		for _, discovered := range []*scm.PR{nil, {Number: "99", URL: "https://github.com/test/repo/pull/99"}, {Number: "42", URL: "https://github.com/test/repo/pull/42"}} {
			owned := "https://github.com/test/repo/pull/42"
			sctx := &pipeline.StepContext{Run: &db.Run{PRURL: &owned}}
			pr, err := bindExistingPR(sctx, &recordingRetargetHost{state: state}, discovered)
			if err == nil || pr != nil {
				t.Fatalf("terminal owned PR selected replacement: %v %v", pr, err)
			}
		}
	}
}

func newLateCIRepairFixture(t *testing.T, ag *mockAgent) *ciRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Agent = ag
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	ci, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.StepResultID = ci.ID
	if err := f.sctx.DB.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[{"id":"late-1","description":"new requirement","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"preserve the operator guidance"},{"id":"deferred-1","description":"second requirement","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"preserve deferred guidance"}]}`
	if err := f.sctx.DB.AdmitLateCIFindings(f.sctx.Run.ID, ci.ID, f.headSHA, raw); err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	selected := types.FindingsMetadata(findings)
	selected.Items = findings.Items[:1]
	deferred := types.FindingsMetadata(findings)
	deferred.Items = findings.Items[1:]
	f.sctx.PreviousFindings, err = types.MarshalFindingsJSON(selected)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.DeferredFindings, err = types.MarshalFindingsJSON(deferred)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.Fixing = true
	return f
}

func assertLateCIRequestParked(t *testing.T, outcome *pipeline.StepOutcome) {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval || outcome.Skipped || outcome.RestartFrom != "" {
		t.Fatalf("unresolved amendment escaped: %+v", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"late-1": "preserve the operator guidance", "deferred-1": "preserve deferred guidance"}
	for _, finding := range findings.Items {
		if guidance, ok := want[finding.ID]; ok {
			if finding.UserInstructions != guidance {
				t.Fatalf("guidance lost for %s: %+v", finding.ID, finding)
			}
			delete(want, finding.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("original amendment missing: %v", want)
	}
}

func TestLateCIRepairRetainedProtectedRetry(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, mode := range []string{"retry", "empty-retained", "repeat-refusal", "closed", "merged", "unreadable", "credentials", "host"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if mode == "empty-retained" {
					gitCmd(t, opts.CWD, "commit", "--allow-empty", "-m", "empty retained repair")
				} else {
					if err := os.WriteFile(filepath.Join(opts.CWD, "repair.txt"), []byte("material repair"), 0o644); err != nil {
						return nil, err
					}
					gitCmd(t, opts.CWD, "add", "repair.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "retained repair")
				}
				if err := os.WriteFile(filepath.Join(opts.CWD, "blocked.lock"), []byte("blocked edit"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"material repair","code_change_needed":true}`)}, nil
			}}
			f := newLateCIRepairFixture(t, ag)
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			if !pipeline.HasProtectedPathRefusal(outcome.Findings) || f.sctx.Run.HeadSHA == f.headSHA {
				t.Fatalf("repair custody/refusal missing: %+v", outcome)
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.DeferredFindings = ""
			if mode != "repeat-refusal" {
				if err := os.Remove(filepath.Join(f.dir, "blocked.lock")); err != nil {
					t.Fatal(err)
				}
			}
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			switch mode {
			case "closed":
				f.sctx.Env = fakeCIGH(t, "CLOSED", `[]`)
			case "merged":
				f.sctx.Env = fakeCIGH(t, "MERGED", `[]`)
			case "unreadable":
				f.sctx.Env = fakeCIGHStateError(t, "cannot read owned PR", `[]`)
			case "credentials":
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_AUTH_ERR=not logged in")
			case "host":
				f.sctx.Run.PRURL = nil
				f.sctx.Repo.UpstreamURL = "https://unsupported.example/test/repo"
			}
			if mode != "retry" && mode != "empty-retained" && mode != "repeat-refusal" {
				if err := os.WriteFile(filepath.Join(f.dir, "uncommitted.txt"), []byte("must remain uncommitted"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			beforeHead := f.localHead(t)
			beforeStatus := gitCmd(t, f.dir, "status", "--porcelain")
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				t.Fatal("retained late repair resumed monitoring")
				return nil
			}}
			outcome, err = step.Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "retry":
				if outcome.RestartFrom != types.StepReview {
					t.Fatalf("retained material repair skipped Review: %+v", outcome)
				}
			case "closed", "merged":
				if !outcome.SkipRemaining || outcome.RestartFrom != "" {
					t.Fatalf("terminal lifecycle lost: %+v", outcome)
				}
			default:
				assertLateCIRequestParked(t, outcome)
				if mode == "repeat-refusal" {
					if !pipeline.HasProtectedPathRefusal(outcome.Findings) {
						t.Fatal("repeat refusal lost its refusal finding")
					}
					persistCIRefusal(t, f, outcome)
					sr, err := f.sctx.DB.GetStepResult(f.sctx.StepResultID)
					if err != nil || sr.FindingsJSON == nil {
						t.Fatalf("recovery gate missing: %v", err)
					}
					assertLateCIRequestParked(t, &pipeline.StepOutcome{NeedsApproval: true, Findings: *sr.FindingsJSON})
					selected, err := types.ParseFindingsJSON(*sr.FindingsJSON)
					if err != nil {
						t.Fatal(err)
					}
					for i := range selected.Items {
						if selected.Items[i].ID == "late-1" {
							selected.Items[i].UserInstructions = "updated operator guidance"
						}
					}
					f.sctx.PreviousFindings, err = types.MarshalFindingsJSON(selected)
					if err != nil {
						t.Fatal(err)
					}
					updated, err := step.Execute(f.sctx)
					if err != nil || updated == nil || !updated.NeedsApproval {
						t.Fatalf("updated request escaped: %+v, %v", updated, err)
					}
					findings, err := types.ParseFindingsJSON(updated.Findings)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, finding := range findings.Items {
						if finding.ID == "late-1" {
							found = finding.UserInstructions == "updated operator guidance"
						}
					}
					if !found {
						t.Fatal("retry replaced current operator instructions with the stored snapshot")
					}
				}
			}
			if len(ag.calls) != 1 || f.localHead(t) != beforeHead || gitCmd(t, f.dir, "status", "--porcelain") != beforeStatus || f.remoteHead(t) != f.headSHA {
				t.Fatal("retry mutated Git or reran fixer unexpectedly")
			}
			persisted, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || persisted.CIReadyAt != nil || persisted.ReviewApprovedHeadSHA != nil || persisted.LastPushedSHA == nil || *persisted.LastPushedSHA != f.headSHA {
				t.Fatalf("retained repair regained readiness/publication: %+v, %v", persisted, err)
			}
		})
	}
}

func TestLateCIRepairAvailabilityFailureParksRequest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, mode := range []string{"credentials", "host"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			ag := &mockAgent{name: "test"}
			f := newLateCIRepairFixture(t, ag)
			if mode == "credentials" {
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_AUTH_ERR=not logged in")
			} else {
				f.sctx.Run.PRURL = nil
				f.sctx.Repo.UpstreamURL = "https://unsupported.example/test/repo"
			}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			if len(ag.calls) != 0 || f.localHead(t) != f.headSHA || f.remoteHead(t) != f.headSHA {
				t.Fatal("unavailable repair entry mutated Git or ran fixer")
			}
		})
	}
}

func TestLateCIRepairTimeoutRetainedCommitRevalidates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, negative := range []bool{false, true} {
		t.Run(fmt.Sprint("negative=", negative), func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					if err := os.WriteFile(filepath.Join(opts.CWD, "repair.txt"), []byte("material repair"), 0o644); err != nil {
						return nil, err
					}
					gitCmd(t, opts.CWD, "add", "repair.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "retained repair")
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &agent.Result{Output: json.RawMessage(fmt.Sprintf(`{"summary":"retained work","code_change_needed":%t}`, !negative))}, nil
			}}
			f := newLateCIRepairFixture(t, ag)
			f.sctx.Config.AgentTimeout = time.Second
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			assertLateCIRequestParked(t, outcome)
			retained := f.localHead(t)
			if retained == f.headSHA || f.sctx.Run.HeadSHA != retained {
				t.Fatal("timed-out repair was not retained")
			}
			persistCIRefusal(t, f, outcome)
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.DeferredFindings = ""
			f.sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			outcome, err = (&CIStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatal(err)
			}
			if negative {
				assertLateCIRequestParked(t, outcome)
			} else if outcome.RestartFrom != types.StepReview {
				t.Fatalf("retained timeout repair skipped Review: %+v", outcome)
			}
			if calls != 2 || f.localHead(t) != retained || f.remoteHead(t) != f.headSHA {
				t.Fatal("retained timeout repair was changed or published")
			}
		})
	}
}

type latePublicationAdmissionStep struct {
	env     []string
	started chan struct{}
}

func (s *latePublicationAdmissionStep) Name() types.StepName { return types.StepCI }
func (s *latePublicationAdmissionStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := sctx.DB.SetRunCIReady(sctx.Run.ID, true); err != nil {
		return nil, err
	}
	close(s.started)
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}
func (s *latePublicationAdmissionStep) VerifyLateCIAdmission(sctx *pipeline.StepContext) error {
	sctx.Env = s.env
	return (&CIStep{}).VerifyLateCIAdmission(sctx)
}

func TestLateCIAdmissionRefusesMovedOrUnreadableOwnedPublicationWithoutMutation(t *testing.T) {
	for _, mode := range []string{"advanced", "missing", "unreadable", "advanced-fork"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "advanced", "advanced-fork":
				target := f.upstream
				if mode == "advanced-fork" {
					target = filepath.Join(t.TempDir(), "fork.git")
					gitCmd(t, f.dir, "clone", "--bare", f.upstream, target)
					f.sctx.Repo.ForkURL = target
				}
				gitCmd(t, f.dir, "commit", "--allow-empty", "-m", "external advance")
				gitCmd(t, f.dir, "push", target, "HEAD:refs/heads/feature")
				gitCmd(t, f.dir, "reset", "--hard", f.headSHA)
			case "missing":
				gitCmd(t, f.upstream, "update-ref", "-d", "refs/heads/feature")
			case "unreadable":
				gitCmd(t, f.dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			}
			t.Setenv("NM_HOME", t.TempDir())
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			step := &latePublicationAdmissionStep{env: fakeCIGH(t, "OPEN", `[]`), started: make(chan struct{})}
			executor := pipeline.NewExecutor(f.sctx.DB, p, f.sctx.Config, f.sctx.Agent, []pipeline.Step{step}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, f.sctx.Run, f.sctx.Repo, f.dir) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("refused monitor did not stop")
				}
			}()
			select {
			case <-step.started:
			case <-ctx.Done():
				t.Fatal("initial monitor did not start")
			}
			before, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSteps, err := f.sctx.DB.GetStepsByRun(f.sctx.Run.ID)
			if err != nil || len(beforeSteps) != 1 {
				t.Fatalf("monitor step missing: %v", err)
			}
			beforeRounds, err := f.sctx.DB.GetRoundsByStep(beforeSteps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.CIReadyAt == nil || before.ReviewApprovedHeadSHA == nil {
				t.Fatal("fixture has no readiness or review authority to preserve")
			}
			err = executor.RespondToLateCIFinding(f.sctx.Run.ID, types.StepCI, types.ActionFix, nil, nil, []types.Finding{{ID: "late-1", Description: "new requirement"}}, "", f.headSHA)
			if err == nil {
				t.Fatal("stale/unreadable owned publication admitted")
			}
			if !strings.Contains(err.Error(), "publication") && !strings.Contains(err.Error(), "published head") {
				t.Fatalf("admission refused for an unrelated reason: %v", err)
			}
			after, readErr := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			afterSteps, readErr := f.sctx.DB.GetStepsByRun(f.sctx.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			afterRounds, readErr := f.sctx.DB.GetRoundsByStep(beforeSteps[0].ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeSteps, afterSteps) || !reflect.DeepEqual(beforeRounds, afterRounds) || f.localHead(t) != f.headSHA || len(f.sctx.Agent.(*mockAgent).calls) != 0 {
				t.Fatal("refused publication changed readiness, authority, gate, worktree or fixer")
			}
		})
	}
}

func TestLateCIRetainedRepairMergeProofUsesPublishedHead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, retention := range []string{"protected", "timeout"} {
		for _, proofHead := range []string{"published", "unpublished"} {
			t.Run(retention+"/"+proofHead, func(t *testing.T) {
				if runtime.GOOS != "darwin" {
					t.Parallel()
				}
				ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if err := os.WriteFile(filepath.Join(opts.CWD, "retained.txt"), []byte("material repair"), 0o644); err != nil {
						return nil, err
					}
					gitCmd(t, opts.CWD, "add", "retained.txt")
					gitCmd(t, opts.CWD, "commit", "-m", "retained repair")
					if retention == "timeout" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if err := os.WriteFile(filepath.Join(opts.CWD, "blocked.lock"), []byte("blocked edit"), 0o644); err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(`{"summary":"retained repair","code_change_needed":true}`)}, nil
				}}
				f := newLateCIRepairFixture(t, ag)
				f.sctx.Config.ProtectedPaths = []string{"*.lock"}
				f.sctx.Config.AgentTimeout = time.Second
				parked, err := (&CIStep{}).Execute(f.sctx)
				if err != nil {
					t.Fatal(err)
				}
				assertLateCIRequestParked(t, parked)
				persistCIRefusal(t, f, parked)
				retained := f.localHead(t)
				if retained == f.headSHA || f.sctx.Run.HeadSHA != retained {
					t.Fatal("repair custody was not advanced")
				}
				expectedProof := f.headSHA
				if proofHead == "unpublished" {
					expectedProof = retained
				}
				url := "https://git.example.com/team/repo/pull/42"
				f.sctx.Run.PRURL = &url
				if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, url); err != nil {
					t.Fatal(err)
				}
				_, logPath := fakeProviderPlugin(t, f.sctx, fakeplugin.State{PRs: []fakeplugin.PR{{Number: "42", URL: url, HeadBranch: "feature", BaseBranch: "main", HeadSHA: expectedProof, State: "merged"}}})
				f.sctx.PreviousFindings = parked.Findings
				f.sctx.DeferredFindings = ""
				resolved, err := (&CIStep{}).ReconcileApprovalGate(f.sctx)
				if proofHead == "published" {
					if err != nil || !resolved {
						t.Fatalf("published merge proof refused retained repair: %v, %v", resolved, err)
					}
				} else if err == nil || resolved {
					t.Fatal("unpublished repair was certified as merged")
				}
				outcome, err := (&CIStep{}).Execute(f.sctx)
				if err != nil {
					t.Fatal(err)
				}
				if proofHead == "published" {
					if !outcome.SkipRemaining || outcome.RestartFrom != "" {
						t.Fatalf("merged publication entered repair: %+v", outcome)
					}
					f.sctx.Fixing = false
					if _, err := (&CIStep{}).Execute(f.sctx); err != nil {
						t.Fatalf("terminal monitor used unpublished custody for proof: %v", err)
					}
				} else {
					assertLateCIRequestParked(t, outcome)
				}
				persisted, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.HeadSHA != retained || persisted.LastPushedSHA == nil || *persisted.LastPushedSHA != f.headSHA || f.localHead(t) != retained || f.remoteHead(t) != f.headSHA || len(ag.calls) != 1 {
					t.Fatal("terminal merge changed unpublished custody or publication")
				}
				if proofHead == "published" && (persisted.PRState == nil || *persisted.PRState != "merged") {
					t.Fatal("published merge did not settle terminal lifecycle")
				}
				for _, op := range pluginOperations(t, logPath) {
					if op == "pr find" || op == "pr create" || op == "pr update" {
						t.Fatalf("terminal repair entered publication/replacement: %s", op)
					}
				}
			})
		}
	}
}

func TestLateCIUnavailablePublicationRefusesAfterValidationRestart(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Parallel()
	}
	for _, mode := range []string{"credentials", "host"} {
		t.Run(mode, func(t *testing.T) {
			if runtime.GOOS != "darwin" {
				t.Parallel()
			}
			f := newLateCIRepairFixture(t, &mockAgent{name: "test"})
			if err := os.WriteFile(filepath.Join(f.dir, "amendment.txt"), []byte("amended requirement"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "add", "amendment.txt")
			gitCmd(t, f.dir, "commit", "-m", "amendment")
			amended := f.localHead(t)
			if err := f.sctx.DB.UpdateRunHeadSHAForRevalidation(f.sctx.Run.ID, amended); err != nil {
				t.Fatal(err)
			}
			f.sctx.Run.HeadSHA = amended
			if err := f.sctx.DB.ResetStepsFrom(f.sctx.Run.ID, 0); err != nil {
				t.Fatal(err)
			}
			recordReviewApproval(t, f.sctx, amended)
			f.sctx.Fixing = false
			f.sctx.PreviousFindings, f.sctx.DeferredFindings = "", ""
			if mode == "credentials" {
				f.sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_AUTH_ERR=temporarily unavailable")
			} else {
				f.sctx.Repo.UpstreamURL = "https://unsupported.example/test/repo"
				f.sctx.Run.PRURL = nil
			}
			if _, err := (&PushStep{}).Execute(f.sctx); err == nil {
				t.Fatal("late amendment published with unavailable lifecycle proof")
			}
			if f.remoteHead(t) != f.headSHA {
				t.Fatal("unavailable late publication moved the remote")
			}
		})
	}
}

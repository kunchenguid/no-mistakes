package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorLateCIFindingJoinsMonitorAndRevalidatesSameRun(t *testing.T) {
	for _, terminal := range []string{"", "merged", "closed"} {
		t.Run("terminal="+terminal, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			dir := t.TempDir()
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			gitCmd := func(args ...string) string {
				t.Helper()
				command := exec.Command(realGit, args...)
				command.Dir = dir
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s: %v", args, output, err)
				}
				return strings.TrimSpace(string(output))
			}
			gitCmd("init", "-b", "feature")
			gitCmd("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
			head := gitCmd("rev-parse", "HEAD")
			run.HeadSHA = head
			if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var order []types.StepName
			ciCalls := 0
			pass := func(name types.StepName) Step {
				return &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
					order = append(order, name)
					if name == types.StepReview {
						return &StepOutcome{ReviewApprovedHeadSHA: head}, nil
					}
					return &StepOutcome{}, nil
				}}
			}
			ciStep := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
				ciCalls++
				if ciCalls == 1 {
					if err := database.SetRunCIReady(run.ID, true); err != nil {
						return nil, err
					}
					sctx.CIReadinessChanged(true, false)
					close(started)
					<-sctx.Ctx.Done()
					close(cancelled)
					<-release
					if terminal != "" {
						if err := database.UpdateRunPRState(run.ID, terminal); err != nil {
							return nil, err
						}
					}
					// Simulate a provider poll completing after cancellation/admission.
					if err := database.SetRunCIReady(run.ID, true); err != nil {
						return nil, err
					}
					sctx.CIReadinessChanged(true, false)
					return nil, sctx.Ctx.Err()
				}
				if ciCalls == 2 {
					if !sctx.Fixing || (!strings.Contains(sctx.PreviousFindings, "new acceptance condition") || !strings.Contains(sctx.PreviousFindings, "preserve parser behavior")) || sctx.DeferredFindings != "" {
						return nil, fmt.Errorf("repair lost selected finding: fixing=%v selected=%s deferred=%s", sctx.Fixing, sctx.PreviousFindings, sctx.DeferredFindings)
					}
					persisted, err := database.GetRun(run.ID)
					if err != nil {
						return nil, err
					}
					if persisted.ReviewApprovedHeadSHA != nil {
						return nil, fmt.Errorf("old review authority survived admission")
					}
					return &StepOutcome{RestartFrom: types.StepReview}, nil
				}
				return &StepOutcome{}, nil
			}}
			ci := &lateAdmissionTestStep{adaptiveCallStep: ciStep}
			cfg := &config.Config{}
			executor := NewExecutor(database, p, cfg, nil, []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), pass(types.StepPush), pass(types.StepPR), ci}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- executor.Execute(ctx, run, repo, dir) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("monitor did not start")
			}
			before, _ := database.GetRun(run.ID)
			finding := []types.Finding{{ID: "late-1", Description: "new acceptance condition", Action: types.ActionAutoFix, Severity: types.FindingSeverityError}}
			if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", "stale"); err == nil {
				t.Fatal("stale head admitted")
			}
			for _, state := range []string{"closed", "merged", "unavailable"} {
				ci.refusal = state
				if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", head); err == nil {
					t.Fatalf("%s PR admitted", state)
				}
				after, _ := database.GetRun(run.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("refused admission mutated run")
				}
			}
			ci.refusal = ""
			response := make(chan error, 1)
			instructions := map[string]string{"late-1": "preserve parser behavior"}
			if terminal == "" {
				finding[0].UserInstructions = "preserve parser behavior"
				instructions = nil
			}
			go func() {
				response <- executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, instructions, finding, "", head)
			}()
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("monitor not cancelled")
			}
			during, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRecoveredRun(database, during, executor.steps); err != nil {
				t.Fatalf("admitted gate cannot survive daemon restart: %v", err)
			}
			results, _ := database.GetStepsByRun(run.ID)
			for _, result := range results {
				if result.StepName != types.StepCI {
					continue
				}
				rounds, _ := database.GetRoundsByStep(result.ID)
				var retained types.Findings
				if len(rounds) == 0 || rounds[len(rounds)-1].FindingsJSON == nil {
					t.Fatal("missing durable finding")
				}
				if err := json.Unmarshal([]byte(*rounds[len(rounds)-1].FindingsJSON), &retained); err != nil {
					t.Fatal(err)
				}
				if len(retained.Items) != 1 || retained.Items[0].ID != "late-1" || retained.Items[0].UserInstructions != "preserve parser behavior" {
					t.Fatalf("instructions lost: %+v", retained)
				}
			}
			if during.CIReadyAt != nil || during.ReviewApprovedHeadSHA != nil {
				t.Fatal("admission retained readiness/approval")
			}
			if during.LastPushedSHA == nil || *during.LastPushedSHA != head || *during.PushGeneration != *before.PushGeneration || *during.PRURL != *before.PRURL {
				t.Fatalf("admission changed publication custody: before=%+v during=%+v", before, during)
			}
			if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", head); err == nil {
				t.Fatal("concurrent finding admitted")
			}
			select {
			case err := <-response:
				t.Fatalf("acknowledged before monitor joined: %v", err)
			default:
			}
			close(release)
			select {
			case err := <-response:
				if (err != nil) != (terminal != "") {
					t.Fatalf("terminal=%q response=%v", terminal, err)
				}
			case <-ctx.Done():
				t.Fatal("handoff never acknowledged")
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("run did not finish")
			}
			if terminal != "" {
				if ciCalls != 1 {
					t.Fatalf("terminal PR launched a repair: %d CI calls", ciCalls)
				}
				terminalRun, _ := database.GetRun(run.ID)
				if terminalRun.Status != types.RunCompleted || terminalRun.PRState == nil || *terminalRun.PRState != terminal {
					t.Fatalf("terminal state lost: %+v", terminalRun)
				}
				results, _ := database.GetStepsByRun(run.ID)
				for _, result := range results {
					if result.StepName == types.StepCI && result.Status != types.StepStatusCompleted {
						t.Fatalf("terminal CI resurrected: %+v", result)
					}
				}
				return
			}
			if ciCalls != 3 {
				t.Fatalf("CI calls=%d", ciCalls)
			}
			want := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR}
			if fmt.Sprint(order) != fmt.Sprint(append(want, want...)) {
				t.Fatalf("validation order=%v", order)
			}
			after, _ := database.GetRun(run.ID)
			if after.CIReadyAt != nil {
				t.Fatal("cancelled monitor restored readiness")
			}

		})
	}
}

func TestExecutorLateCIFindingRecoveredGateRequiresExplicitFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	names := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	steps := make([]Step, 0, len(names))
	ciCalls := 0
	var ciID string
	for _, name := range names {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(result.ID); err != nil {
			t.Fatal(err)
		}
		if name != types.StepCI {
			if err := database.CompleteStep(result.ID, 0, 0, "historical.log"); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, newPassStep(name))
			continue
		}
		ciID = result.ID
		steps = append(steps, &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
			ciCalls++
			if ciCalls == 1 {
				if !sctx.Fixing || !strings.Contains(sctx.PreviousFindings, "retained acceptance") || !strings.Contains(sctx.PreviousFindings, "retained guidance") {
					return nil, fmt.Errorf("recovery lost amendment: fixing=%v selected=%s", sctx.Fixing, sctx.PreviousFindings)
				}
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}})
	}
	raw := `{"findings":[{"id":"late-1","description":"retained acceptance","severity":"error","action":"ask-user","category":"ci-late-finding","user_instructions":"retained guidance"}]}`
	if err := database.AdmitLateCIFindings(run.ID, ciID, run.HeadSHA, raw); err != nil {
		t.Fatal(err)
	}
	retained, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked := make(chan struct{}, 1)
	executor := NewExecutor(database, p, &config.Config{}, nil, steps, func(event ipc.Event) {
		if event.StepName != nil && *event.StepName == types.StepCI && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parked <- struct{}{}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- executor.Resume(ctx, retained, repo, t.TempDir()) }()
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("recovered amendment not parked")
	}
	if ciCalls != 0 {
		t.Fatal("recovery automatically evaluated the retained amendment")
	}
	if err := executor.Respond(types.StepCI, types.ActionFix, []string{"late-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("recovery failed to resume")
	}
	if ciCalls != 2 {
		t.Fatalf("CI calls=%d, want fix then post-validation monitor", ciCalls)
	}
}

type lateAdmissionTestStep struct {
	*adaptiveCallStep
	refusal string
}

func (s *lateAdmissionTestStep) VerifyLateCIAdmission(*StepContext) error {
	if s.refusal != "" {
		return fmt.Errorf("owned PR: %s", s.refusal)
	}
	return nil
}

func TestExecutorLateCIAmendmentsAcrossPublishedCycles(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash-after-second-admission=%t", crash), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			dir, remote := t.TempDir(), t.TempDir()
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			gitCmd := func(cwd string, args ...string) string {
				t.Helper()
				cmd := exec.Command(realGit, args...)
				cmd.Dir = cwd
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			gitCmd(remote, "init", "--bare")
			gitCmd(dir, "init", "-b", "feature")
			gitCmd(dir, "config", "user.name", "Test")
			gitCmd(dir, "config", "user.email", "test@example.invalid")
			gitCmd(dir, "commit", "--allow-empty", "-m", "initial")
			gitCmd(dir, "remote", "add", "origin", remote)
			initial := gitCmd(dir, "rev-parse", "HEAD")
			if err := database.UpdateRunHeadSHA(run.ID, initial); err != nil {
				t.Fatal(err)
			}
			url := "https://github.com/test/repo/pull/42"
			if err := database.UpdateRunPRURL(run.ID, url); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			type cycle struct {
				head            string
				joined, release chan struct{}
			}
			cycles := make(chan cycle, 3)
			parked := make(chan ipc.Event, 4)
			type visit struct {
				step types.StepName
				head string
			}
			var visits []visit
			var repaired []string
			monitorEntries := 0
			names := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR}
			var steps []Step
			for _, name := range names {
				steps = append(steps, &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
					head := gitCmd(dir, "rev-parse", "HEAD")
					visits = append(visits, visit{name, head})
					if name == types.StepReview {
						return &StepOutcome{ReviewApprovedHeadSHA: head}, nil
					}
					if name == types.StepPush {
						bound, err := sctx.DB.GetRun(sctx.Run.ID)
						if err != nil {
							return nil, err
						}
						if bound.ReviewApprovedHeadSHA == nil || *bound.ReviewApprovedHeadSHA != head {
							return nil, fmt.Errorf("publication lacks current Review authority")
						}
						gitCmd(dir, "push", "origin", "HEAD:refs/heads/feature")
						if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", TargetFingerprint: "owned-publication", Ref: "refs/heads/feature"}); err != nil {
							return nil, err
						}
					}
					return &StepOutcome{}, nil
				}})
			}
			ci := &lateAdmissionTestStep{adaptiveCallStep: &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if sctx.Fixing {
					findings, err := types.ParseFindingsJSON(sctx.PreviousFindings)
					if err != nil {
						return nil, err
					}
					if len(findings.Items) != 1 || sctx.DeferredFindings != "" {
						return nil, fmt.Errorf("repair carried a stale selection: %s / %s", sctx.PreviousFindings, sctx.DeferredFindings)
					}
					item := findings.Items[0]
					want := []string{"ordinary-auto", "late-A", "late-B"}
					if len(repaired) >= len(want) || item.ID != want[len(repaired)] {
						return nil, fmt.Errorf("unexpected repair/replay: %s after %v", item.ID, repaired)
					}
					if item.ID != "ordinary-auto" && item.UserInstructions != "guidance for "+item.ID {
						return nil, fmt.Errorf("repair lost current instructions: %+v", item)
					}
					if err := os.WriteFile(filepath.Join(dir, item.ID+".txt"), []byte(item.UserInstructions+"\n"), 0600); err != nil {
						return nil, err
					}
					gitCmd(dir, "add", item.ID+".txt")
					gitCmd(dir, "commit", "-m", item.ID)
					head := gitCmd(dir, "rev-parse", "HEAD")
					if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, head); err != nil {
						return nil, err
					}
					sctx.Run.HeadSHA = head
					repaired = append(repaired, item.ID)
					return &StepOutcome{RestartFrom: types.StepReview}, nil
				}
				monitorEntries++
				if monitorEntries == 1 || monitorEntries == 4 {
					id := "ordinary-auto"
					if monitorEntries == 4 {
						id = "budget-must-not-reset"
					}
					raw, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: id, Description: "ordinary CI defect", Severity: types.FindingSeverityError, Action: types.ActionAutoFix}}})
					return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: raw}, nil
				}
				if err := sctx.DB.SetRunCIReady(sctx.Run.ID, true); err != nil {
					return nil, err
				}
				sctx.CIReadinessChanged(true, false)
				c := cycle{head: gitCmd(remote, "rev-parse", "refs/heads/feature"), joined: make(chan struct{}), release: make(chan struct{})}
				cycles <- c
				<-sctx.Ctx.Done()
				close(c.joined)
				<-c.release
				return nil, sctx.Ctx.Err()
			}}}
			steps = append(steps, ci)
			cfg := &config.Config{AutoFix: config.AutoFix{CI: 1}}
			onEvent := func(event ipc.Event) {
				if event.StepName != nil && *event.StepName == types.StepCI && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					parked <- event
				}
			}
			executor := NewExecutor(database, p, cfg, nil, steps, onEvent)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- executor.Execute(ctx, run, repo, dir) }()
			waitPark := func(id string) {
				t.Helper()
				for {
					select {
					case event := <-parked:
						if event.Findings != nil {
							f, err := types.ParseFindingsJSON(*event.Findings)
							if err != nil {
								t.Fatal(err)
							}
							for _, item := range f.Items {
								if item.ID == id {
									return
								}
							}
						}
					case <-ctx.Done():
						t.Fatalf("gate for %s not observed", id)
					}
				}
			}
			var published []string
			var ciID string
			lastRound := 0
			for i, id := range []string{"late-A", "late-B"} {
				var c cycle
				select {
				case c = <-cycles:
				case <-ctx.Done():
					t.Fatal("fresh published monitor not entered")
				}
				published = append(published, c.head)
				before, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if before.CIReadyAt == nil || before.ReviewApprovedHeadSHA == nil || *before.ReviewApprovedHeadSHA != c.head || before.LastPushedSHA == nil || *before.LastPushedSHA != c.head {
					t.Fatal("fresh monitor lacks published validation/readiness")
				}
				finding := []types.Finding{{ID: id, Description: "requirement " + id, Severity: types.FindingSeverityError, UserInstructions: "guidance for " + id}}
				response := make(chan error, 1)
				if crash && i == 1 {
					executor.mu.Lock()
					err = executor.admitLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", c.head)
					executor.mu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				} else {
					go func() {
						response <- executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, finding, "", c.head)
					}()
				}
				select {
				case <-c.joined:
				case <-ctx.Done():
					t.Fatal("admission did not cancel monitor")
				}
				after, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.CIReadyAt != nil || after.ReviewApprovedHeadSHA != nil || after.HeadSHA != c.head || *after.PRURL != url || *after.LastPushedSHA != c.head || *after.PushGeneration != *before.PushGeneration || *after.PushRef != *before.PushRef || *after.PushTargetFingerprint != *before.PushTargetFingerprint {
					t.Fatal("admission failed to revoke authority or preserve custody")
				}
				results, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, sr := range results {
					if sr.StepName == types.StepCI {
						ciID = sr.ID
						if sr.FindingsJSON == nil {
							t.Fatal("admission missing recovery findings")
						}
						f, err := types.ParseFindingsJSON(*sr.FindingsJSON)
						if err != nil || len(f.Items) != 1 || f.Items[0].ID != id || f.Items[0].UserInstructions != "guidance for "+id {
							t.Fatalf("pending gate replayed history: %+v %v", f, err)
						}
					}
				}
				rounds, err := database.GetRoundsByStep(ciID)
				if err != nil {
					t.Fatal(err)
				}
				if len(rounds) == 0 || rounds[len(rounds)-1].Round <= lastRound {
					t.Fatal("admission reset durable rounds")
				}
				lastRound = rounds[len(rounds)-1].Round
				if i == 1 && len(rounds) < 5 {
					t.Fatal("second admission discarded historical rounds")
				}
				if crash && i == 1 {
					if len(repaired) != 2 {
						t.Fatal("second fixer ran before recovery")
					}
					if err := database.Close(); err != nil {
						t.Fatal(err)
					}
					close(c.release)
					select {
					case err := <-completed:
						if err == nil {
							t.Fatal("simulated lost process completed")
						}
					case <-ctx.Done():
						t.Fatal("old executor did not stop")
					}
					database, err = db.Open(p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer database.Close()
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateRecoveredRun(database, run, steps); err != nil {
						t.Fatal(err)
					}
					executor = NewExecutor(database, p, cfg, nil, steps, onEvent)
					go func() { completed <- executor.Resume(ctx, run, repo, dir) }()
					waitPark("late-B")
					if len(repaired) != 2 {
						t.Fatal("recovery invoked fixer before explicit response")
					}
					if err := executor.Respond(types.StepCI, types.ActionFix, []string{"late-B"}); err != nil {
						t.Fatal(err)
					}
				} else {
					close(c.release)
					select {
					case err := <-response:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal("admission handoff did not finish")
					}
				}
			}
			waitPark("budget-must-not-reset")
			if !reflect.DeepEqual(repaired, []string{"ordinary-auto", "late-A", "late-B"}) {
				t.Fatalf("repair selection replayed: %v", repaired)
			}
			final, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			published = append(published, gitCmd(remote, "rev-parse", "refs/heads/feature"))
			if len(published) != 3 || published[0] == published[1] || published[1] == published[2] || final.LastPushedSHA == nil || *final.LastPushedSHA != published[2] || *final.PRURL != url || final.ID != run.ID {
				t.Fatal("two repairs did not publish distinct heads under the same custody")
			}
			wantHeads := append([]string{initial}, published...)
			if len(visits) != len(wantHeads)*len(names) {
				t.Fatalf("validation cycles missing: %v", visits)
			}
			for i, v := range visits {
				if v.step != names[i%len(names)] || v.head != wantHeads[i/len(names)] {
					t.Fatalf("validation/publication reordered: %v", visits)
				}
			}
			for _, id := range []string{"late-A", "late-B"} {
				if got := gitCmd(remote, "show", "refs/heads/feature:"+id+".txt"); got != "guidance for "+id {
					t.Fatalf("published repair lost current guidance: %s", got)
				}
			}
			rounds, err := database.GetRoundsByStep(ciID)
			if err != nil {
				t.Fatal(err)
			}
			autoSelections, historyA, historyB := 0, false, false
			previous := 0
			for _, round := range rounds {
				if round.Round <= previous {
					t.Fatal("rounds are not monotonic")
				}
				previous = round.Round
				if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
					autoSelections++
				}
				if round.FindingsJSON != nil {
					f, err := types.ParseFindingsJSON(*round.FindingsJSON)
					if err != nil {
						t.Fatal(err)
					}
					for _, item := range f.Items {
						historyA = historyA || item.ID == "late-A"
						historyB = historyB || item.ID == "late-B"
					}
				}
			}
			if autoSelections != 1 || !historyA || !historyB {
				t.Fatal("cycles lost history or reset automatic repair budget")
			}
			pending, err := database.GetStepResult(ciID)
			if err != nil || pending.FindingsJSON == nil {
				t.Fatal("budget gate missing")
			}
			f, err := types.ParseFindingsJSON(*pending.FindingsJSON)
			if err != nil || len(f.Items) != 1 || f.Items[0].ID != "budget-must-not-reset" {
				t.Fatalf("old amendments replayed at final gate: %+v", f)
			}
			cancel()
			select {
			case <-completed:
			case <-time.After(5 * time.Second):
				t.Fatal("executor did not stop")
			}
		})
	}
}

func TestExecutorLateCIRefusesDirectFixMonitorWithoutMutation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realGit, "init", "-b", "feature")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	cmd = exec.Command(realGit, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "published")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	cmd = exec.Command(realGit, "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	step := &lateAdmissionTestStep{adaptiveCallStep: &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"ordinary","severity":"error","description":"ordinary failure","action":"auto-fix"}]}`}, nil
		}
		if err := sctx.MarkRunning(); err != nil {
			return nil, err
		}
		if err := sctx.DB.SetRunCIReady(run.ID, true); err != nil {
			return nil, err
		}
		close(entered)
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}}}
	executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, dir) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("direct fix monitor not entered")
	}
	before, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.CIReadyAt == nil || len(beforeSteps) != 1 || beforeSteps[0].Status != types.StepStatusRunning {
		t.Fatal("fixture is not a running monitor inside a fix execution")
	}
	beforeRounds, err := database.GetRoundsByStep(beforeSteps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.RespondToLateCIFinding(run.ID, types.StepCI, types.ActionFix, nil, nil, []types.Finding{{ID: "unsafe", Description: "late requirement"}}, "", head); err == nil {
		t.Fatal("direct fixing monitor accepted an amendment")
	}
	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRounds, err := database.GetRoundsByStep(beforeSteps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeSteps, afterSteps) || !reflect.DeepEqual(beforeRounds, afterRounds) {
		t.Fatal("refused direct-fix amendment mutated authority, gate or history")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct fix monitor did not stop")
	}
}

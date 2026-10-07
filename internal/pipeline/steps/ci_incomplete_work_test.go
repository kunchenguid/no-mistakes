package steps

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func assertIncompleteCIRefusal(t *testing.T, raw string) {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings.Items {
		if finding.ID == "ci-incomplete-work" && finding.ActionOrDefault() == types.ActionAskUser && finding.Description != "" {
			return
		}
	}
	t.Fatalf("no actionable incomplete-work refusal: %+v", findings)
}

func TestCIStep_UnfinishedRefusalFindings(t *testing.T) {
	for _, operation := range []string{"merge", "rebase", "rebase-apply", "unmerged", "unreadable"} {
		for _, source := range []string{"empty", "previous", "restored"} {
			t.Run(operation+"/"+source, func(t *testing.T) {
				f := newUnconcludedFixture(t, true, func(string) {}, `{}`)
				switch operation {
				case "rebase":
					unconcludedGit(f.dir, "rebase", "main")
				case "rebase-apply":
					unconcludedGit(f.dir, "-c", "rebase.backend=apply", "rebase", "main")
				default:
					leaveMerge(f.dir)
				}
				if !unfinishedRepairOperation(f.sctx) {
					t.Fatal("fixture did not leave unfinished work")
				}
				if operation == "unmerged" {
					marker := gitCmd(t, f.dir, "rev-parse", "--git-path", "MERGE_HEAD")
					if !filepath.IsAbs(marker) {
						marker = filepath.Join(f.dir, marker)
					}
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
				}
				beforeHead := f.localHead(t)
				beforeIndex := gitCmd(t, f.dir, "ls-files", "--stage")
				beforeBytes, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
				if err != nil {
					t.Fatal(err)
				}
				indexPath := gitCmd(t, f.dir, "rev-parse", "--git-path", "index")
				if !filepath.IsAbs(indexPath) {
					indexPath = filepath.Join(f.dir, indexPath)
				}
				if operation == "unreadable" {
					if err := os.WriteFile(indexPath, []byte("broken index"), 0600); err != nil {
						t.Fatal(err)
					}
					marker := gitCmd(t, f.dir, "rev-parse", "--git-path", "MERGE_HEAD")
					if !filepath.IsAbs(marker) {
						marker = filepath.Join(f.dir, marker)
					}
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
				}
				rawIndex, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				previous := f.sctx.PreviousFindings
				f.sctx.PreviousFindings = ""
				if source == "previous" {
					f.sctx.PreviousFindings = previous
				} else if source == "restored" {
					sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.sctx.DB.SetStepFindings(sr.ID, previous); err != nil {
						t.Fatal(err)
					}
					f.sctx.StepResultID = sr.ID
				}
				out, err := (&CIStep{}).Execute(f.sctx)
				if err != nil || out == nil || !out.NeedsApproval {
					t.Fatalf("Execute refusal=%#v err=%v", out, err)
				}
				assertIncompleteCIRefusal(t, out.Findings)
				findings, _ := types.ParseFindingsJSON(out.Findings)
				wantCount := 1
				if source != "empty" {
					wantCount = 2
				}
				if len(findings.Items) != wantCount {
					t.Fatalf("prior/restored finding lost: %+v", findings)
				}
				afterBytes, err := os.ReadFile(filepath.Join(f.dir, "feature.txt"))
				if err != nil || !reflect.DeepEqual(beforeBytes, afterBytes) || f.localHead(t) != beforeHead || f.remoteHead(t) != f.headSHA {
					t.Fatal("refusal changed bytes/HEAD/publication")
				}
				afterIndex, err := os.ReadFile(indexPath)
				if err != nil || !reflect.DeepEqual(rawIndex, afterIndex) {
					t.Fatal("refusal changed raw index bytes")
				}
				if operation != "unreadable" && gitCmd(t, f.dir, "ls-files", "--stage") != beforeIndex {
					t.Fatal("refusal changed index")
				}
				t.Logf("source=%s operation=%s local=%s published=%s findings=%s", source, operation, beforeHead, f.remoteHead(t), out.Findings)
			})
		}
	}
}

func TestCIRepair_IncompleteWorkFindingClearsOnAcceptance(t *testing.T) {
	f := newUnconcludedFixture(t, true, func(string) {}, `{}`)
	leaveMerge(f.dir)
	out, err := (&CIStep{}).Execute(f.sctx)
	if err != nil || out == nil {
		t.Fatalf("initial refusal=%#v %v", out, err)
	}
	assertIncompleteCIRefusal(t, out.Findings)
	// Resolve the operation explicitly. The next accepted repair selects only
	// the original check; the synthetic refusal is deferred, not selected.
	gitCmd(t, f.dir, "merge", "--abort")
	initial, err := types.ParseFindingsJSON(out.Findings)
	if err != nil {
		t.Fatal(err)
	}
	deferred := types.FilterFindings(initial, []string{"ci-incomplete-work"})
	f.sctx.DeferredFindings, err = types.MarshalFindingsJSON(deferred)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.Agent = &mockAgent{name: "accept", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"summary":"accepted resolved work","code_change_needed":true}`)}, nil
	}}
	if err := os.WriteFile(filepath.Join(f.dir, "repair.txt"), []byte("accepted repair"), 0600); err != nil {
		t.Fatal(err)
	}
	f.sctx.Config.CI.RevalidateRepairs = true
	out = f.repairRound(t)
	if out == nil || out.RestartFrom != types.StepReview {
		t.Fatalf("accepted repair=%#v", out)
	}
	if out.Findings != "" || out.NeedsApproval {
		t.Fatalf("accepted repair carried a stale refusal gate: %#v", out)
	}
	t.Logf("accepted restart=%s findings=%s", out.RestartFrom, out.Findings)
}

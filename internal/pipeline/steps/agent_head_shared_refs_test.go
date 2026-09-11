package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentHeadRecordingPreservesSharedGateRefs(t *testing.T) {
	for _, recorder := range []string{"review", "ci"} {
		for _, relation := range []string{"descendant", "divergent"} {
			t.Run(recorder+"/"+relation, func(t *testing.T) {
				t.Setenv("NM_HOME", t.TempDir())
				source, baseSHA, submittedHead := setupGitRepo(t)
				upstream := t.TempDir()
				gitCmd(t, upstream, "init", "--bare")
				gitCmd(t, source, "remote", "add", "origin", upstream)
				gitCmd(t, source, "push", "origin", "feature")
				gateDir := filepath.Join(t.TempDir(), "gate.git")
				gitCmd(t, source, "clone", "--bare", source, gateDir)
				gitCmd(t, gateDir, "remote", "set-url", "origin", upstream)
				workDir := filepath.Join(t.TempDir(), "detached")
				gitCmd(t, gateDir, "worktree", "add", "--detach", workDir, submittedHead)
				writeCommit := func(dir, name, content string) string {
					t.Helper()
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", name)
					gitCmd(t, dir, "commit", "-m", name)
					return gitCmd(t, dir, "rev-parse", "HEAD")
				}
				agentHead := writeCommit(workDir, "agent.txt", "agent fix\n")
				if relation == "descendant" {
					gitCmd(t, source, "fetch", gateDir, agentHead)
					gitCmd(t, source, "checkout", "--detach", agentHead)
				}
				firstPrivateHead := writeCommit(source, "private-one.txt", "first private change\n")
				privateHead := writeCommit(source, "private-two.txt", "second private change\n")
				gitCmd(t, gateDir, "fetch", source, privateHead+":refs/heads/feature")
				if got := gitCmd(t, workDir, "rev-parse", "refs/heads/feature"); got != privateHead {
					t.Fatalf("detached fixture does not see the shared branch: %s", got)
				}
				if status := gitStatusPorcelain(t, workDir); status != "" {
					t.Fatalf("agent fixture must be clean: %s", status)
				}
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, workDir, baseSHA, submittedHead, config.Commands{})
				sctx.GateDir = gateDir
				sctx.Repo.UpstreamURL = upstream
				recordReviewApproval(t, sctx, submittedHead)
				if recorder == "review" {
					sctx.ReviewStartingHeadSHA = submittedHead
					if err := commitAgentFixes(sctx, types.StepReview, "record agent fixes", ""); err != nil {
						t.Fatal(err)
					}
				} else {
					sctx.Config.CI.RevalidateRepairs = true
					repair, err := (&CIStep{}).commitRepair(sctx, "record CI repair")
					if err != nil || !repair.HeadAdvanced || !repair.Revalidate {
						t.Fatalf("CI recording = %+v, err = %v", repair, err)
					}
				}
				assertRecorded := func() {
					t.Helper()
					persisted, err := sctx.DB.GetRun(sctx.Run.ID)
					if err != nil || persisted == nil || persisted.HeadSHA != agentHead || sctx.Run.HeadSHA != agentHead {
						t.Fatalf("agent head not recorded: memory=%s persisted=%+v err=%v", sctx.Run.HeadSHA, persisted, err)
					}
					if sctx.Run.SubmittedHeadSHA == nil || *sctx.Run.SubmittedHeadSHA != submittedHead || persisted.SubmittedHeadSHA == nil || *persisted.SubmittedHeadSHA != submittedHead {
						t.Fatalf("recording changed Decision 41-A's exact submission boundary: %+v", persisted)
					}
					rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
					if err != nil || rng == nil || rng.FromSHA != submittedHead || rng.ToSHA != agentHead || rng.SourceRunID != sctx.Run.ID {
						t.Fatalf("uncertified range = %+v, err = %v, want %s..%s", rng, err, submittedHead, agentHead)
					}
				}
				assertPrivateRef := func() {
					t.Helper()
					for _, dir := range []string{workDir, gateDir} {
						if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != privateHead {
							t.Fatalf("shared ref overwritten in %s: got %s, want %s", dir, got, privateHead)
						}
					}
					if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
						t.Fatalf("preserved private head was archived: %s", tags)
					}
				}
				assertRecorded()
				assertPrivateRef()
				persisted, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil || persisted.LastPushedSHA != nil {
					t.Fatalf("local recording published a head: %+v err=%v", persisted, err)
				}
				if recorder == "ci" && (persisted.ReviewApprovedHeadSHA != nil || sctx.Run.ReviewApprovedHeadSHA != nil) {
					t.Fatal("CI recording did not revoke stale review authority")
				}
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != submittedHead {
					t.Fatalf("head recording moved upstream: %s", got)
				}
				recordReviewApproval(t, sctx, agentHead)
				_, err = (&PushStep{}).Execute(sctx)
				wantRemote := agentHead
				if relation == "divergent" {
					wantRemote = submittedHead
					if err == nil {
						t.Fatal("publication accepted divergent unique private content")
					}
					for _, want := range []string{firstPrivateHead, privateHead, "private-one.txt", "private-two.txt"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("publication refusal omitted at-risk commit %q: %v", want, err)
						}
					}
				} else if err != nil {
					t.Fatalf("publication refused preserved descendant: %v", err)
				}
				assertRecorded()
				assertPrivateRef()
				rng, rangeErr := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
				t.Logf("Handoff: recorder=%s relation=%s publication_error=%v; recorded_head=%s; uncertified_range=%+v (read_error=%v); preserved_gate_head=%s; upstream=%s", recorder, relation, err, sctx.Run.HeadSHA, rng, rangeErr, gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"), gitCmd(t, upstream, "rev-parse", "refs/heads/feature"))
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != wantRemote {
					t.Fatalf("upstream = %s, want %s", got, wantRemote)
				}
				if got := gitCmd(t, workDir, "rev-parse", "HEAD"); got != agentHead {
					t.Fatalf("recording moved detached agent HEAD: %s", got)
				}
			})
		}
	}
}

func TestAgentHeadRecordingUpdatesNonSharedWorktreeRef(t *testing.T) {
	for _, recorder := range []string{"review", "ci"} {
		t.Run(recorder, func(t *testing.T) {
			dir, baseSHA, submittedHead := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
			gateDir := setupGateMirror(t, sctx)
			gitCmd(t, gateDir, "fetch", dir, submittedHead+":refs/heads/feature")
			gitCmd(t, dir, "checkout", "--detach", submittedHead)
			if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent fix\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "agent.txt")
			gitCmd(t, dir, "commit", "-m", "agent fix")
			agentHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if recorder == "review" {
				if err := commitAgentFixes(sctx, types.StepReview, "record agent fixes", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				sctx.Config.CI.RevalidateRepairs = true
				if _, err := (&CIStep{}).commitRepair(sctx, "record CI repair"); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != agentHead {
				t.Fatalf("non-shared branch = %s, want agent head %s", got, agentHead)
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != submittedHead {
				t.Fatalf("non-shared bookkeeping moved gate ref to %s", got)
			}
			persisted, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || persisted == nil || persisted.HeadSHA != agentHead || sctx.Run.HeadSHA != agentHead {
				t.Fatalf("head recording = %+v, err = %v, want %s", persisted, err, agentHead)
			}
			rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
			if err != nil || rng == nil || rng.FromSHA != submittedHead || rng.ToSHA != agentHead {
				t.Fatalf("uncertified range = %+v, err = %v", rng, err)
			}
		})
	}
}

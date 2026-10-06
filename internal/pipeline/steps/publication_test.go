package steps

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReboundPublicationPushesOnlyAppendOnlyToExistingBranch(t *testing.T) {
	for _, scenario := range []string{"append", "advanced-managed", "rewrite", "missing", "advanced", "deleted-during-push", "closed-pr", "foreign-pr", "changed-target"} {
		t.Run(scenario, func(t *testing.T) {
			remote := filepath.ToSlash(t.TempDir())
			// The publication identity is a slug derived from the fork URL, and
			// a Windows temp path has no `/` after its drive letter, so the slug
			// comes out empty and every provider read refuses. Register the same
			// directory with forward slashes: the slug is then the same string on
			// every platform, and Git accepts either form as a local remote.
			gitCmd(t, remote, "init", "--bare")
			dir, base, submitted := setupGitRepo(t)
			gitCmd(t, dir, "push", remote, "refs/heads/main:refs/heads/main")
			gitCmd(t, dir, "push", remote, "HEAD:refs/heads/existing")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "validated descendant")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "remote", "add", "origin", remote)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			repo, err := sctx.DB.UpdateRepoMetadataWithFork(sctx.Repo.ID, "https://github.com/test/repo", remote, "main")
			if err != nil {
				t.Fatal(err)
			}
			sctx.Repo = repo
			if err := sctx.DB.RebindPublication(repo, sctx.Run, "existing", "https://github.com/test/repo/pull/1", branchsync.TargetFingerprint(repo.PushURL())); err != nil {
				t.Fatal(err)
			}
			// The executor still holds its original custody-only Run. Publication
			// must read the destination from the database at the actual boundary.
			if sctx.Run.PublicationBranch != nil {
				t.Fatal("fixture accidentally bypasses durable reload")
			}
			sctx.Env, _ = fakeGH(t, "https://github.com/test/repo/pull/1")
			sctx.Env = append(sctx.Env, fmt.Sprintf(`FAKE_CLI_PR_LIST_JSON=[{"number":1,"url":"https://github.com/test/repo/pull/1","headRefName":"existing","headRepository":{"nameWithOwner":%q}}]`, github.RepoSlug(remote)))
			sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+submitted)
			sctx.Env = append(sctx.Env, fmt.Sprintf(`FAKE_CLI_PR_PUBLICATION_JSON={"headRefOid":%q,"headRefName":"existing","headRepository":{"nameWithOwner":%q}}`, submitted, github.RepoSlug(remote)))
			recordReviewApproval(t, sctx, head)
			gate := setupGateMirror(t, sctx)
			gitCmd(t, gate, "fetch", dir, submitted+":refs/heads/feature")
			gitCmd(t, dir, "checkout", "--detach", head)
			before := submitted
			switch scenario {
			case "advanced-managed":
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "later reviewed correction")
				head = gitCmd(t, dir, "rev-parse", "HEAD")
				recordReviewApproval(t, sctx, head)
			case "deleted-during-push":
				path, _ := envValue(sctx.Env, "PATH")
				linkTestBinary(t, filepath.SplitList(path)[0], "git")
				sctx.Env = append(sctx.Env, "FAKE_CLI_MODE=gh-with-intervening-push", "FAKE_CLI_STATE=OPEN", "FAKE_CLI_REAL_GIT="+testGitExecutable, "FAKE_CLI_INTERLOPER_DIR="+dir, "FAKE_CLI_INTERLOPER_REMOTE="+remote, "FAKE_CLI_INTERLOPER_REF=:refs/heads/existing")
			case "foreign-pr":
				sctx.Env = append(sctx.Env, fmt.Sprintf(`FAKE_CLI_PR_PUBLICATION_JSON={"headRefOid":%q,"headRefName":"existing","headRepository":{"nameWithOwner":"other/repo"}}`, submitted))
			case "closed-pr":
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_STATE=CLOSED")
			case "changed-target":
				if _, err := sctx.DB.UpdateRepoMetadataWithFork(repo.ID, repo.UpstreamURL, "https://github.com/other/repo", "main"); err != nil {
					t.Fatal(err)
				}
			case "rewrite":
				gitCmd(t, dir, "reset", "--hard", base)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "divergent history")
				head = gitCmd(t, dir, "rev-parse", "HEAD")
				sctx.Run.HeadSHA = head
				recordReviewApproval(t, sctx, head)
			case "missing":
				gitCmd(t, remote, "update-ref", "-d", "refs/heads/existing")
			case "advanced":
				gitCmd(t, dir, "checkout", "-b", "other", submitted)
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "outside publisher")
				before = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "push", remote, "HEAD:refs/heads/existing")
				gitCmd(t, dir, "checkout", "feature")
			}
			_, err = (&PushStep{}).Execute(sctx)
			if scenario == "append" || scenario == "advanced-managed" {
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, remote, "rev-parse", "refs/heads/existing"); got != head {
					t.Fatalf("wrong published head: %s", got)
				}
				if sctx.Run.HeadSHA != head || sctx.Run.Branch != "refs/heads/feature" {
					t.Fatalf("executor state did not follow publication: %+v", sctx.Run)
				}
				if published, err := publishedBranchHead(sctx); err != nil || published != head {
					t.Fatalf("CI monitored the wrong branch: %s %v", published, err)
				}
				if got := gitCmd(t, gate, "rev-parse", "refs/heads/feature"); got != head {
					t.Fatalf("custody ref left stale: %s, want %s", got, head)
				}
				if _, err := git.Run(sctx.Ctx, gate, "rev-parse", "--verify", "refs/heads/existing"); err == nil {
					t.Fatal("destination ref created in the custody mirror")
				}
				r, _ := sctx.DB.GetRun(sctx.Run.ID)
				if scenario == "append" {
					sctx.Env = append(sctx.Env, "FAKE_CLI_PR_LIST_JSON=[]")
					if _, err := (&PRStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "replacement PR") {
						t.Fatalf("missing rebound PR was not refused: %v", err)
					}
					if len(sctx.Agent.(*mockAgent).calls) != 0 {
						t.Fatal("drafted a replacement PR")
					}
				}
				if r.Branch != "refs/heads/feature" || r.PushRef == nil || *r.PushRef != "refs/heads/existing" {
					t.Fatalf("binding/custody = %+v", r)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe rebound push accepted")
				}
				if !strings.Contains(err.Error(), "rebound publication") && !strings.Contains(err.Error(), "refusing") {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if scenario != "missing" && scenario != "deleted-during-push" {
					if got := gitCmd(t, remote, "rev-parse", "refs/heads/existing"); got != before {
						t.Fatal("existing PR history replaced")
					}
				}
				if scenario == "deleted-during-push" {
					if _, err := git.Run(sctx.Ctx, remote, "rev-parse", "--verify", "refs/heads/existing"); err == nil {
						t.Fatal("deleted PR branch was recreated")
					}
				}
			}
			if _, err := git.Run(sctx.Ctx, remote, "rev-parse", "--verify", "refs/heads/feature"); err == nil {
				t.Fatal("publication created a replacement source branch")
			}
		})
	}
}

func TestPublicationHostUsesRegisteredUpstreamWithRepointedOrigin(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", "https://github.com/unrelated/repo")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Env, _ = fakeGH(t, "https://github.com/test/repo/pull/1")
	host, reason := PublicationHost(sctx)
	if host == nil {
		t.Fatal(reason)
	}
	pr, err := host.FindPR(sctx.Ctx, "existing", "")
	if err != nil || pr == nil || pr.URL != "https://github.com/test/repo/pull/1" {
		t.Fatalf("PR proof routed outside registration: %+v %v", pr, err)
	}
	if sctx.Repo.URLsVerified {
		t.Fatal("provider scoping mutated the caller's repository snapshot")
	}
}

func TestReboundCICreditsOnlyThePublishedRunHead(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
			sctx.Repo.URLsVerified = true
			prURL := "https://github.com/test/repo/pull/42"
			if err := sctx.DB.RebindPublication(sctx.Repo, sctx.Run, "existing", prURL, branchsync.TargetFingerprint(sctx.Repo.PushURL())); err != nil {
				t.Fatal(err)
			}
			providerHead := base
			if published {
				providerHead = head
			}
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
			sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+providerHead,
				fmt.Sprintf(`FAKE_CLI_PR_PUBLICATION_JSON={"headRefOid":%q,"headRefName":"existing","headRepository":{"nameWithOwner":"test/repo"}}`, providerHead))
			recordReviewApproval(t, sctx, head)
			step := (&CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true })
			reason, err := step.VerifyApprovalOverride(sctx)
			if err != nil || (reason == "") != published {
				t.Fatalf("approval reason=%q err=%v published=%v", reason, err, published)
			}
			if !published {
				if !strings.Contains(reason, "unpublished") {
					t.Fatal(reason)
				}
				outcome, err := step.Execute(sctx)
				if err != nil || outcome == nil || !outcome.NeedsApproval {
					t.Fatalf("unpublished CI was credited: %+v %v", outcome, err)
				}
				findings, _ := types.ParseFindingsJSON(outcome.Findings)
				if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "unpublished") {
					t.Fatalf("wrong condition: %+v", findings)
				}
				resolved, err := step.ReconcileApprovalGate(sctx)
				if resolved || err == nil {
					t.Fatalf("unpublished gate reconciled: %v %v", resolved, err)
				}
			}
		})
	}
}

func TestReboundCIRefusesAutomaticSkipWithoutPublicationProof(t *testing.T) {
	for _, rebound := range []bool{false, true} {
		for _, scenario := range []string{"provider", "authentication", "identity"} {
			t.Run(fmt.Sprintf("rebound=%v/%s", rebound, scenario), func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
				sctx.Repo.URLsVerified = true
				prURL := "https://github.com/test/repo/pull/42"
				if rebound {
					if err := sctx.DB.RebindPublication(sctx.Repo, sctx.Run, "existing", prURL, branchsync.TargetFingerprint(sctx.Repo.PushURL())); err != nil {
						t.Fatal(err)
					}
				}
				sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+base)
				switch scenario {
				case "provider":
					sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderUnknown}
				case "authentication":
					sctx.Env = append(sctx.Env, "FAKE_CLI_AUTH_ERR=authentication expired")
				case "identity":
					if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, ""); err != nil {
						t.Fatal(err)
					}
				}
				outcome, err := (&CIStep{}).Execute(sctx)
				if rebound {
					if err == nil || outcome != nil {
						t.Fatalf("unproven rebound publication completed: %+v %v", outcome, err)
					}
					if !strings.Contains(err.Error(), "rebound CI publication cannot be verified") {
						t.Fatal(err)
					}
				} else if err != nil || outcome == nil || !outcome.Skipped {
					t.Fatalf("ordinary CI skip changed: %+v %v", outcome, err)
				}
				got, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.LastPushedSHA != nil || got.CIReadyAt != nil {
					t.Fatalf("skip or refusal credited publication or CI: %+v", got)
				}
			})
		}
	}
}

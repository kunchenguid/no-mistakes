package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PR fixtures model their preceding successful Push step explicitly. Other
// pipeline-step fixtures retain their original unpublished state.
func newPRTestContext(t *testing.T, ag agent.Agent, dir, base, head string, cmds config.Commands, branches ...string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContext(t, ag, dir, base, head, cmds)
	repo, err := sctx.DB.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	branch := "refs/heads/feature"
	if len(branches) > 0 {
		branch = normalizedBranchRef(branches[0])
	}
	run, err := sctx.DB.InsertRun(repo.ID, branch, head, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Repo, sctx.Run = repo, run
	recordPRTestPush(t, sctx)
	return sctx
}

func capturePRProofNotice(t *testing.T, sctx *pipeline.StepContext, provider string) func(string) {
	t.Helper()
	var logs []string
	log := sctx.Log
	sctx.Log = func(line string) {
		logs = append(logs, line)
		log(line)
	}
	return func(body string) {
		t.Helper()
		notice := "PR head proof unavailable for provider " + provider + "; the exact pushed head was not verified."
		if !strings.Contains(strings.Join(logs, "\n"), notice) || !strings.Contains(body, notice) {
			t.Fatalf("missing provider proof limitation: logs=%v body=%s", logs, body)
		}
	}
}

func TestPRStep_ProviderWithoutProofPublishesWithNotice(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Env, _ = fakeGH(t, "")
	completeCleanReview(t, sctx)
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		t.Fatal(reason)
	}
	step := &PRStep{host: struct{ scm.Host }{host}}
	out, err := step.Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("provider without proof failed: %+v %v", out, err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "PR head proof unavailable") || !strings.Contains(readPRBodyFile(t, envEntry(sctx.Env, "FAKE_CLI_PR_BODY_FILE")), "PR head proof unavailable") {
		t.Fatal("provider proof limitation was not recorded in output and body")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.PRURL == nil || *run.PRURL != out.PRURL || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
		t.Fatalf("missing publication or lost custody: %+v %v", run, err)
	}
}

func TestPRStep_GitLabPublishesWithoutHeadProof(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo"
	sctx.Env, _ = fakeGlab(t, "")
	recordPRTestPush(t, sctx)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("provider without proof failed: %+v %v", out, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.PRURL == nil || *run.PRURL != out.PRURL || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
		t.Fatalf("missing publication or lost custody: %+v %v", run, err)
	}
}

func TestPRStep_RejectsForkPushOnADifferentForge(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.ForkURL = "https://other.example/test/repo.git"
	sctx.Env, _ = fakeGH(t, "")
	raw, err := json.Marshal(map[string]any{
		"number": 99, "html_url": "https://github.com/test/repo/pull/99", "state": "open", "merged": false,
		"base": map[string]any{"repo": map[string]string{"full_name": "test/repo"}},
		"head": map[string]any{"ref": "feature", "sha": head, "repo": map[string]any{"full_name": "test/repo", "owner": map[string]string{"login": "test"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(sctx.Env, "FAKE_CLI_PR_PROOF_JSON="+string(raw))
	recordPRTestPush(t, sctx)
	out, err := (&PRStep{}).Execute(sctx)
	if err == nil || out != nil {
		t.Fatalf("PR on another forge certified the push: %+v %v", out, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.PRURL != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
		t.Fatalf("unverified URL or lost push evidence: %+v %v", run, err)
	}
}

func recordPRTestPush(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	kind := "upstream"
	if sctx.Repo.ForkURL != "" {
		kind = "fork"
	}
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: sctx.Run.HeadSHA, TargetKind: kind, TargetFingerprint: branchsync.TargetFingerprint(resolvePushURL(sctx)), Ref: normalizedBranchRef(sctx.Run.Branch)}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPublishedPR_ProviderWithoutProofKeepsPriorBehavior(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	host := struct{ scm.Host }{}
	if err := verifyPublishedPR(sctx, host, nil); err != nil {
		t.Fatalf("missing optional capability blocked publication: %v", err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != head || run.PRURL != nil {
		t.Fatalf("refusal changed publication evidence: %+v %v", run, err)
	}
}

// The oracle is issue #1433's successful-push contract. Real commits and the
// durable SQLite push binding are independent of the synthetic forge response.
func TestPRStep_ExactPushedHeadPostcondition(t *testing.T) {
	for _, route := range []string{"create", "update", "persisted"} {
		for _, fault := range []string{"", "wrong_sha", "moved_head", "missing_sha", "wrong_repository", "wrong_source_repository", "wrong_owner", "wrong_branch", "closed", "merged", "unreadable", "lookup_failed", "missing_push", "active_push", "wrong_push_target", "wrong_push_ref"} {
			t.Run(route+"/"+fault, func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				number := 42
				url := "https://github.com/test/repo/pull/42"
				discovered := url
				if route == "create" {
					discovered, number, url = "", 99, "https://github.com/test/repo/pull/99"
				}
				if route == "persisted" {
					sctx.Run.PRURL = &url
					if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
						t.Fatal(err)
					}
					discovered = ""
				}
				env, logFile := fakeGH(t, discovered)
				proof := map[string]any{
					"number": number, "html_url": url, "state": "open", "merged": false,
					"base": map[string]any{"repo": map[string]any{"full_name": "test/repo"}},
					"head": map[string]any{"ref": "feature", "sha": head, "repo": map[string]any{"full_name": "test/repo", "owner": map[string]any{"login": "test"}}},
				}
				prHead := proof["head"].(map[string]any)
				initialProof, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "wrong_sha", "moved_head":
					prHead["sha"] = base
				case "missing_sha":
					delete(prHead, "sha")
				case "wrong_repository":
					proof["base"] = map[string]any{"repo": map[string]any{"full_name": "other/repo"}}
				case "wrong_source_repository":
					prHead["repo"].(map[string]any)["full_name"] = "test/other"
				case "wrong_owner":
					prHead["repo"].(map[string]any)["owner"] = map[string]any{"login": "other"}
				case "wrong_branch":
					prHead["ref"] = "other"
				case "closed":
					proof["state"] = "closed"
				case "merged":
					proof["merged"] = true
				}
				raw, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "unreadable" {
					raw = []byte("{")
				}
				env = append(env, "FAKE_CLI_PR_PROOF_JSON="+string(raw))
				if fault == "lookup_failed" {
					env = append(env, "FAKE_CLI_PR_PROOF_FAIL=1")
				}
				if fault == "moved_head" {
					env = append(env, "FAKE_CLI_PR_PROOF_AFTER_UPDATE=1", "FAKE_CLI_PR_PROOF_INITIAL_JSON="+string(initialProof))
				}
				sctx.Env = env
				binding := db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(resolvePushURL(sctx)), Ref: "refs/heads/feature"}
				if fault == "wrong_push_target" {
					binding.TargetFingerprint = strings.Repeat("0", 64)
				}
				if fault == "wrong_push_ref" {
					binding.Ref = "refs/heads/other"
				}
				if fault != "missing_push" {
					if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, binding); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "active_push" {
					if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "missing_push" || fault == "active_push" || fault == "wrong_push_target" || fault == "wrong_push_ref" {
					if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
						t.Fatal(err)
					}
				}
				var logs []string
				sctx.Log = func(line string) { logs = append(logs, line) }
				outcome, executeErr := (&PRStep{}).Execute(sctx)
				if fault == "" {
					if executeErr != nil || outcome == nil || outcome.PRURL != url {
						t.Fatalf("matching push: outcome=%+v err=%v", outcome, executeErr)
					}
				} else {
					if executeErr == nil {
						t.Fatalf("unproven %s PR succeeded: %+v", fault, outcome)
					}
					if outcome != nil && outcome.PRURL != "" {
						t.Fatalf("unverified PR outcome: %+v", outcome)
					}
					for _, line := range logs {
						if strings.Contains(line, "created pull request:") {
							t.Fatalf("reported unverified PR: %s", line)
						}
					}
				}
				stored, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fault != "" && route != "persisted" && stored.PRURL != nil && *stored.PRURL != "" {
					t.Fatalf("persisted unverified URL: %s", *stored.PRURL)
				}
				if fault != "missing_push" && (stored.LastPushedSHA == nil || *stored.LastPushedSHA != head || stored.PushRef == nil || *stored.PushRef != binding.Ref) {
					t.Fatalf("lost push/custody evidence: %+v", stored)
				}
				if fault == "" && (stored.PRURL == nil || *stored.PRURL != url) {
					t.Fatalf("verified URL not persisted: %+v", stored.PRURL)
				}
				if fault == "missing_push" || fault == "active_push" || fault == "wrong_push_target" || fault == "wrong_push_ref" {
					calls, err := os.ReadFile(logFile)
					if err != nil {
						t.Fatal(err)
					}
					if stored.ClosingIssueRefsLockedAt != nil || strings.Contains(string(calls), "pr create") || strings.Contains(string(calls), "--method PATCH") {
						t.Fatalf("invalid binding claimed references or mutated the forge: %+v\n%s", stored, calls)
					}
				}
				if fault == "moved_head" && route != "create" {
					calls, err := os.ReadFile(logFile)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Count(string(calls), "api --hostname github.com repos/test/repo/pulls/42") != 2 {
						t.Fatalf("must reread after mutation: %s", calls)
					}
				}
			})
		}
	}
}

type prLagFixtureHost struct {
	scm.Host
	verify func(context.Context, *scm.PR, string, string) error
}

func (h *prLagFixtureHost) VerifyPRHead(ctx context.Context, pr *scm.PR, branch, head string) error {
	return h.verify(ctx, pr, branch, head)
}

func prLagPublishedContext(t *testing.T, scenario string) (*pipeline.StepContext, string, string) {
	t.Helper()
	dir, base, previous := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, previous, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "set-url", "origin", upstream)
	gitCmd(t, dir, "push", "-q", "origin", "main")
	sctx.Env, _ = fakeGH(t, "")
	recordReviewApproval(t, sctx, previous)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("next delivered commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "repair.txt")
	if scenario == "repair" {
		gitCmd(t, dir, "commit", "-q", "--amend", "-m", "repaired delivered commit")
	} else {
		gitCmd(t, dir, "commit", "-q", "-m", "next delivered commit")
	}
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if scenario == "new_run" {
		if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		run, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, head, base)
		if err != nil {
			t.Fatal(err)
		}
		sctx.Run = run
		sctx.Shared = &pipeline.RunShared{}
	} else {
		if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, head); err != nil {
			t.Fatal(err)
		}
		sctx.Run.HeadSHA = head
	}
	recordReviewApproval(t, sctx, head)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("%s publication: %v", scenario, err)
	}
	if sctx.Run.LastPushedSHA != nil {
		t.Fatal("executor snapshot unexpectedly acquired a push binding")
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("remote head = %s, want %s", got, head)
	}
	return sctx, previous, head
}

func TestPRStep_WaitsForPreviousPushedHead(t *testing.T) {
	for _, scenario := range []string{"new_run", "repair"} {
		for _, route := range []string{"create", "update", "persisted"} {
			t.Run(scenario+"/"+route, func(t *testing.T) {
				t.Parallel()
				sctx, previous, head := prLagPublishedContext(t, scenario)
				url := "https://github.com/test/repo/pull/42"
				discovered := url
				if route == "create" {
					discovered = ""
				}
				if route == "persisted" {
					discovered = ""
					sctx.Run.PRURL = &url
					if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
						t.Fatal(err)
					}
				}
				sctx.Env, _ = fakeGH(t, discovered)
				host, reason := buildHost(sctx, resolvedProvider(sctx))
				if host == nil {
					t.Fatal(reason)
				}
				calls := 0
				step := &PRStep{host: &prLagFixtureHost{Host: host, verify: func(_ context.Context, pr *scm.PR, branch, pushed string) error {
					calls++
					if pr == nil || branch != "feature" || pushed != head {
						t.Fatalf("unbound proof request: %+v %s %s", pr, branch, pushed)
					}
					if calls == 1 {
						return &scm.PRHeadChangedError{HeadSHA: previous}
					}
					return nil
				}}}
				out, err := step.Execute(sctx)
				if err != nil || out == nil || out.PRURL == "" || calls < 2 {
					t.Fatalf("lagging previous head did not converge: %+v %v calls=%d", out, err, calls)
				}
			})
		}
	}
}

func TestVerifyPublishedPR_RetriesOnlyPreviousHeadWithAHardCap(t *testing.T) {
	for _, fault := range []string{"previous", "unrelated", "missing", "lookup", "no_previous", "restart", "empty_previous", "unchanged", "wrong_run", "wrong_repo", "wrong_head", "wrong_target", "wrong_kind", "wrong_ref"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			sctx, previous, _ := prLagPublishedContext(t, "new_run")
			if fault == "no_previous" {
				sctx.Shared = &pipeline.RunShared{}
			} else if fault == "restart" {
				sctx.Shared = nil
			} else {
				publication, ok := sctx.Shared.PublishedPush()
				if !ok {
					t.Fatal("successful push produced no handoff")
				}
				switch fault {
				case "empty_previous":
					publication.PreviousHeadSHA = ""
				case "unchanged":
					publication.PreviousHeadSHA = publication.Binding.HeadSHA
				case "wrong_run":
					publication.RunID = "another-run"
				case "wrong_repo":
					publication.RepoID = "another-repo"
				case "wrong_head":
					publication.Binding.HeadSHA = previous
				case "wrong_target":
					publication.Binding.TargetFingerprint = strings.Repeat("0", 64)
				case "wrong_kind":
					publication.Binding.TargetKind = "fork"
				case "wrong_ref":
					publication.Binding.Ref = "refs/heads/other"
				}
				sctx.Shared.SetPublishedPush(publication)
			}
			calls := 0
			host := &prLagFixtureHost{verify: func(ctx context.Context, _ *scm.PR, _, _ string) error {
				calls++
				if ctx != sctx.Ctx {
					t.Fatal("proof read did not retain the caller's context")
				}
				switch fault {
				case "unrelated":
					return &scm.PRHeadChangedError{HeadSHA: strings.Repeat("a", 40)}
				case "missing":
					return &scm.PRHeadChangedError{}
				case "lookup":
					return errors.New("lookup failed")
				default:
					return &scm.PRHeadChangedError{HeadSHA: previous}
				}
			}}
			start := time.Now()
			err := verifyPublishedPR(sctx, host, &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"})
			if err == nil {
				t.Fatal("unverified head succeeded")
			}
			if fault == "previous" {
				if !errors.Is(err, scm.ErrHeadChanged) || calls < 2 || time.Since(start) > 5*time.Second {
					t.Fatalf("previous head did not stop at deadline: %v calls=%d elapsed=%s", err, calls, time.Since(start))
				}
			} else if calls != 1 {
				t.Fatalf("retried %s: %d reads", fault, calls)
			}
		})
	}
}

func TestVerifyPublishedPR_SlowProofReadsKeepTheCallersContext(t *testing.T) {
	for _, phase := range []string{"initial", "retry"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			sctx, previous, _ := prLagPublishedContext(t, "new_run")
			ctx, cancel := context.WithTimeout(sctx.Ctx, 10*time.Second)
			defer cancel()
			sctx.Ctx = ctx
			calls := 0
			host := &prLagFixtureHost{verify: func(readCtx context.Context, _ *scm.PR, _, _ string) error {
				calls++
				if phase == "retry" && calls == 1 {
					return &scm.PRHeadChangedError{HeadSHA: previous}
				}
				timer := time.NewTimer(3100 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					return nil
				case <-readCtx.Done():
					return readCtx.Err()
				}
			}}
			if err := verifyPublishedPR(sctx, host, &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}); err != nil {
				t.Fatalf("slow successful %s proof was canceled: %v", phase, err)
			}
		})
	}
}

//go:build e2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Controlled-provider integration, not real-forge evidence. The built CLI talks
// over IPC to a real recovered executor and CIStep, which invokes real glab
// against a loopback GitLab API. Only IPC dispatch/snapshots and the forge are
// test-owned; no daemon, pipeline launch, remote write, or real credentials.
func TestAxiRespond_RecheckControlledGitLab(t *testing.T) {
	if _, err := exec.LookPath("glab"); err != nil {
		t.Skip("controlled GitLab integration requires glab on PATH")
	}
	bin := filepath.Join(t.TempDir(), "no-mistakes")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/no-mistakes")
	build.Dir = repoRootFromThisFile()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	// Do not inherit forge credentials, alternate endpoints, proxies, or git
	// configuration from the caller. All glab state belongs to this test.
	for _, key := range []string{
		"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN",
		"GLAB_ENABLE_CI_AUTOLOGIN", "GITLAB_HOST", "GL_HOST", "GITLAB_URI",
		"GITLAB_API_HOST", "GITLAB_REPO", "GITLAB_GROUP", "REMOTE_ALIAS",
		"GIT_REMOTE_URL_VAR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CHECK_UPDATE", "false")
	t.Setenv("GLAB_SEND_TELEMETRY", "false")
	t.Setenv("GLAB_SHOW_WHATS_NEW", "false")
	t.Setenv("NO_PROMPT", "true")

	for _, unrelated := range []bool{false, true} {
		name := "provider_read_gate"
		if unrelated {
			name = "unrelated_gate"
		}
		t.Run(name, func(t *testing.T) {
			var executor *pipeline.Executor
			completed := make(chan struct{})
			var terminalEvent ipc.Event
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{subscribe: func(ctx context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
				// Forward the executor's actual terminal event to every subscriber.
				// hangSubscribe discards it, leaving a completion racing the first
				// snapshot dependent on a heartbeat beyond the CLI's wait budget.
				return func(send func(interface{}) error) error {
					select {
					case <-ctx.Done():
						return nil
					case <-completed:
						if err := send(terminalEvent); err != nil {
							return err
						}
						<-ctx.Done()
						return nil
					}
				}, nil
			}, respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.RespondParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				if err := executor.RespondWithOverrides(params.Step, params.Action, params.FindingIDs, params.Instructions, params.AddedFindings, params.ApprovalReason); err != nil {
					return nil, err
				}
				return &ipc.RespondResult{OK: true}, nil
			}})
			workDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			cliGit(t, workDir, "remote", "add", "origin", "https://gitlab.test/group/project.git")
			provider := &recheckGitLabAPI{head: fx.head, mode: "green"}
			server := httptest.NewServer(provider)
			t.Cleanup(server.Close)
			cfg := fmt.Sprintf("check_update: false\ntelemetry: false\nhosts:\n  gitlab.test:\n    token: synthetic-loopback-only\n    api_host: %s\n    api_protocol: http\n    git_protocol: https\n", strings.TrimPrefix(server.URL, "http://"))
			if err := os.WriteFile(filepath.Join(os.Getenv("GLAB_CONFIG_DIR"), "config.yml"), []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}

			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repos, err := database.GetRepos()
			if err != nil || len(repos) != 1 {
				t.Fatalf("isolated repository: %v %v", repos, err)
			}
			repo, err := database.ReplaceRepoURLs(repos[0].ID, "https://gitlab.test/group/project.git", "")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature/timeout", fx.head, fx.head)
			if err != nil {
				t.Fatal(err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(database.UpdateRunStatus(run.ID, types.RunRunning))
			must(database.UpdateRunPRURL(run.ID, "https://gitlab.test/group/project/-/merge_requests/123"))
			row, err := database.InsertStepResult(run.ID, types.StepCI)
			must(err)
			must(database.StartStep(row.ID))
			findings := `{"findings":[{"id":"ci-1","severity":"warning","description":"provider read failed","action":"ask-user","category":"ci-provider-read"}]}`
			if unrelated {
				findings = strings.ReplaceAll(findings, "ci-provider-read", "ci-review-bot")
			}
			_, err = database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 1)
			must(err)
			must(database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings))
			run, err = database.GetRun(run.ID)
			must(err)
			parkedSince := *run.AwaitingAgentSince
			stats, err := database.StepRoundStats(row.ID)
			must(err)
			ag := &fakeSuggesterAgent{}
			parked := make(chan struct{}, 1)
			executor = pipeline.NewExecutor(database, p, &config.Config{}, ag, []pipeline.Step{&steps.CIStep{}}, func(ev ipc.Event) {
				if ev.Type == ipc.EventRunCompleted {
					terminalEvent = ev
					close(completed)
				}
				if ev.Status != nil && *ev.Status == string(types.StepStatusAwaitingApproval) {
					select {
					case parked <- struct{}{}:
					default:
					}
				}
			})
			// A real glab traversal spawns several processes; use the production
			// verification timeout rather than the short fake-CLI test budget.
			executor.SetGateReconcileTimings(time.Hour, 30*time.Second)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var resumeErr error
			go func() {
				defer close(done)
				resumeErr = executor.Resume(ctx, run, repo, workDir)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("recovered executor did not stop")
				}
			})
			select {
			case <-parked:
			case <-done:
				t.Fatalf("recover gate: %v", resumeErr)
			case <-time.After(20 * time.Second):
				t.Fatal("recovered gate did not park")
			}
			snapshot := func(context.Context) (*ipc.RunInfo, error) {
				current, err := database.GetRun(run.ID)
				if err != nil {
					return nil, err
				}
				step, err := database.GetStepResult(row.ID)
				if err != nil {
					return nil, err
				}
				override := ""
				if step.OverrideReason != nil {
					override = *step.OverrideReason
				}
				return &ipc.RunInfo{ID: current.ID, Branch: current.Branch, HeadSHA: current.HeadSHA, Status: current.Status, Steps: []ipc.StepResultInfo{{StepName: step.StepName, Status: step.Status, FindingsJSON: step.FindingsJSON, OverrideReason: override}}}, nil
			}
			fx.setGetActive(snapshot)
			fx.setGetRun(func(ctx context.Context, _ int) (*ipc.RunInfo, error) { return snapshot(ctx) })
			assertParked := func(t *testing.T) {
				t.Helper()
				current, err := database.GetRun(run.ID)
				must(err)
				step, err := database.GetStepResult(row.ID)
				must(err)
				if current.Status != types.RunRunning || current.AwaitingAgentSince == nil || *current.AwaitingAgentSince != parkedSince || step.Status != types.StepStatusAwaitingApproval || step.FindingsJSON == nil || *step.FindingsJSON != findings || step.OverrideReason != nil {
					t.Fatalf("recheck changed unresolved decision: %+v %+v", current, step)
				}
			}
			respond := func(t *testing.T, extra ...string) (string, error) {
				t.Helper()
				args := append([]string{"axi", "respond", "--action", "recheck", "--wait", "45s"}, extra...)
				cmd := exec.CommandContext(t.Context(), bin, args...)
				cmd.Dir = workDir
				out, err := cmd.CombinedOutput()
				t.Logf("controlled-provider CLI %v: %v\n%s", extra, err, out)
				return string(out), err
			}
			if unrelated {
				out, err := respond(t)
				if err == nil || !strings.Contains(out, "provider-read CI gate") {
					t.Fatalf("unrelated gate accepted: %v %s", err, out)
				}
				assertParked(t)
			} else {
				for _, tc := range []struct{ mode, reason string }{
					{"unavailable", "read GitLab pipeline 77"},
					{"empty", "pipeline 77 has no jobs or bridges"},
					{"pending", "checks are absent or not all passed"},
					{"failed", "checks are absent or not all passed"},
					{"changed head", "not open at expected head"},
					{"grandchild pending", "checks are absent or not all passed"},
					{"grandchild failed", "checks are absent or not all passed"},
					{"grandchild unreadable", "unreadable GitLab pipeline"},
					{"grandchild absent", "no verified successful downstream"},
					{"grandchild empty", "pipeline 79 has no jobs or bridges"},
				} {
					t.Run(tc.mode, func(t *testing.T) {
						provider.setMode(tc.mode)
						out, err := respond(t)
						if err == nil || !strings.Contains(out, tc.reason) || strings.Contains(out, "signal: killed") {
							t.Fatalf("unverified evidence accepted: %v %s", err, out)
						}
						assertParked(t)
					})
				}
				provider.setMode("green")
				for _, extra := range [][]string{{"--yes"}, {"--findings", "ci-1"}, {"--instructions", "repair"}, {"--add-finding", `{"description":"repair"}`}, {"--reason", "waive"}, {"--step", "review"}} {
					out, err := respond(t, extra...)
					if err == nil || !strings.Contains(out, "recheck is CI-only") {
						t.Fatalf("repair/waiver input accepted: %v %s", err, out)
					}
					assertParked(t)
				}
				dirty := filepath.Join(workDir, "unvalidated.txt")
				must(os.WriteFile(dirty, []byte("unvalidated"), 0600))
				out, err := respond(t)
				if err == nil || !strings.Contains(out, "uncommitted changes") {
					t.Fatalf("dirty caller accepted: %v %s", err, out)
				}
				assertParked(t)
				must(os.Remove(dirty))
				cliGit(t, workDir, "commit", "--allow-empty", "-m", "unvalidated head")
				out, err = respond(t)
				if err == nil || !strings.Contains(out, "head differs") {
					t.Fatalf("changed caller head accepted: %v %s", err, out)
				}
				assertParked(t)
				cliGit(t, workDir, "reset", "--hard", fx.head)
				out, err = respond(t)
				if err != nil || !strings.Contains(out, "outcome: passed") || strings.Contains(out, "passed-with-override") {
					t.Fatalf("authoritative green did not continue: %v %s", err, out)
				}
				select {
				case <-done:
					must(resumeErr)
				case <-time.After(5 * time.Second):
					t.Fatal("same-run completion did not finish")
				}
				current, err := database.GetRun(run.ID)
				must(err)
				step, err := database.GetStepResult(row.ID)
				must(err)
				if current.Status != types.RunCompleted || current.AwaitingAgentSince != nil || step.Status != types.StepStatusCompleted || step.OverrideReason != nil || step.ApprovalReason != nil {
					t.Fatalf("not clean same-run completion: %+v %+v", current, step)
				}
				if step.FindingsJSON == nil {
					t.Fatal("missing verification receipt")
				}
				receipt, err := types.ParseFindingsJSON(*step.FindingsJSON)
				must(err)
				if len(receipt.Items) != 0 || !strings.Contains(receipt.Summary, fx.head) {
					t.Fatalf("wrong verification receipt: %+v", receipt)
				}
				t.Logf("persisted completion: run=%s status=%s ci=%s awaiting_agent=%v override=%v approval_reason=%v receipt=%s", current.ID, current.Status, step.Status, current.AwaitingAgentSince, step.OverrideReason, step.ApprovalReason, *step.FindingsJSON)
			}
			currentStats, err := database.StepRoundStats(row.ID)
			must(err)
			runs, err := database.GetRunsByRepo(repo.ID)
			must(err)
			if currentStats != stats || len(runs) != 1 || ag.callCount() != 0 || cliGit(t, workDir, "rev-parse", "HEAD") != fx.head || cliGit(t, workDir, "status", "--porcelain") != "" {
				t.Fatal("recheck started repair, changed rounds/head/worktree, or replaced the run")
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if len(provider.unexpected) != 0 {
				t.Fatalf("unexpected provider operations: %v", provider.unexpected)
			}
			if !unrelated && provider.grandchildReads == 0 {
				t.Fatal("linked grandchild was never read")
			}
			t.Logf("isolated run observations: runs=%d repair_calls=%d rounds_unchanged=%t head_unchanged=true worktree_clean=true unexpected_provider_operations=%v grandchild_reads=%d", len(runs), ag.callCount(), currentStats == stats, provider.unexpected, provider.grandchildReads)
		})
	}
}

type recheckGitLabAPI struct {
	mu              sync.Mutex
	head            string
	mode            string
	grandchildReads int
	unexpected      []string
}

func (api *recheckGitLabAPI) setMode(mode string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.mode = mode
}

func (api *recheckGitLabAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		api.unexpected = append(api.unexpected, r.Method+" "+r.URL.RequestURI())
		http.Error(w, "writes forbidden", http.StatusForbidden)
		return
	}
	pipeline := func(id int, project, head, status string) string {
		return fmt.Sprintf(`{"id":%d,"sha":%q,"status":%q,"web_url":"https://gitlab.test/group/%s/-/pipelines/%d"}`, id, head, status, project, id)
	}
	bridge := func(child string) string {
		return `[{"id":10,"name":"trigger","status":"success","downstream_pipeline":` + child + `}]`
	}
	child := pipeline(78, "child", strings.Repeat("2", 40), "success")
	grandchild := pipeline(79, "grandchild", strings.Repeat("3", 40), "success")
	path := strings.TrimPrefix(r.URL.Path, "/api/v4/")
	var out string
	switch path {
	case "user":
		out = `{"id":1,"username":"test"}`
	case "projects/group/project":
		out = `{"id":1,"path_with_namespace":"group/project","default_branch":"main"}`
	case "projects/group/project/merge_requests/123/approval_state":
		out = `{"rules":[]}`
	case "projects/group/project/merge_requests/123":
		head := api.head
		if api.mode == "changed head" {
			head = strings.Repeat("4", 40)
		}
		out = fmt.Sprintf(`{"id":123,"iid":123,"project_id":1,"state":"opened","sha":%q,"merge_status":"can_be_merged","detailed_merge_status":"mergeable","head_pipeline":{"id":77,"sha":%q}}`, head, head)
	case "projects/group/project/pipelines/77":
		if api.mode == "unavailable" {
			http.Error(w, "synthetic provider unavailable", http.StatusServiceUnavailable)
			return
		}
		status := "success"
		if api.mode == "pending" || api.mode == "failed" {
			status = api.mode
		}
		out = pipeline(77, "project", api.head, status)
	case "projects/group/project/pipelines":
		// Obsolete failed/canceled pipelines must never veto the authoritative
		// green MR pipeline. A history-based implementation sees both here.
		out = "[" + pipeline(75, "project", api.head, "failed") + "," + pipeline(76, "project", api.head, "canceled") + "," + pipeline(77, "project", api.head, "success") + "]"
	case "projects/group/project/pipelines/77/jobs":
		out = `[]`
	case "projects/group/project/pipelines/77/bridges":
		out = bridge(child)
		if api.mode == "empty" {
			out = `[]`
		}
	case "projects/group/child/pipelines/78":
		out = child
	case "projects/group/child/pipelines/78/jobs":
		out = `[]`
	case "projects/group/child/pipelines/78/bridges":
		out = bridge(grandchild)
		if api.mode == "grandchild absent" {
			out = bridge("null")
		}
	case "projects/group/grandchild/pipelines/79":
		api.grandchildReads++
		out = grandchild
		if api.mode == "grandchild unreadable" {
			out = `not-json`
		} else if api.mode == "grandchild pending" || api.mode == "grandchild failed" {
			out = pipeline(79, "grandchild", strings.Repeat("3", 40), strings.TrimPrefix(api.mode, "grandchild "))
		}
	case "projects/group/grandchild/pipelines/79/jobs":
		out = `[{"id":11,"name":"test","status":"success"}]`
		if api.mode == "grandchild empty" {
			out = `[]`
		}
	case "projects/group/grandchild/pipelines/79/bridges":
		out = `[]`
	default:
		api.unexpected = append(api.unexpected, r.Method+" "+r.URL.RequestURI())
		http.Error(w, "unexpected provider read", http.StatusNotFound)
		return
	}
	fmt.Fprint(w, out)
}

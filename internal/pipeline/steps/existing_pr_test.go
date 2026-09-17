package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const fixtureExistingPR = "https://github.com/upstream/widgets/pull/168"
const fixtureSourceURL = "https://github.com/contributor/widgets.git"

func existingPRFixture(head string) string {
	return fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","merged":false,"base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, fixtureExistingPR, head)
}

// stubFixtureIntegrationBase stands the pull request's repository in with the
// worktree's own main branch and fetches the run's integration ref, which the
// daemon establishes at launch and the rebase step refreshes before any gate
// measures a diff against it.
func stubFixtureIntegrationBase(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	gitCmd(t, sctx.WorkDir, "config", "url."+sctx.WorkDir+".insteadOf", "https://github.com/upstream/widgets.git")
	if err := FetchRunUpstreamBranch(context.Background(), sctx, "main"); err != nil {
		t.Fatal(err)
	}
}

// The PR step always runs after the gates, so its evidence appendix has step
// results to report.
func recordCompletedReviewStep(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
}

func pinFixturePR(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	sctx.Repo.UpstreamURL = fixtureSourceURL
	sctx.Repo.URLsVerified = true
	run, err := sctx.DB.InsertRunWithIntentAndLaunchNonce(sctx.Repo.ID, sctx.Run.Branch, sctx.Run.HeadSHA, sctx.Run.BaseSHA, nil, "", "", "", "", fixtureExistingPR)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
}

// Representative reproduction of the symptom without touching a live forge.
// With no explicit target, the upstream PR is outside the fork-scoped query;
// ordinary behavior still creates on the registered fork repository.
func TestPRStep_ForkOriginWithoutExplicitTargetRetainsCreation(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = fixtureSourceURL
	created := "https://github.com/contributor/widgets/pull/2"
	env, log := fakeGH(t, "")
	sctx.Env = append(env, "FAKE_CLI_CREATED_PR_URL="+created)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out.PRURL != created {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "pr list --head feature --repo contributor/widgets") || !strings.Contains(string(data), "pr create --head feature --base main --repo contributor/widgets") {
		t.Fatalf("unexpected discovery boundary:\n%s", data)
	}
}

func TestPRStep_ExplicitUpstreamNeverDiscoversAlternate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, replace, with string
		liveHead            string
		unavailable         bool
	}{
		{name: "valid fork origin"},
		{name: "wrong source ref", replace: `"ref":"feature"`, with: `"ref":"other"`},
		{name: "wrong source repository", replace: `"full_name":"contributor/widgets"`, with: `"full_name":"contributor/other"`},
		{name: "wrong target repository", replace: `"full_name":"upstream/widgets"`, with: `"full_name":"elsewhere/widgets"`},
		{name: "different head", liveHead: strings.Repeat("f", 40)},
		{name: "missing head", replace: `"sha":`, with: `"ignored_sha":`},
		{name: "closed target", replace: `"state":"open"`, with: `"state":"closed"`},
		{name: "unavailable validation", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			pinFixturePR(t, sctx)
			recordCompletedReviewStep(t, sctx)
			stubFixtureIntegrationBase(t, sctx)
			env, log := fakeGH(t, "https://github.com/contributor/widgets/pull/2")
			payload := existingPRFixture(head)
			if tc.liveHead != "" {
				payload = existingPRFixture(tc.liveHead)
			}
			if tc.replace != "" {
				payload = strings.Replace(payload, tc.replace, tc.with, 1)
			}
			bodyFile := filepath.Join(t.TempDir(), "body.md")
			if err := os.WriteFile(bodyFile, []byte("## Overview\n\nUpstream author narrative.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			sctx.Env = append(env, "FAKE_CLI_EXISTING_PR_JSON="+payload, "FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168", "FAKE_CLI_PR_BODY_FILE="+bodyFile)
			if tc.unavailable {
				sctx.Env = append(sctx.Env, "FAKE_CLI_EXISTING_PR_ERROR=unavailable")
			}
			out, err := (&PRStep{}).Execute(sctx)
			valid := tc.replace == "" && tc.liveHead == "" && !tc.unavailable
			if valid && (err != nil || out.PRURL != fixtureExistingPR) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if !valid && err == nil {
				t.Fatalf("wanted failure, got %+v", out)
			}
			if tc.liveHead != "" {
				// A source branch that moved outside the run names both heads
				// and the supported recovery, not just a bare SHA.
				for _, want := range []string{shortSHA(tc.liveHead), shortSHA(head), "--retire-existing-pr"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("out-of-band head refusal omitted %q: %v", want, err)
					}
				}
			}
			data, _ := os.ReadFile(log)
			calls := string(data)
			if strings.Contains(calls, "pr list") || strings.Contains(calls, "pr create") {
				t.Fatalf("explicit target fell through to discovery/create:\n%s", calls)
			}
			if !valid && strings.Contains(calls, "pr edit") {
				t.Fatalf("invalid target mutated PR:\n%s", calls)
			}
			if valid && (!strings.Contains(calls, "pr edit 168 --repo upstream/widgets") || strings.Contains(calls, "--repo contributor/widgets")) {
				t.Fatalf("wrong update routing:\n%s", calls)
			}
			stored, e := sctx.DB.GetRun(sctx.Run.ID)
			if e != nil || stored.ExistingPRURL == nil || *stored.ExistingPRURL != fixtureExistingPR {
				t.Fatalf("lost durable target: %+v %v", stored, e)
			}
		})
	}
}

// An explicit target is somebody else's review object: the run may append its
// own evidence, never replace the author's narrative or title - including the
// first association, where no template and no prior appendix exist.
func TestPRStep_ExplicitTargetKeepsAuthorTitleAndBody(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	sctx.Config.PR.TitleFormat = "PROJ-123: %s"
	recordCompletedReviewStep(t, sctx)
	stubFixtureIntegrationBase(t, sctx)
	author := "## Overview\n\nHand-written upstream narrative.\n\nCloses https://github.com/upstream/widgets/issues/7\n"
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte(author), 0o644); err != nil {
		t.Fatal(err)
	}
	env, logFile := fakeGH(t, "")
	sctx.Env = append(env,
		"FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(head),
		"FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=Author title",
	)
	out, execErr := (&PRStep{}).Execute(sctx)
	if execErr != nil || out.PRURL != fixtureExistingPR {
		t.Fatalf("out=%+v err=%v", out, execErr)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || !parts.managed {
		t.Fatalf("pipeline evidence is not separately owned: %+v, %v", parts, err)
	}
	if strings.TrimSpace(parts.before) != strings.TrimSpace(author) {
		t.Fatalf("author narrative was rewritten:\n%s", body)
	}
	logs, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), "--title") {
		t.Fatalf("author title was rewritten:\n%s", logs)
	}
}

// GitHub answers a pull request from a replica, so the head it reports can
// still trail a commit the push step just proved on the source remote. That is
// the forge catching up, not the source branch moving outside the run, and the
// PR step re-reads a bounded number of times before it refuses. Equality stays
// exact: the run publishes only once the pull request answers the same commit.
func TestPRStep_ExplicitTargetReReadsAHeadTheForgeHasNotCaughtUpWith(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	recordCompletedReviewStep(t, sctx)
	stubFixtureIntegrationBase(t, sctx)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("## Overview\n\nUpstream author narrative.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, log := fakeGH(t, "")
	sctx.Env = append(env,
		"FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(head),
		"FAKE_CLI_EXISTING_PR_JSON_FIRST="+existingPRFixture(base),
		"FAKE_CLI_EXISTING_PR_FIRST_MARKER="+filepath.Join(t.TempDir(), "served"),
		"FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
	)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out.PRURL != fixtureExistingPR {
		t.Fatalf("a settling pull request head failed the run: out=%+v err=%v", out, err)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "pr list") || strings.Contains(string(calls), "pr create") {
		t.Fatalf("explicit target fell through to discovery/create:\n%s", calls)
	}
	if !strings.Contains(string(calls), "pr edit 168 --repo upstream/widgets") {
		t.Fatalf("settled target was not updated:\n%s", calls)
	}
}

// An empty description is still the author's: a repository template fills a
// blank body on the repository's own pull requests, never on a pull request the
// run is only associated with.
func TestPRStep_ExplicitTargetWithEmptyBodyIsNotTemplated(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	drafted := false
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		drafted = true
		data, _ := json.Marshal(prContent{Title: "drafted title", Body: filledPRTemplate})
		return &agent.Result{Output: data}, nil
	}
	pinFixturePR(t, sctx)
	recordCompletedReviewStep(t, sctx)
	stubFixtureIntegrationBase(t, sctx)
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env, logFile := fakeGH(t, "")
	sctx.Env = append(env,
		"FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(sctx.Run.HeadSHA),
		"FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=Author title",
	)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out.PRURL != fixtureExistingPR {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if drafted {
		t.Fatal("drafted a narrative for a pull request the run does not own")
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || !parts.managed {
		t.Fatalf("pipeline evidence is not separately owned: %+v, %v", parts, err)
	}
	if strings.TrimSpace(parts.before) != "" {
		t.Fatalf("empty author description was filled in:\n%s", body)
	}
	logs, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), "--title") {
		t.Fatalf("author title was rewritten:\n%s", logs)
	}
}

// Both halves of a gate prompt must agree about what the change integrates
// with: an associated run measures its base from the pull request's branch in
// the pull request's repository, so the prompt names that, not the fork's
// default branch. A run with no association keeps naming the repository
// default exactly as before.
func TestReviewStep_PromptNamesTheBranchItsBaseWasMeasuredFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		associated bool
		want       string
	}{
		{name: "associated", associated: true, want: "- default branch: upstream/widgets:main"},
		{name: "unassociated", want: "- default branch: main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			prompts := make(chan string, 4)
			ag := &mockAgent{name: "prompt-probe", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				prompts <- opts.Prompt
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"none","risk_scope":"source-or-external"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			if tc.associated {
				pinFixturePR(t, sctx)
				stubFixtureIntegrationBase(t, sctx)
			}
			if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			select {
			case prompt := <-prompts:
				if !strings.Contains(prompt, tc.want) {
					t.Fatalf("prompt did not name %q:\n%s", tc.want, prompt)
				}
			default:
				t.Fatal("review never reached the agent")
			}
		})
	}
}

func TestCIStep_ExplicitPRUsesPublishedHeadAndUpstreamHost(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	sctx.Env = append(fakeCIGH(t, "MERGED", `[]`), "FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(head), "FAKE_CLI_LOG="+log)
	out, err := (&CIStep{}).Execute(sctx)
	if err != nil || out.Skipped {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "pr view 168 --repo upstream/widgets") || strings.Contains(string(data), "--repo contributor/widgets") {
		t.Fatalf("wrong CI routing:\n%s", data)
	}
	sctx.Env = append(sctx.Env, "FAKE_CLI_EXISTING_PR_ERROR=unavailable")
	if out, err := (&CIStep{}).Execute(sctx); err == nil {
		t.Fatalf("unavailable validation skipped or passed: %+v", out)
	}
}

func TestPushStep_ExplicitPRValidatesRemoteHeadBeforePublishing(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%v", invalid), func(t *testing.T) {
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			dir, base, prior := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", remote)
			gitCmd(t, dir, "push", "origin", "main", "feature")
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", ".")
			gitCmd(t, dir, "commit", "-m", "pipeline fix")
			next := gitCmd(t, dir, "rev-parse", "HEAD")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, next, config.Commands{})
			pinFixturePR(t, sctx)
			// Real local Git transport; the logical GitHub URL is never contacted.
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", fixtureSourceURL)
			setupGateMirror(t, sctx)
			recordReviewApproval(t, sctx, next)
			log := filepath.Join(t.TempDir(), "gh.log")
			expected := prior
			if invalid {
				expected = next
			}
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(expected), "FAKE_CLI_PR_BODY="+compliantPipelineBody(t, prior), "FAKE_CLI_LOG="+log)
			_, err := (&PushStep{}).Execute(sctx)
			if (err != nil) != invalid {
				t.Fatalf("push err=%v", err)
			}
			want := next
			if invalid {
				want = prior
			}
			if got := gitCmd(t, remote, "rev-parse", "refs/heads/feature"); got != want {
				t.Fatalf("remote moved incorrectly: %s want %s", got, want)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "pr list") || strings.Contains(string(data), "pr create") || invalid && strings.Contains(string(data), "pr edit") {
				t.Fatalf("unsafe PR calls:\n%s", data)
			}
			if !invalid && !strings.Contains(string(data), "pr edit 168 --repo upstream/widgets") {
				t.Fatalf("missing upstream attestation:\n%s", data)
			}
		})
	}
}

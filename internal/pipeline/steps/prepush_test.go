package steps

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// prePushProbe is a scripted pre_push_check. It records the decision context it
// was handed plus the remote head as it stood WHILE the check ran, then exits
// with the configured code.
type prePushProbe struct {
	command    string
	recordPath string
	remotePath string
}

// ran reports whether the check executed at all.
func (p prePushProbe) ran() bool {
	_, err := os.Stat(p.recordPath)
	return err == nil
}

func (p prePushProbe) mustRun(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(p.recordPath)
	if err != nil {
		t.Fatalf("pre_push_check did not run: %v", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		fields[key] = strings.TrimSpace(value)
	}
	return fields
}

// remoteHeadDuringCheck is the SHA the remote branch pointed at when the check
// ran. It is what proves the hook is a PRE-push hook: on the passing path the
// push succeeds afterwards, so a value equal to the pre-push remote head is the
// only evidence that ordering was respected.
func (p prePushProbe) remoteHeadDuringCheck(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(p.remotePath)
	if err != nil {
		t.Fatalf("pre_push_check did not observe the remote: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// newPrePushProbe writes a platform-appropriate check script and returns the
// command string a repository would put in pre_push_check.
func newPrePushProbe(t *testing.T, upstream, ref string, exitCode int) prePushProbe {
	t.Helper()
	dir := t.TempDir()
	probe := prePushProbe{
		recordPath: filepath.Join(dir, "context"),
		remotePath: filepath.Join(dir, "remote"),
	}

	if runtime.GOOS == "windows" {
		script := filepath.Join(dir, "pre-push-check.cmd")
		body := "@echo off\r\n" +
			"(\r\n" +
			"echo pr_url=%NO_MISTAKES_PR_URL%\r\n" +
			"echo pr_number=%NO_MISTAKES_PR_NUMBER%\r\n" +
			"echo ref=%NO_MISTAKES_REF%\r\n" +
			"echo branch=%NO_MISTAKES_BRANCH%\r\n" +
			"echo base_branch=%NO_MISTAKES_BASE_BRANCH%\r\n" +
			"echo head_sha=%NO_MISTAKES_HEAD_SHA%\r\n" +
			"echo remote_sha=%NO_MISTAKES_REMOTE_SHA%\r\n" +
			") > \"" + probe.recordPath + "\"\r\n" +
			"git ls-remote \"" + upstream + "\" " + ref + " > \"" + probe.remotePath + "\"\r\n" +
			"echo the pull request is held by an external merge process\r\n" +
			"exit /b " + strconv.Itoa(exitCode) + "\r\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		probe.command = `"` + script + `"`
		return probe
	}

	script := filepath.Join(dir, "pre-push-check.sh")
	body := "#!/bin/sh\n" +
		"{\n" +
		"printf 'pr_url=%s\\n' \"$NO_MISTAKES_PR_URL\"\n" +
		"printf 'pr_number=%s\\n' \"$NO_MISTAKES_PR_NUMBER\"\n" +
		"printf 'ref=%s\\n' \"$NO_MISTAKES_REF\"\n" +
		"printf 'branch=%s\\n' \"$NO_MISTAKES_BRANCH\"\n" +
		"printf 'base_branch=%s\\n' \"$NO_MISTAKES_BASE_BRANCH\"\n" +
		"printf 'head_sha=%s\\n' \"$NO_MISTAKES_HEAD_SHA\"\n" +
		"printf 'remote_sha=%s\\n' \"$NO_MISTAKES_REMOTE_SHA\"\n" +
		"} > '" + probe.recordPath + "'\n" +
		"git ls-remote '" + upstream + "' " + ref + " > '" + probe.remotePath + "'\n" +
		"printf 'the pull request is held by an external merge process\\n'\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	probe.command = "'" + script + "'"
	return probe
}

// newPrePushFixture builds a worktree whose feature branch is already published
// on a bare upstream and then advances the local head, so the push step is in
// the exact position this hook guards: about to move an existing remote branch.
func newPrePushFixture(t *testing.T) (sctx *pipeline.StepContext, upstream, publishedHead, newHead string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	publishedHead = submittedHead

	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes: apply agent fixes")
	newHead = gitCmd(t, dir, "rev-parse", "HEAD")

	sctx = newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, newHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	recordReviewApproval(t, sctx, newHead)
	return sctx, upstream, publishedHead, newHead
}

// TestPushStep_PrePushCheckBlocksUpdateToAlreadyOpenPullRequest is the
// regression test for the gap this hook closes. A gate round produced a fix
// commit for a branch whose open pull request an external merge process already
// owns; pushing it changes the PR head and invalidates whatever that process
// has in flight. With pre_push_check configured, the veto must land BEFORE any
// object moves and say plainly why.
func TestPushStep_PrePushCheckBlocksUpdateToAlreadyOpenPullRequest(t *testing.T) {
	t.Parallel()
	sctx, upstream, publishedHead, newHead := newPrePushFixture(t)
	probe := newPrePushProbe(t, upstream, "refs/heads/feature", 3)
	sctx.Config.PrePushCheck = probe.command
	prURL := "https://github.com/test/repo/pull/7"
	sctx.Run.PRURL = &prURL

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected the configured pre_push_check to refuse the push")
	}
	for _, want := range []string{"pre_push_check", "pull request #7", "exited with code 3", "external merge process"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must explain itself; missing %q in:\n%s", want, err)
		}
	}
	if !strings.Contains(err.Error(), "held by an external merge process") {
		t.Fatalf("refusal must quote the check's own output:\n%s", err)
	}

	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != publishedHead {
		t.Fatalf("remote moved from %s to %s despite the refusal", publishedHead, remote)
	}
	if fileAtRef(t, upstream, "refs/heads/feature", "fix.txt") {
		t.Fatal("the blocked commit reached the remote branch")
	}

	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.LastPushedSHA != nil && *dbRun.LastPushedSHA == newHead {
		t.Fatal("a refused push must not be recorded as delivered")
	}
	if dbRun.PushActive {
		t.Fatal("push-active marker remained set after the refusal")
	}
}

// TestPushStep_PrePushCheckPassesAndRunsBeforeThePush pins the other half:
// exit 0 pushes exactly as before, and the check saw the pre-push remote head,
// which is what makes it a pre-push hook rather than a post-mortem.
func TestPushStep_PrePushCheckPassesAndRunsBeforeThePush(t *testing.T) {
	t.Parallel()
	sctx, upstream, publishedHead, newHead := newPrePushFixture(t)
	probe := newPrePushProbe(t, upstream, "refs/heads/feature", 0)
	sctx.Config.PrePushCheck = probe.command
	prURL := "https://github.com/test/repo/pull/7"
	sctx.Run.PRURL = &prURL

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("a passing pre_push_check must not change push behavior: %v", err)
	}

	if observed := probe.remoteHeadDuringCheck(t); observed != publishedHead {
		t.Fatalf("check observed remote head %s, want the pre-push head %s", observed, publishedHead)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != newHead {
		t.Fatalf("remote head = %s, want the pushed head %s", remote, newHead)
	}

	fields := probe.mustRun(t)
	want := map[string]string{
		"pr_url":      prURL,
		"pr_number":   "7",
		"ref":         "refs/heads/feature",
		"branch":      "feature",
		"base_branch": "main",
		"head_sha":    newHead,
		"remote_sha":  publishedHead,
	}
	for key, expected := range want {
		if fields[key] != expected {
			t.Errorf("pre_push_check %s = %q, want %q", key, fields[key], expected)
		}
	}
}

// TestPushStep_PrePushCheckSkipsFirstPublicationOfABranch pins the scope: the
// hook guards pushes that land under something already open. Publishing a
// branch for the first time - the push that a brand-new pull request is created
// from - has nothing to land under and must not be gated.
func TestPushStep_PrePushCheckSkipsFirstPublicationOfABranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	recordReviewApproval(t, sctx, headSHA)

	probe := newPrePushProbe(t, upstream, "refs/heads/feature", 1)
	sctx.Config.PrePushCheck = probe.command

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("creating a new remote branch must not be gated: %v", err)
	}
	if probe.ran() {
		t.Fatal("pre_push_check ran while publishing a branch for the first time")
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != headSHA {
		t.Fatalf("remote head = %s, want %s", remote, headSHA)
	}
}

// TestPushStep_PrePushCheckUnsetLeavesExistingBehaviorIntact is the
// opt-in guarantee: a repository that never heard of this field pushes exactly
// as it did before, with no subprocess and no forge lookup.
func TestPushStep_PrePushCheckUnsetLeavesExistingBehaviorIntact(t *testing.T) {
	t.Parallel()
	sctx, upstream, _, newHead := newPrePushFixture(t)
	if sctx.Config.PrePushCheck != "" {
		t.Fatal("pre_push_check must default to unset")
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != newHead {
		t.Fatalf("remote head = %s, want %s", remote, newHead)
	}
}

// TestRunConfiguredPrePushCheck_SkipMatrix pins the three cases that must never
// launch the command, straight against the decision the push step hands it.
func TestRunConfiguredPrePushCheck_SkipMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure bool
		decision  forcePushDecision
	}{
		{name: "unset", configure: false, decision: forcePushDecision{remoteSHA: strings.Repeat("a", 40)}},
		{name: "new branch", configure: true, decision: forcePushDecision{newBranch: true}},
		{name: "remote already at head", configure: true, decision: forcePushDecision{remoteSHA: strings.Repeat("a", 40), upToDate: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			probe := newPrePushProbe(t, dir, "refs/heads/feature", 1)
			if tt.configure {
				sctx.Config.PrePushCheck = probe.command
			}

			if err := runConfiguredPrePushCheck(sctx, tt.decision, prePushTarget{
				Ref:     "refs/heads/feature",
				Branch:  "feature",
				HeadSHA: headSHA,
			}); err != nil {
				t.Fatalf("skipped case must not fail: %v", err)
			}
			if probe.ran() {
				t.Fatal("pre_push_check ran for a push it does not guard")
			}
		})
	}
}

// writeRelativePrePushScript writes a trivial pre_push_check script at relPath
// under root and returns the command a repository could configure to invoke
// it BY THAT RELATIVE PATH, i.e. relying on the process working directory to
// resolve it - exactly what a contributor's own pushed branch can supply at a
// path the trusted config names (the documented example is
// "scripts/merge-queue-hold.sh").
func writeRelativePrePushScript(t *testing.T, root, relPath string, exitCode int) string {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		full += ".cmd"
		relPath += ".cmd"
		body := "@echo off\r\nexit /b " + strconv.Itoa(exitCode) + "\r\n"
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return relPath
	}
	body := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return "sh " + relPath
}

// TestRunConfiguredPrePushCheck_RunsOutsideWorktree is the regression test for
// the trust-boundary bypass a repository-relative pre_push_check script would
// otherwise open: pre_push_check is trusted-only precisely because it is a
// security veto, but running it inside the pushed worktree would let a
// contributor shadow a repo-relative script - such as the documented
// "scripts/merge-queue-hold.sh" example - with their own branch content and
// make the trusted command always pass using the daemon's own credentials.
// The script here lives ONLY in the pushed worktree (sctx.WorkDir), so the
// check must fail to resolve it rather than silently executing it.
func TestRunConfiguredPrePushCheck_RunsOutsideWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	// A contributor-controlled script at a repository-relative path that
	// always exits 0 (i.e. it would silently defeat the guard if it ran).
	sctx.Config.PrePushCheck = writeRelativePrePushScript(t, dir, "scripts/merge-queue-hold.sh", 0)

	err := runConfiguredPrePushCheck(sctx, forcePushDecision{remoteSHA: strings.Repeat("a", 40)}, prePushTarget{
		Ref:     "refs/heads/feature",
		Branch:  "feature",
		HeadSHA: headSHA,
	})
	if err == nil {
		t.Fatal("a repository-relative pre_push_check script resolved against the pushed worktree; a contributor's own branch could shadow the trusted check")
	}
}

// TestRunConfiguredPrePushCheck_UsesLivePRBaseBranch pins the same precedence
// the CI step already applies (AGENTS.md, pr.base_branch): once a pull
// request exists, its actual forge base branch is authoritative over a
// since-changed pr.base_branch, because the check exists to describe the real
// target of the pull request the guard protects, not a hypothetical one a
// later config edit selected.
func TestRunConfiguredPrePushCheck_UsesLivePRBaseBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Config.PR.BaseBranch = "main"
	env, _ := fakeGHWithBase(t, prURL, "develop")
	sctx.Env = env

	probe := newPrePushProbe(t, dir, "refs/heads/feature", 0)
	sctx.Config.PrePushCheck = probe.command

	if err := runConfiguredPrePushCheck(sctx, forcePushDecision{remoteSHA: strings.Repeat("a", 40)}, prePushTarget{
		Ref:        "refs/heads/feature",
		Branch:     "feature",
		BaseBranch: prePushBaseBranch(sctx),
		HeadSHA:    headSHA,
	}); err != nil {
		t.Fatal(err)
	}

	fields := probe.mustRun(t)
	if fields["base_branch"] != "develop" {
		t.Fatalf("pre_push_check base_branch = %q, want the pull request's live base %q, not the stale configured %q", fields["base_branch"], "develop", "main")
	}
}

func TestPRNumberFromURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://github.com/owner/name/pull/1234":            "1234",
		"https://gitlab.com/group/name/-/merge_requests/42/": "42",
		"https://example.com/owner/name/pulls/9":             "9",
		"https://github.com/owner/name/pull/":                "",
		"https://github.com/owner/name":                      "",
		"":                                                   "",
	}
	for input, want := range tests {
		if got := prNumberFromURL(input); got != want {
			t.Errorf("prNumberFromURL(%q) = %q, want %q", input, got, want)
		}
	}
}

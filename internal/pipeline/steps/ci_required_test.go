package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func splitCertificationConfig() config.Certification {
	return config.Certification{
		Mode:           config.CertificationModeCIAuthoritative,
		LocalFast:      config.LocalFastCommands{Lint: "true", Typecheck: "true ", Test: "true  "},
		RequiredChecks: []string{"full-suite"},
	}
}

// A PR head that is not this run's verified published revision is terminal on
// the first observation: the monitor must not spend another poll interval (or
// the whole idle timeout) on a revision it can never certify.
func TestCIStep_CIAuthoritativeRevisionMismatchIsTerminalOnFirstObservation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const driftedHead = "cccccccccccccccccccccccccccccccccccccccc"
	env := fakeCIGH(t, "OPEN", `[{"name":"full-suite","state":"SUCCESS","bucket":"pass"}]`)
	env = append(env, "FAKE_CLI_HEAD_SHA="+driftedHead)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	logs := []string{}
	sctx.Log = func(s string) { logs = append(logs, s) }
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.Certification = splitCertificationConfig()
	sctx.Config.CITimeout = time.Minute
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: headSHA, TargetKind: "upstream", TargetFingerprint: "github:test/repo", Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}

	polls := 0
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		polls++
		return context.Canceled
	}}
	_, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "published revision") {
		t.Fatalf("Execute error = %v, want a terminal published-revision refusal", err)
	}
	if polls != 0 {
		t.Fatalf("monitor waited %d more poll(s) instead of failing immediately on the revision mismatch", polls)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt != nil {
		t.Fatal("green checks on a drifted PR head marked the run CI ready")
	}
}

// A fix push that reaches the remote but cannot record its published revision
// is the one way the pipeline could manufacture the mismatch above. It stops
// the run where the cause is visible instead of surfacing one poll later as an
// unexplained PR-head divergence.
func TestCIStep_PushUpdatedHeadSHA_UnrecordedPublishedRevisionStopsSplitCertification(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("ci fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "ci fix")
	fixedSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = headSHA
	sctx.Config.Certification = splitCertificationConfig()

	// Controlled double for "the binding write could not complete": the push
	// itself still succeeds, so the remote advances past the recorded revision.
	sctx.DB.Close()

	step := &CIStep{}
	_, err := step.pushUpdatedHeadSHA(sctx, fixedSHA)
	if !errors.Is(err, errPublishedRevisionUnbound) {
		t.Fatalf("pushUpdatedHeadSHA error = %v, want an unbound published revision error", err)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != fixedSHA {
		t.Fatalf("remote head = %s, want the pushed fix %s (precondition for the divergence)", remote, fixedSHA)
	}
	if terminal := unboundPublishedRevisionError(sctx, err); terminal == nil {
		t.Fatal("split certification kept polling after the published revision could not be recorded")
	}
	sctx.Config.Certification = config.Certification{Mode: config.CertificationModeLocalHeavy}
	if terminal := unboundPublishedRevisionError(sctx, err); terminal != nil {
		t.Fatalf("legacy certification became terminal on a push-binding failure: %v", terminal)
	}
}

func TestEvaluateRequiredChecksFailsClosed(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name     string
		observed string
		checks   []scm.Check
		wantWait bool
		wantFail bool
		wantErr  string
	}{
		{"missing", head, nil, true, false, ""},
		{"stale revision", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketPass}}, false, false, "published revision"},
		{"pending", head, []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketPending}}, true, false, ""},
		{"skipped", head, []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketSkip}}, false, true, ""},
		{"cancelled", head, []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketCancel}}, false, true, ""},
		{"failed", head, []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketFail}}, false, true, ""},
		{"green", head, []scm.Check{{Name: "full-suite", Bucket: scm.CheckBucketPass}}, false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evaluateRequiredChecks(tt.checks, []string{"full-suite"}, head, tt.observed)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Pending != tt.wantWait || len(result.Failing) > 0 != tt.wantFail {
				t.Fatalf("result = %+v, want pending=%v fail=%v", result, tt.wantWait, tt.wantFail)
			}
		})
	}
}

func TestCIStep_CIAuthoritativeMarksReadyOnlyForRequiredChecksOnPublishedHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env := fakeCIGH(t, "OPEN", `[{"name":"full-suite","state":"SUCCESS","bucket":"pass"}]`)
	env = append(env, "FAKE_CLI_HEAD_SHA="+headSHA)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.Certification = config.Certification{
		Mode:           config.CertificationModeCIAuthoritative,
		LocalFast:      config.LocalFastCommands{Lint: "true", Typecheck: "true ", Test: "true  "},
		RequiredChecks: []string{"full-suite"},
	}
	sctx.Config.CITimeout = time.Minute
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: headSHA, TargetKind: "upstream", TargetFingerprint: "github:test/repo", Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want cancellation after first reconciliation", err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil {
		t.Fatal("exact-head green required check did not mark CI ready")
	}
}

func TestCIStep_CIAuthoritativeUsesPublishedSHAChecksNotPRChecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env := fakeCIGH(t, "OPEN", `[{"name":"full-suite","state":"SUCCESS","bucket":"pass"}]`)
	env = append(env,
		"FAKE_CLI_HEAD_SHA="+headSHA,
		"FAKE_CLI_PR_CHECKS_ERR=pr-current checks should not be used",
	)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.Certification = splitCertificationConfig()
	sctx.Config.CITimeout = time.Minute
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: headSHA, TargetKind: "upstream", TargetFingerprint: "github:test/repo", Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want cancellation after SHA-bound check reconciliation", err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil {
		t.Fatal("published-SHA required check did not mark CI ready")
	}
}

func TestEvaluateRequiredChecksRequiresEveryConfiguredNameOnSameHead(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	result, err := evaluateRequiredChecks([]scm.Check{
		{Name: "full-suite", Bucket: scm.CheckBucketPass},
		{Name: "production-build", Bucket: scm.CheckBucketPass},
		{Name: "optional-preview", Bucket: scm.CheckBucketFail},
	}, []string{"full-suite", "production-build"}, head, head)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pending || len(result.Failing) != 0 {
		t.Fatalf("required checks did not pass: %+v", result)
	}
}

package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

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

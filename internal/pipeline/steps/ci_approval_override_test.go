package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestCIStep_VerifyApprovalOverride pins CIStep's implementation of
// pipeline.ApprovalOverrideVerifier against the real scm.Host/gh plumbing
// (via the fakecli gh double every other CI step test uses), not just the
// executor-level fake used in internal/pipeline's regression tests. See
// pipeline.ApprovalOverrideVerifier's doc for the incident this exists for:
// a human approving a CI gate must never let a still-failing live check read
// as a clean pass.
func TestCIStep_VerifyApprovalOverride(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		checksJSON     string
		wantUnresolved bool
		wantContains   string
	}{
		{
			name:           "still failing",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"PR must be raised via no-mistakes","state":"FAILURE","bucket":"fail"}]`,
			wantUnresolved: true,
			wantContains:   "PR must be raised via no-mistakes",
		},
		{
			name:           "became green",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"PR must be raised via no-mistakes","state":"SUCCESS","bucket":"pass"}]`,
			wantUnresolved: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			env := fakeCIGH(t, "OPEN", tc.checksJSON)

			prURL := "https://github.com/test/repo/pull/42"
			sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})
			sctx.Env = env
			sctx.Run.PRURL = &prURL

			step := &CIStep{}
			unresolved, err := step.VerifyApprovalOverride(sctx)
			if err != nil {
				t.Fatalf("VerifyApprovalOverride() error = %v", err)
			}
			if tc.wantUnresolved && unresolved == "" {
				t.Fatal("unresolved = \"\", want a reason naming the still-failing check")
			}
			if !tc.wantUnresolved && unresolved != "" {
				t.Fatalf("unresolved = %q, want \"\" once every check passed", unresolved)
			}
			if tc.wantContains != "" && !strings.Contains(unresolved, tc.wantContains) {
				t.Errorf("unresolved = %q, want it to name %q", unresolved, tc.wantContains)
			}
		})
	}
}

// TestCIStep_VerifyApprovalOverride_NoPRURL covers the "cannot verify" fail-
// closed path: a run with no PR URL yet cannot have a live state to check
// against, so this must report an unresolved reason (never silently clear),
// matching ApprovalOverrideVerifier's documented fail-closed contract.
func TestCIStep_VerifyApprovalOverride_NoPRURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})

	step := &CIStep{}
	unresolved, err := step.VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want a fail-closed reason when there is no PR URL to verify")
	}
}

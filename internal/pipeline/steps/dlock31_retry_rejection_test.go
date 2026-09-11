package steps

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestDLOCK31RetainedRetryFailsClosed(t *testing.T) {
	for _, condition := range []string{"unbound", "auth_failure", "missing_pr", "attestation_still_failing", "moved_anchor", "changed_owner_branch"} {
		t.Run(condition, func(t *testing.T) {
			f := newDLOCK31Publication(t)
			switch condition {
			case "unbound":
				if err := f.sctx.DB.ClearRetainedCIRepair(f.sctx.Run.ID, f.retained); err != nil {
					t.Fatal(err)
				}
			case "auth_failure":
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_AUTH_ERR=fixture unauthorized")
			case "missing_pr":
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_LIST_JSON=[]", "FAKE_CLI_STATE=CLOSED")
			case "attestation_still_failing":
				f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_EDIT_ERR=fixture unavailable")
			case "moved_anchor":
				gitCmd(t, f.sctx.WorkDir, "update-ref", "refs/no-mistakes/ci-repair/"+f.sctx.Run.ID, f.published)
			case "changed_owner_branch":
				f.sctx.Run.Branch = "refs/heads/other"
			}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("retry did not park: %+v %v", outcome, err)
			}
			remote, err := os.ReadFile(f.remoteFile)
			if err != nil || string(remote) != f.published || f.agentCalls != 0 {
				t.Fatalf("unsafe side effects: remote=%s agent=%d err=%v", remote, f.agentCalls, err)
			}
			stored, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || stored.HeadSHA != f.published {
				t.Fatal("refusal rewrote recorded head")
			}
			if gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD") != f.retained {
				t.Fatal("refusal lost correction")
			}
		})
	}
}

func TestDLOCK31RetainedAttestationPolicyChangesFailClosed(t *testing.T) {
	for _, changed := range []string{"unsupported_provider", "branch_became_base"} {
		t.Run(changed, func(t *testing.T) {
			f := newDLOCK31Publication(t)
			if changed == "unsupported_provider" {
				f.sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderUnknown}
			} else {
				f.sctx.Config.PR.BaseBranch = "feature"
			}
			_, outcome := (&CIStep{}).retryRetainedPublication(f.sctx)
			remote, err := os.ReadFile(f.remoteFile)
			if outcome == nil || !outcome.NeedsApproval || err != nil || string(remote) != f.published {
				t.Fatalf("policy change bypassed retained attestation: outcome=%+v remote=%s error=%v", outcome, remote, err)
			}
		})
	}
}

func TestDLOCK31NextRepairCanReplaceOnlyPublishedAnchor(t *testing.T) {
	f := newDLOCK31Publication(t)
	if _, err := (&CIStep{}).publishRepair(f.sctx, f.retained); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.sctx.WorkDir, "commit", "--allow-empty", "-m", "next repair")
	next := gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD")
	f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_EDIT_ERR=fixture unavailable")
	if _, err := (&CIStep{}).publishRepair(f.sctx, next); err == nil {
		t.Fatal("expected attestation failure")
	}
	binding, err := f.sctx.DB.RetainedCIRepair(f.sctx.Run.ID)
	if err != nil || binding == nil || binding.RetainedHead != next || binding.RecordedHead != f.retained {
		t.Fatalf("next repair did not bind: %+v %v", binding, err)
	}
}

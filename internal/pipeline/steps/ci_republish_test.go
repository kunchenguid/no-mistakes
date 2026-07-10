package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestCIStep_VerifyCIPatchGatesRejection proves the CI patch verifier fails
// closed on a blocking verdict and passes on a clean one, before any commit.
func TestCIStep_VerifyCIPatchGatesRejection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	reject := &mockAgent{
		name:             "test",
		ciVerifierOutput: `{"findings":[{"severity":"error","file":"main.go","description":"regression"}],"summary":"unsafe CI patch"}`,
	}
	sctx := newTestContext(t, reject, dir, "base", "head", config.Commands{})
	step := &CIStep{}
	if err := step.verifyCIPatch(sctx, "base"); err == nil {
		t.Fatal("expected the CI patch verifier to fail closed on a blocking verdict")
	}

	clean := &mockAgent{name: "test"} // empty ciVerifierOutput -> clean verdict
	sctxClean := newTestContext(t, clean, dir, "base", "head", config.Commands{})
	if err := step.verifyCIPatch(sctxClean, "base"); err != nil {
		t.Fatalf("expected a clean verifier verdict to pass, got %v", err)
	}
}

// TestCIStep_RepublishSealsVerifiedSHA proves a successful CI republish seals the
// exact pushed SHA under the ci_republish reason.
func TestCIStep_RepublishSealsVerifiedSHA(t *testing.T) {
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

	os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected commitAndPush to report a republish")
	}

	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil {
		t.Fatal("expected a ci_republish seal after republish")
	}
	if seal.SHA != newHead {
		t.Fatalf("ci_republish seal SHA = %s, want republished HEAD %s", seal.SHA, newHead)
	}
}

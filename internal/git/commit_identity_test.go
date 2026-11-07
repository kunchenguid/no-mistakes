package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// A checkout can have no local identity and still select a different identity
// through a conditional global include. The gate must keep that effective value.
func TestCommitIdentityConditionalIncludeBindsDestination(t *testing.T) {
	ctx := context.Background()
	src, dst := initTestRepo(t), initTestRepo(t)
	run(t, src, "git", "config", "--local", "--unset", "user.name")
	run(t, src, "git", "config", "--local", "--unset", "user.email")
	global := filepath.Join(t.TempDir(), "global")
	identity := filepath.Join(t.TempDir(), "submitter")
	if err := os.WriteFile(identity, []byte("[user]\nname = Submitter\nemail = submitter@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[user]\nname = Ambient\nemail = ambient@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	gitDir := run(t, src, "git", "rev-parse", "--absolute-git-dir")
	run(t, src, "git", "config", "--file", global, "includeIf.gitdir/i:"+filepath.ToSlash(gitDir)+".path", identity)
	bound, err := CaptureCommitIdentity(ctx, src)
	if err != nil || bound.AuthorName != "Submitter" || bound.CommitterEmail != "submitter@example.test" {
		t.Fatalf("effective submission identity = %+v, %v", bound, err)
	}
	if err := CopyLocalUserIdentity(ctx, dst, bound); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"user.name": "Submitter", "user.email": "submitter@example.test"} {
		if got := run(t, dst, "git", "config", "--local", "--get", key); got != want {
			t.Errorf("%s = %q, want effective checkout value %q", key, got, want)
		}
	}
}

func TestCommitIdentityPinsBothRolesDespiteLaterAmbientOverrides(t *testing.T) {
	dir := initTestRepo(t)
	t.Setenv("GIT_AUTHOR_NAME", "Submitting Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Submitting Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.test")
	identity, err := CaptureCommitIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithCommitIdentity(context.Background(), identity)
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(key, "Daemon")
	}
	for _, key := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "daemon@example.test")
	}
	if _, err := Run(ctx, dir, "commit", "--allow-empty", "-m", "correction"); err != nil {
		t.Fatal(err)
	}
	got, err := Run(ctx, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := "Submitting Author <author@example.test> | Submitting Committer <committer@example.test>"
	if got != want {
		t.Fatalf("commit identity = %q, want %q", got, want)
	}
}

func TestCommitIdentityRefusesMissingIdentity(t *testing.T) {
	dir := initTestRepo(t)
	for _, key := range []string{"user.name", "user.email"} {
		run(t, dir, "git", "config", "--local", "--unset", key)
	}
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(key, "")
	}
	head := run(t, dir, "git", "rev-parse", "HEAD")
	if _, err := CaptureCommitIdentity(context.Background(), dir); err == nil {
		t.Fatal("missing identity was accepted")
	}
	if got := run(t, dir, "git", "rev-parse", "HEAD"); got != head {
		t.Fatal("identity refusal changed HEAD")
	}
}

func TestCommitIdentityUsesWorktreeConfiguration(t *testing.T) {
	dir := initTestRepo(t)
	run(t, dir, "git", "config", "extensions.worktreeConfig", "true")
	run(t, dir, "git", "config", "--worktree", "user.name", "Worktree Submitter")
	run(t, dir, "git", "config", "--worktree", "user.email", "worktree@example.test")
	identity, err := CaptureCommitIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if identity.AuthorName != "Worktree Submitter" || identity.CommitterEmail != "worktree@example.test" {
		t.Fatalf("lost worktree identity: %+v", identity)
	}
}

func TestCommitIdentitySnapshotAlsoPinsWorktreeConfiguration(t *testing.T) {
	src, dst := initTestRepo(t), initTestRepo(t)
	t.Setenv("GIT_AUTHOR_NAME", "Submitting Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Submitting Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.test")
	identity, err := CaptureCommitIdentity(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_NAME", "Daemon")
	t.Setenv("GIT_AUTHOR_EMAIL", "daemon@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "Daemon")
	t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
	if err := CopyLocalUserIdentity(context.Background(), dst, identity); err != nil {
		t.Fatal(err)
	}
	// A Git child with no identity environment must still get the submitting
	// roles from the worktree, rather than re-resolving the daemon's defaults.
	ctx := WithEnvironment(context.Background(), runenv.Overlay{Unset: []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	}})
	if _, err := Run(ctx, dst, "commit", "--allow-empty", "-m", "config-bound correction"); err != nil {
		t.Fatal(err)
	}
	got, err := Run(ctx, dst, "show", "-s", "--format=%an <%ae> | %cn <%ce>")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Submitting Author <author@example.test> | Submitting Committer <committer@example.test>"; got != want {
		t.Fatalf("worktree-config commit = %q, want %q", got, want)
	}
}

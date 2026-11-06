package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A checkout can have no local identity and still select a different identity
// through a conditional global include. The gate must keep that effective value.
func TestCopyLocalUserIdentityUsesEffectiveCheckoutIdentity(t *testing.T) {
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
	run(t, src, "git", "config", "--file", global, "includeIf.gitdir:"+filepath.ToSlash(src)+"/.git.path", identity)
	if err := CopyLocalUserIdentity(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"user.name": "Submitter", "user.email": "submitter@example.test"} {
		if got := run(t, dst, "git", "config", "--local", "--get", key); got != want {
			t.Errorf("%s = %q, want effective checkout value %q", key, got, want)
		}
	}
}

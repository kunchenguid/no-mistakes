package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestObjectMissingSeparatesAbsenceFromUnreadability pins the whole reason
// this helper exists: absence must be positive evidence, so a present object,
// a genuinely absent one, and a directory Git cannot read must be three
// distinct answers rather than the single "non-zero exit" that `cat-file -e`
// collapses them into.
func TestObjectMissingSeparatesAbsenceFromUnreadability(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	objectMissingRun(t, repo, "init", "-b", "main", ".")
	objectMissingRun(t, repo, "config", "user.email", "test@example.com")
	objectMissingRun(t, repo, "config", "user.name", "Test User")
	objectMissingRun(t, repo, "commit", "--allow-empty", "-m", "base")
	head := objectMissingRun(t, repo, "rev-parse", "HEAD")

	missing, err := ObjectMissing(ctx, repo, head)
	if err != nil || missing {
		t.Fatalf("present object: missing=%v err=%v", missing, err)
	}

	absent := strings.Repeat("a", 40)
	missing, err = ObjectMissing(ctx, repo, absent)
	if err != nil || !missing {
		t.Fatalf("absent object: missing=%v err=%v", missing, err)
	}

	// A directory that is not a repository must be undetermined, never
	// reported as proof that the object is gone.
	notARepo := t.TempDir()
	missing, err = ObjectMissing(ctx, notARepo, absent)
	if err == nil || missing {
		t.Fatalf("non-repository: missing=%v err=%v, want an error and missing=false", missing, err)
	}

	if _, err := ObjectMissing(ctx, repo, "  "); err == nil {
		t.Fatal("empty object id was accepted")
	}
}

// TestObjectMissingReadsABareRepositoryObjectStore covers the gate shape: the
// local gate is bare, and safe.bareRepository=explicit forbids cwd discovery
// (issue #362), so the probe must name the git dir like every other gate call.
func TestObjectMissingReadsABareRepositoryObjectStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	objectMissingRun(t, root, "init", "-b", "main", source)
	objectMissingRun(t, source, "config", "user.email", "test@example.com")
	objectMissingRun(t, source, "config", "user.name", "Test User")
	objectMissingRun(t, source, "commit", "--allow-empty", "-m", "base")
	head := objectMissingRun(t, source, "rev-parse", "HEAD")
	gate := filepath.Join(root, "gate.git")
	objectMissingRun(t, root, "clone", "--bare", source, gate)

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")

	missing, err := ObjectMissing(ctx, gate, head)
	if err != nil || missing {
		t.Fatalf("bare gate present object: missing=%v err=%v", missing, err)
	}
	missing, err = ObjectMissing(ctx, gate, strings.Repeat("b", 40))
	if err != nil || !missing {
		t.Fatalf("bare gate absent object: missing=%v err=%v", missing, err)
	}
}

func objectMissingRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

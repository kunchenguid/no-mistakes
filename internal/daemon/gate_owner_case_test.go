package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// swapCase flips the letter case of every ASCII letter in s.
func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

// filesystemFoldsCase reports whether dir's filesystem resolves two spellings
// that differ only in letter case to the same entry, by asking the filesystem
// instead of assuming from GOOS (APFS and NTFS volumes can be either).
func filesystemFoldsCase(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// TestOwnedGateAcceptsACaseVariantSpellingOnACaseInsensitiveFilesystem pins
// issue #1452: the account's home is registered as /Users/Name while the
// directory on disk is /Users/name, so the gate git hands the hook is spelled
// differently from the root the daemon runs with. canonicalRoot only folds
// case on Windows, so the textual compare refused every push even though both
// spellings are one directory.
func TestOwnedGateAcceptsACaseVariantSpellingOnACaseInsensitiveFilesystem(t *testing.T) {
	base := t.TempDir()
	if !filesystemFoldsCase(t, base) {
		t.Skip("filesystem is case-sensitive: a case variant is a different directory here")
	}

	const repoID = "case-variant-repo"
	p := paths.WithRoot(filepath.Join(base, "NmHome"))
	if err := os.MkdirAll(p.RepoDir(repoID), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(base, "nmhome", "repos", repoID+".git")

	got, err := ownedGateRepoID(p, gate)
	if err != nil {
		t.Fatalf("a case-variant spelling of the daemon's own gate must be owned: %v", err)
	}
	if got != repoID {
		t.Fatalf("repo id = %q, want %q", got, repoID)
	}
}

// TestOwnedGateRefusesACaseVariantThatIsAnotherDirectory keeps the guard
// fail-closed where case is significant: there a differently-cased gate path is
// a different directory, possibly a foreign root, and must never be admitted.
func TestOwnedGateRefusesACaseVariantThatIsAnotherDirectory(t *testing.T) {
	base := t.TempDir()
	if filesystemFoldsCase(t, base) {
		t.Skip("filesystem is case-insensitive: a case variant is the same directory here")
	}

	const repoID = "case-sensitive-repo"
	p := paths.WithRoot(filepath.Join(base, "NmHome"))
	if err := os.MkdirAll(p.RepoDir(repoID), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(base, swapCase("NmHome"), "repos", repoID+".git")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := ownedGateRepoID(p, foreign); err == nil {
		t.Fatal("a differently-cased gate that is a distinct directory must be refused")
	}
}

// TestOwnedGateRefusesAMissingCaseVariant covers the other fail-closed edge: a
// gate path that cannot be identified on disk is never accepted on a
// case-folded guess.
func TestOwnedGateRefusesAMissingCaseVariant(t *testing.T) {
	base := t.TempDir()
	const repoID = "missing-variant-repo"
	p := paths.WithRoot(filepath.Join(base, "NmHome"))
	if err := os.MkdirAll(p.RepoDir(repoID), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "OtherHome", "repos", repoID+".git")

	if _, err := ownedGateRepoID(p, other); err == nil {
		t.Fatal("a gate under a different, non-existent root must be refused")
	}
}

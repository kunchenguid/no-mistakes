package publication

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

const identityFullSHA = "0123456789abcdef0123456789abcdef01234567"

func identityBuildInfo(revision, modified string) (*debug.BuildInfo, bool) {
	return &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: revision},
		{Key: "vcs.modified", Value: modified},
	}}, true
}

func TestResolvePublisherBuildSHAAcceptsFullLdflagOrCleanVCSRevision(t *testing.T) {
	t.Run("full ldflag", func(t *testing.T) {
		got, err := resolvePublisherBuildSHA(identityFullSHA, nil, false)
		if err != nil || got != identityFullSHA {
			t.Fatalf("resolve full ldflag = %q, %v", got, err)
		}
	})

	t.Run("short matching clean VCS revision", func(t *testing.T) {
		info, ok := identityBuildInfo(identityFullSHA, "false")
		got, err := resolvePublisherBuildSHA(identityFullSHA[:7], info, ok)
		if err != nil || got != identityFullSHA {
			t.Fatalf("resolve short ldflag = %q, %v", got, err)
		}
	})

	t.Run("clean VCS fallback", func(t *testing.T) {
		info, ok := identityBuildInfo(identityFullSHA, "false")
		got, err := resolvePublisherBuildSHA("unknown", info, ok)
		if err != nil || got != identityFullSHA {
			t.Fatalf("resolve VCS fallback = %q, %v", got, err)
		}
	})
}

func TestResolvePublisherBuildSHAFailsClosedForMismatchMissingDirtyOrInvalid(t *testing.T) {
	clean, cleanOK := identityBuildInfo(identityFullSHA, "false")
	dirty, dirtyOK := identityBuildInfo(identityFullSHA, "true")
	invalidRevision, invalidRevisionOK := identityBuildInfo(strings.Repeat("z", 40), "false")
	tests := map[string]struct {
		ldflag string
		info   *debug.BuildInfo
		ok     bool
	}{
		"short mismatch":       {ldflag: "7654321", info: clean, ok: cleanOK},
		"short missing VCS":    {ldflag: identityFullSHA[:7]},
		"short dirty VCS":      {ldflag: identityFullSHA[:7], info: dirty, ok: dirtyOK},
		"fallback missing VCS": {ldflag: "unknown"},
		"fallback dirty VCS":   {ldflag: "unknown", info: dirty, ok: dirtyOK},
		"invalid ldflag":       {ldflag: "not-a-sha", info: clean, ok: cleanOK},
		"invalid VCS revision": {ldflag: "unknown", info: invalidRevision, ok: invalidRevisionOK},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := resolvePublisherBuildSHA(test.ldflag, test.info, test.ok); err == nil {
				t.Fatalf("resolvePublisherBuildSHA() = %q, want fail-closed error", got)
			}
		})
	}
}

func TestPublisherBindingHashesExactRegularExecutableBytesAndDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-mistakes")
	first := []byte("first executable bytes\n")
	if err := os.WriteFile(path, first, 0o700); err != nil {
		t.Fatal(err)
	}

	firstBinding, err := publisherBindingWithBuildInfo(path, identityFullSHA, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest := sha256.Sum256(first)
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstBinding.ExecutablePath != resolvedPath || firstBinding.ExecutableSHA256 != hex.EncodeToString(firstDigest[:]) ||
		firstBinding.BuildSHA != identityFullSHA || firstBinding.Protocol != ProtocolV1 {
		t.Fatalf("first publisher binding = %#v", firstBinding)
	}

	replacement := filepath.Join(dir, "replacement")
	second := []byte("replacement executable bytes\n")
	if err := os.WriteFile(replacement, second, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	secondBinding, err := publisherBindingWithBuildInfo(path, identityFullSHA, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest := sha256.Sum256(second)
	if secondBinding.ExecutableSHA256 != hex.EncodeToString(secondDigest[:]) || secondBinding.ExecutableSHA256 == firstBinding.ExecutableSHA256 {
		t.Fatalf("replacement hash = %q, first = %q", secondBinding.ExecutableSHA256, firstBinding.ExecutableSHA256)
	}
}

func TestPublisherBindingRejectsRelativeOrNonRegularExecutable(t *testing.T) {
	if _, err := publisherBindingWithBuildInfo("relative/no-mistakes", identityFullSHA, nil, false); err == nil {
		t.Fatal("relative executable path accepted")
	}
	if _, err := publisherBindingWithBuildInfo(t.TempDir(), identityFullSHA, nil, false); err == nil {
		t.Fatal("directory accepted as publisher executable")
	}
}

package scm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAuthCheckCommand(t *testing.T) {
	tests := []struct {
		provider Provider
		want     []string
	}{
		{ProviderGitHub, []string{"gh", "auth", "status"}},
		{ProviderGitLab, []string{"glab", "auth", "status"}},
		{ProviderBitbucket, []string{"bb", "profile", "which"}},
	}

	for _, tt := range tests {
		got := tt.provider.AuthCheckCommand()
		if len(got) != len(tt.want) {
			t.Fatalf("%q AuthCheckCommand len = %d, want %d", tt.provider, len(got), len(tt.want))
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("%q AuthCheckCommand[%d] = %q, want %q", tt.provider, i, got[i], tt.want[i])
			}
		}
	}
}

func TestCLIAvailable(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"gh", "bb"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(""), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if !CLIAvailable(ProviderGitHub) {
		t.Fatal("expected gh to be available")
	}
	if !CLIAvailable(ProviderBitbucket) {
		t.Fatal("expected bb to be available")
	}
	if CLIAvailable(ProviderGitLab) {
		t.Fatal("did not expect glab to be available")
	}
	if CLIAvailable(ProviderUnknown) {
		t.Fatal("did not expect unknown provider to be available")
	}
}

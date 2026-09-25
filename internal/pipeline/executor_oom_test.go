package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestOOMFailureKeepsRestorationDetail(t *testing.T) {
	const snapshot = "/tmp/nm-recovery-snapshot"
	orig := fmt.Errorf("restore worktree: %w; retained snapshot %s", shellenv.ErrOutOfMemory, snapshot)
	got := keepOOMDetail(orig)
	if !errors.Is(got, shellenv.ErrOutOfMemory) {
		t.Fatal("joined error must still be an out-of-memory error")
	}
	text := got.Error()
	if !strings.Contains(text, "restore worktree") || !strings.Contains(text, snapshot) {
		t.Fatalf("error dropped restoration detail: %s", text)
	}
}

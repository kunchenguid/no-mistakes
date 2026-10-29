package git

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureDirtyWork_RefusesCustomAttributesBeforeCleanReturn(t *testing.T) {
	for _, source := range []string{"relative", "absolute"} {
		for _, state := range []string{"clean", "dirty"} {
			t.Run(source+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "custom clean attributes")
				run(t, wt, "git", "config", "core.eol", "lf")
				writeFile(t, filepath.Join(wt, ".gitignore"), "local.attrs\n")
				run(t, wt, "git", "add", ".gitignore")
				run(t, wt, "git", "commit", "-m", "ignore local attribute source")
				marker := "ignored-custom-attribute-source-not-collected"
				attributePath := filepath.Join(wt, "local.attrs")
				attributes := "# " + marker + "\nfile.txt text eol=crlf\n"
				writeFile(t, attributePath, attributes)
				configuredPath := "local.attrs"
				if source == "absolute" {
					configuredPath = attributePath
				}
				run(t, wt, "git", "config", "core.attributesFile", configuredPath)
				writeFile(t, filepath.Join(wt, "file.txt"), "base\r\n")
				if state == "dirty" {
					writeFile(t, filepath.Join(wt, "unrelated.txt"), "unique unrelated work\n")
				}
				if err := DirtyWorkStillMatches(ctx, wt, nil); !errors.Is(err, ErrDirtyWorkRejected) || !strings.Contains(err.Error(), "custom attributes file") {
					t.Fatalf("confirmation accepted an unverified custom source: %v", err)
				}
				if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); !errors.Is(err, ErrDirtyWorkRejected) || !strings.Contains(err.Error(), "custom attributes file") {
					t.Fatalf("cleanup accepted an unverified custom source: %v", err)
				}
				assertNoDirtySnapshot(t, gate)
				assertObjectStoreLacks(t, gate, marker)
				if got := string(mustRead(t, filepath.Join(wt, "file.txt"))); got != "base\r\n" {
					t.Fatalf("rejected cleanup changed tracked bytes: %q", got)
				}
				if got := string(mustRead(t, attributePath)); got != attributes {
					t.Fatalf("rejected cleanup changed ignored attributes: %q", got)
				}
			})
		}
	}
}

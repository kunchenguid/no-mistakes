package git

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirtyWorkErrorsRedactCredentials(t *testing.T) {
	const fixtureURL = "https://fixture-user:fixture-token@example.invalid/blob"
	for _, operation := range []string{"filter", "arguments", "ignored-paths"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "redacted errors")
			var err error
			switch operation {
			case "filter":
				writeFile(t, filepath.Join(wt, ".gitattributes"), "file.txt filter=failing\n")
				run(t, wt, "git", "config", "filter.failing.clean", "cat")
				run(t, wt, "git", "config", "filter.failing.smudge", "printf '%s\\n' '"+fixtureURL+"' >&2; exit 1")
				run(t, wt, "git", "config", "filter.failing.required", "true")
				_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			case "arguments":
				err = gitStreamTo(ctx, wt, nil, io.Discard, "-c", "fixture.url="+fixtureURL, "not-a-git-command")
			case "ignored-paths":
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
				t.Setenv("GIT_CONFIG_VALUE_0", fixtureURL)
				_, err = ignoredPathSet(ctx, wt, []string{"file.txt"})
			}
			if err == nil {
				t.Fatal("fixture did not produce a Git error")
			}
			message := err.Error()
			if strings.Contains(message, "fixture-user") || strings.Contains(message, "fixture-token") {
				t.Fatal("Git error exposed fixture credential material")
			}
			if !strings.Contains(message, "example.invalid") || !strings.Contains(message, "redacted") {
				t.Fatal("Git error lost useful redacted URL context")
			}
		})
	}
}

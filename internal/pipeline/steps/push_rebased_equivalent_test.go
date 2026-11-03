package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestPushStep_PublishesRebasedEquivalentsOverAnEarlierRunsHead reproduces a
// new run on a branch an earlier run already published. The new run's head
// carries one more commit that edits the lines the published commits wrote,
// and the rebase step moved it onto a newer main, so the private mirror still
// holds the earlier run's pre-rebase copies of the same commits. That head is
// not run-owned, so Decision 41-A does not apply and the preservation proof
// must: the rebased copies survive in the live history, so the push goes
// through, while a commit whose content the live head genuinely lacks still
// refuses, naming the whole private-only range.
func TestPushStep_PublishesRebasedEquivalentsOverAnEarlierRunsHead(t *testing.T) {
	for _, variant := range []string{"rebased_equivalents", "dropped_commits", "mixed"} {
		t.Run(variant, func(t *testing.T) {
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			dir, baseSHA, _ := setupGitRepo(t)
			write := func(name, content string) {
				t.Helper()
				full := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			commit := func(message string) string {
				t.Helper()
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-q", "-m", message)
				return gitCmd(t, dir, "rev-parse", "HEAD")
			}
			const widgetV1 = "package widget\n\nconst Size = 1\n"
			const widgetV2 = "package widget\n\nconst Size = 2\n"
			const notes = "widget notes\n"

			// The earlier run published widget + notes to the PR branch.
			gitCmd(t, dir, "checkout", "-q", "-B", "feature", baseSHA)
			write("src/widget.go", widgetV1)
			widget := commit("feat: widget")
			write("docs/widget.md", notes)
			published := commit("docs: widget notes")
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "-q", "origin", "main", "feature")

			// The author adds a commit that edits the widget's own lines and
			// starts a new run from it.
			write("src/widget.go", widgetV2)
			submitted := commit("fix: widget size")

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
			sctx.Repo.UpstreamURL = upstream
			gateDir := setupGateMirror(t, sctx)
			gitCmd(t, gateDir, "fetch", dir, published+":refs/heads/feature")

			// Main moves on, and the rebase step replays the branch onto it.
			gitCmd(t, dir, "checkout", "-q", "main")
			write("other.txt", "unrelated main work\n")
			commit("chore: unrelated main work")
			gitCmd(t, dir, "push", "-q", "origin", "main")
			gitCmd(t, dir, "checkout", "-q", "-B", "feature", "main")
			switch variant {
			case "rebased_equivalents":
				gitCmd(t, dir, "cherry-pick", widget, published, submitted)
			case "dropped_commits":
				write("src/widget.go", widgetV2)
				commit("feat: widget, rewritten")
			case "mixed":
				gitCmd(t, dir, "cherry-pick", widget, submitted)
			}
			rebased := gitCmd(t, dir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, rebased); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = rebased
			recordReviewApproval(t, sctx, rebased)

			_, err := (&PushStep{}).Execute(sctx)
			if variant != "rebased_equivalents" {
				if err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") {
					t.Fatalf("genuinely dropped content was published: %v", err)
				}
				if !strings.Contains(err.Error(), published) || !strings.Contains(err.Error(), "docs: widget notes") {
					t.Fatalf("refusal did not name the dropped commit %s: %v", published, err)
				}
				if !strings.Contains(err.Error(), widget) {
					t.Fatalf("refusal did not name the whole private-only range: %v", err)
				}
				for _, repo := range []string{upstream, gateDir} {
					if got := gitCmd(t, repo, "rev-parse", "refs/heads/feature"); got != published {
						t.Fatalf("refused publication moved %s to %s, want %s", repo, got, published)
					}
				}
				if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
					t.Fatalf("refused publication archived a head: %s", tags)
				}
				return
			}
			if err != nil {
				t.Fatalf("rebased equivalents of the published commits were refused: %v", err)
			}
			for _, repo := range []string{upstream, gateDir} {
				if got := gitCmd(t, repo, "rev-parse", "refs/heads/feature"); got != rebased {
					t.Fatalf("%s = %s, want rebased head %s", repo, got, rebased)
				}
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/feature/"+published+"^{commit}"); got != published {
				t.Fatalf("replaced mirror head archived as %s, want %s", got, published)
			}
		})
	}
}

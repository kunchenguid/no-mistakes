package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestPushStep_RebasedPrivateContentRequiresCurrentTreeProof checks that a
// clean replay can publish when its current tree proves containment, while an
// edited private copy from an earlier run is refused because that run owns no
// exception. Dropped content also refuses and names the whole private range.
func TestPushStep_RebasedPrivateContentRequiresCurrentTreeProof(t *testing.T) {
	for _, variant := range []string{"clean_rebased_equivalents", "conflicting_rebased_equivalents", "dropped_commits", "mixed"} {
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

			// The new run starts from the earlier published head. Its clean
			// variant adds unrelated content; the other variants edit the widget.
			if variant == "clean_rebased_equivalents" {
				write("docs/followup.md", "unrelated follow-up\n")
			} else {
				write("src/widget.go", widgetV2)
			}
			submitted := commit("follow-up: author change")

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
			case "clean_rebased_equivalents", "conflicting_rebased_equivalents":
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
			if variant != "clean_rebased_equivalents" {
				if err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") {
					t.Fatalf("unsafe private content was published: %v", err)
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
				t.Fatalf("clean rebased equivalents were refused: %v", err)
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

package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestPushStep_RepublishesReviewedRebaseOverRunsOwnPublishedHead reproduces a
// run that published a head carrying a pipeline commit, then took a CI
// merge-conflict repair: the repair always rebases, so it is revalidated from
// Review and published again. The mirror still holds the run's own last pushed
// head, whose patches the conflict resolution changed, so only Decision 41-A
// for that exact head lets the reviewed rebase replace it. The same in-memory
// run is reused across both publications, as the executor does.
func TestPushStep_RepublishesReviewedRebaseOverRunsOwnPublishedHead(t *testing.T) {
	for _, variant := range []string{"renumbered_migration", "plain_conflict_resolution", "external_mirror_head", "remote_moved_out_of_band"} {
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
			const migration = "CREATE TABLE upload_intents (id text primary key);\n"

			gitCmd(t, dir, "checkout", "-q", "-B", "feature", baseSHA)
			write("migrations/0279_upload_intents.sql", migration)
			write("migrations/journal.txt", "0279 upload_intents\n")
			submitted := commit("feat: upload intents")
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "-q", "origin", "main")

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submitted, config.Commands{})
			sctx.Repo.UpstreamURL = upstream
			gateDir := setupGateMirror(t, sctx)
			gitCmd(t, gateDir, "fetch", dir, submitted+":refs/heads/feature")

			// A pipeline commit lands before the first publication, so the run's
			// published head differs from its submitted head.
			write("src/finalize.ts", "export const finalize = () => 1;\n")
			published := commit("no-mistakes(lint): extract helpers")
			sctx.Run.HeadSHA = published
			recordReviewApproval(t, sctx, published)
			if _, err := (&PushStep{}).Execute(sctx); err != nil {
				t.Fatalf("first publication: %v", err)
			}
			for _, dir := range []string{upstream, gateDir} {
				if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != published {
					t.Fatalf("first publication left %s at %s, want %s", dir, got, published)
				}
			}
			// Model an executor snapshot that still remembers an older generation:
			// publication advances LastPushedSHA durably, while the in-memory run may
			// survive across the later CI repair and Review cycle.
			sctx.Run.LastPushedSHA = &submitted

			// The base branch takes the same migration number meanwhile.
			gitCmd(t, dir, "checkout", "-q", "main")
			write("migrations/0279_fiscal_profile.sql", "CREATE TABLE fiscal (id int);\n")
			write("migrations/journal.txt", "0279 fiscal_profile\n")
			commit("feat: fiscal profile")
			gitCmd(t, dir, "push", "-q", "origin", "main")

			mirrorHead := published
			remoteHead := published
			switch variant {
			case "external_mirror_head":
				// Someone else's commit reaches the private mirror on top of the
				// published head: it is not run-owned and must keep the proof.
				gitCmd(t, dir, "checkout", "-q", "--detach", published)
				write("external.txt", "external work\n")
				mirrorHead = commit("external work")
				gitCmd(t, gateDir, "fetch", dir, mirrorHead+":refs/heads/feature")
			case "remote_moved_out_of_band":
				outOfBand := t.TempDir()
				gitCmd(t, outOfBand, "clone", "-q", "--branch", "feature", upstream, outOfBand)
				if err := os.WriteFile(filepath.Join(outOfBand, "out-of-band.txt"), []byte("someone else's work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, outOfBand, "add", "-A")
				gitCmd(t, outOfBand, "commit", "-q", "-m", "out-of-band commit")
				gitCmd(t, outOfBand, "push", "-q", "origin", "feature")
				remoteHead = gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
			}

			// The CI merge-conflict repair rebases onto the moved base and
			// resolves the conflict, rewriting every patch of the published range.
			gitCmd(t, dir, "checkout", "-q", "-B", "feature", "main")
			if variant == "plain_conflict_resolution" {
				write("migrations/0279_upload_intents.sql", migration)
				write("migrations/journal.txt", "0279 fiscal_profile\n0279b upload_intents\n")
			} else {
				write("migrations/0280_upload_intents.sql", migration)
				write("migrations/journal.txt", "0279 fiscal_profile\n0280 upload_intents\n")
			}
			commit("feat: upload intents")
			write("src/finalize.ts", "export const finalize = () => 1;\n")
			rebased := commit("no-mistakes(lint): extract helpers")

			// recordLocalRepair: the rebased head is recorded and review
			// authority is revoked until Review approves it again.
			if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, rebased); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = rebased
			sctx.Run.ReviewApprovedHeadSHA = nil
			assertUnchanged := func() {
				t.Helper()
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != remoteHead {
					t.Fatalf("remote moved to %s, want %s", got, remoteHead)
				}
				if got := gitCmd(t, gateDir, "rev-parse", "refs/heads/feature"); got != mirrorHead {
					t.Fatalf("mirror moved to %s, want %s", got, mirrorHead)
				}
				if tags := gitCmd(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
					t.Fatalf("refused publication archived a head: %s", tags)
				}
			}
			if _, err := (&PushStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "refusing to push") {
				t.Fatalf("unreviewed rebase was published: %v", err)
			}
			assertUnchanged()

			recordReviewApproval(t, sctx, rebased)
			_, err := (&PushStep{}).Execute(sctx)
			switch variant {
			case "external_mirror_head":
				if err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), mirrorHead) {
					t.Fatalf("external mirror head bypassed the preservation proof: %v", err)
				}
				assertUnchanged()
				return
			case "remote_moved_out_of_band":
				if err == nil || !strings.Contains(err.Error(), remoteHead[:12]) {
					t.Fatalf("push did not stay leased on the run's published head: %v", err)
				}
				assertUnchanged()
				return
			}
			if err != nil {
				t.Fatalf("reviewed rebase of the run's own published head was refused: %v", err)
			}
			for _, dir := range []string{upstream, gateDir} {
				if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != rebased {
					t.Fatalf("%s = %s, want reviewed rebase %s", dir, got, rebased)
				}
			}
			if got := gitCmd(t, gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/feature/"+published+"^{commit}"); got != published {
				t.Fatalf("replaced published head archived as %s, want %s", got, published)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != rebased || run.HeadSHA != rebased {
				t.Fatalf("publication not recorded: %+v err=%v", run, err)
			}
		})
	}
}

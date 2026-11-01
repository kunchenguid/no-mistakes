package db

import (
	"path/filepath"
	"testing"
)

func mediaTestRun(t *testing.T, d *DB) *Run {
	t.Helper()
	repo, err := d.InsertRepo("/tmp/widgets", "https://github.com/example/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunMediaAttachmentFromOnePublisherIsAMissForAnother(t *testing.T) {
	d := openTestDB(t)
	run := mediaTestRun(t, d)
	gh := RunMediaAttachment{Path: "/evidence/checkout.png", Digest: "sha256:aa", Publisher: MediaPublisherGitHub, URL: "https://github.com/user-attachments/assets/1"}
	if err := d.UpsertRunMediaAttachment(run.ID, gh); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.GetRunMediaAttachment(run.ID, gh.Path, gh.Digest, "cloudflare-pages:nm-evidence"); err != nil || found {
		t.Fatalf("found=%t err=%v, want a miss for another publisher", found, err)
	}
	pages := RunMediaAttachment{Path: gh.Path, Digest: gh.Digest, Publisher: "cloudflare-pages:nm-evidence", URL: "https://5d4c3b2a.nm-evidence.pages.dev/aa/checkout.png"}
	if err := d.UpsertRunMediaAttachment(run.ID, pages); err != nil {
		t.Fatal(err)
	}
	got, found, err := d.GetRunMediaAttachment(run.ID, gh.Path, gh.Digest, pages.Publisher)
	if err != nil || !found || got != pages {
		t.Fatalf("got %+v found=%t err=%v, want %+v", got, found, err, pages)
	}
	if _, found, _ := d.GetRunMediaAttachment(run.ID, gh.Path, gh.Digest, MediaPublisherGitHub); found {
		t.Fatal("the replaced GitHub URL is still served")
	}
}

func TestRunMediaAttachmentWrittenBeforeThePublisherColumnReadsAsGitHub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := mediaTestRun(t, d)
	if _, err := d.sql.Exec(`ALTER TABLE run_media_attachments DROP COLUMN publisher`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`INSERT INTO run_media_attachments (run_id, path, digest, url, created_at, updated_at) VALUES (?, '/evidence/checkout.png', 'sha256:aa', 'https://github.com/user-attachments/assets/1', 1, 1)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, found, err := reopened.GetRunMediaAttachment(run.ID, "/evidence/checkout.png", "sha256:aa", MediaPublisherGitHub)
	if err != nil || !found || got.URL != "https://github.com/user-attachments/assets/1" {
		t.Fatalf("got %+v found=%t err=%v, want the legacy GitHub row", got, found, err)
	}
}

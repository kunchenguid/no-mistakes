package db

import (
	"database/sql"
	"errors"
)

const MediaPublisherGitHub = "github-user-attachments"

// Digest identifies the file contents, not merely its path.
type RunMediaAttachment struct {
	Path      string
	Digest    string
	Publisher string
	URL       string
}

// A file overwritten during repair misses on its digest, so it is uploaded
// again rather than rendered with stale evidence.
func (d *DB) GetRunMediaAttachment(runID, path, digest, publisher string) (RunMediaAttachment, bool, error) {
	var attachment RunMediaAttachment
	err := d.sql.QueryRow(`SELECT path, digest, publisher, url FROM run_media_attachments WHERE run_id = ? AND path = ? AND digest = ? AND publisher = ?`, runID, path, digest, publisher).
		Scan(&attachment.Path, &attachment.Digest, &attachment.Publisher, &attachment.URL)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunMediaAttachment{}, false, nil
		}
		return RunMediaAttachment{}, false, err
	}
	return attachment, true, nil
}

func (d *DB) UpsertRunMediaAttachment(runID string, attachment RunMediaAttachment) error {
	ts := now()
	_, err := d.sql.Exec(`
		INSERT INTO run_media_attachments (run_id, path, digest, publisher, url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, path) DO UPDATE SET
			digest = excluded.digest,
			publisher = excluded.publisher,
			url = excluded.url,
			updated_at = excluded.updated_at`,
		runID, attachment.Path, attachment.Digest, attachment.Publisher, attachment.URL, ts, ts)
	return err
}

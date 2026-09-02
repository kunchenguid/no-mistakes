package db

import (
	"database/sql"
	"errors"
)

// RunMediaAttachment is a GitHub user-attachment uploaded for one run's local
// evidence file. Digest identifies the file contents, not merely its path.
type RunMediaAttachment struct {
	Path   string
	Digest string
	URL    string
}

// GetRunMediaAttachment returns the cached attachment for path only when its
// content digest still matches. A file overwritten during repair is therefore
// uploaded again rather than being rendered with stale evidence.
func (d *DB) GetRunMediaAttachment(runID, path, digest string) (RunMediaAttachment, bool, error) {
	var attachment RunMediaAttachment
	err := d.sql.QueryRow(`SELECT path, digest, url FROM run_media_attachments WHERE run_id = ? AND path = ? AND digest = ?`, runID, path, digest).
		Scan(&attachment.Path, &attachment.Digest, &attachment.URL)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunMediaAttachment{}, false, nil
		}
		return RunMediaAttachment{}, false, err
	}
	return attachment, true, nil
}

// UpsertRunMediaAttachment records the URL returned for the current contents
// of path. A changed file at the same path replaces the stale cache entry.
func (d *DB) UpsertRunMediaAttachment(runID string, attachment RunMediaAttachment) error {
	ts := now()
	_, err := d.sql.Exec(`
		INSERT INTO run_media_attachments (run_id, path, digest, url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, path) DO UPDATE SET
			digest = excluded.digest,
			url = excluded.url,
			updated_at = excluded.updated_at`,
		runID, attachment.Path, attachment.Digest, attachment.URL, ts, ts)
	return err
}

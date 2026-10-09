// Package verificationplan owns opt-in, local-only run input snapshots.
package verificationplan

import (
	"bytes"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

// Snapshot identifies captured evidence, never instructions or user intent.
// ID reserves the future run ID so an attachment cannot be rebound to a second run.
type Snapshot struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	SHA256     string `json:"sha256"`
	CapturedAt int64  `json:"captured_at"`
}

type capture struct {
	Snapshot
	RepoID  string `json:"repo_id"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
}

// Capture reads the source once, before any branch mutation. The manifest binds
// this input to the proposed launch; neither launch nor recovery reopens source.
func Capture(root, source, repoID, branch, head string) (_ *Snapshot, err error) {
	if !filepath.IsAbs(source) {
		return nil, fmt.Errorf("verification plan source must be absolute")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("read verification plan: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("verification plan must be a regular file")
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, fmt.Errorf("read verification plan: %w", err)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("verification plan must be a regular file")
	}
	const maxBytes = 64 * 1024
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read verification plan: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("verification plan exceeds maximum size of 64 KiB (65,536 bytes)")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("verification plan is empty")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("verification plan must be UTF-8 text")
	}
	id := ulid.Make().String()
	dir := filepath.Join(root, id)
	if err = os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	s := Snapshot{ID: id, Path: filepath.Join(dir, "verification-plan.txt"), SourcePath: source, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), CapturedAt: time.Now().Unix()}
	if err = os.WriteFile(s.Path, data, 0o400); err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(capture{Snapshot: s, RepoID: repoID, Branch: branch, HeadSHA: head})
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "capture.json"), manifest, 0o400); err != nil {
		return nil, err
	}
	return &s, nil
}

// Resolve accepts only an application-owned capture for this exact launch.
func Resolve(root, id, repoID, branch, head string) (*Snapshot, error) {
	if id == "" {
		return nil, nil
	}
	if _, err := ulid.ParseStrict(id); err != nil {
		return nil, fmt.Errorf("invalid verification plan capture ID")
	}
	dir := filepath.Join(root, id)
	b, err := os.ReadFile(filepath.Join(dir, "capture.json"))
	if err != nil {
		return nil, fmt.Errorf("read verification plan capture: %w", err)
	}
	var c capture
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode verification plan capture: %w", err)
	}
	if c.ID != id || c.Path != filepath.Join(dir, "verification-plan.txt") || c.RepoID != repoID || c.Branch != branch || c.HeadSHA != head {
		return nil, fmt.Errorf("verification plan capture does not match launch")
	}
	if _, err := c.Read(); err != nil {
		return nil, err
	}
	return &c.Snapshot, nil
}

// Read validates the pinned digest on every consumer, including recovery.
func (s *Snapshot) Read() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read captured verification plan: %w", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != s.SHA256 {
		return nil, fmt.Errorf("captured verification plan digest mismatch")
	}
	return b, nil
}

func (s Snapshot) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *Snapshot) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("invalid persisted verification plan")
	}
	return json.Unmarshal(b, s)
}

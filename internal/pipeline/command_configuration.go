package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

const commandConfigurationFile = "command-config.ndjson"

func recordCommandConfiguration(logDir, event string, cfg *config.Config) error {
	path := filepath.Join(logDir, commandConfigurationFile)
	if cfg == nil || len(cfg.CommandOverrides) == 0 {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect command configuration evidence: %w", err)
		}
	}
	// Resumes re-read configuration. Keep both snapshots, including removal of
	// a local override, so later execution never overwrites earlier evidence.
	payload, err := json.Marshal(struct {
		Version         int            `json:"version"`
		Event           string         `json:"event"`
		At              time.Time      `json:"at"`
		BuildVersion    string         `json:"build_version"`
		BuildCommit     string         `json:"build_commit"`
		EffectiveConfig *config.Config `json:"effective_config"`
	}{1, event, time.Now().UTC(), buildinfo.Version, buildinfo.Commit, cfg})
	if err != nil {
		return fmt.Errorf("serialize command configuration evidence: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open command configuration evidence: %w", err)
	}
	_, writeErr := file.Write(append(payload, '\n'))
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return fmt.Errorf("record command configuration evidence: %w", err)
	}
	return nil
}

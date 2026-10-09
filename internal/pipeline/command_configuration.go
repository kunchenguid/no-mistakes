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

// inheritedCommandEnvironmentKeys are the toolchain-path and parallelism
// variables configured commands inherit from the daemon's environment. Their
// values are recorded only in the private snapshot, never in logs or prompts.
var inheritedCommandEnvironmentKeys = []string{"PATH", "GOMAXPROCS", "MAKEFLAGS", "CARGO_BUILD_JOBS", "CMAKE_BUILD_PARALLEL_LEVEL"}

// inheritedCommandEnvironment omits an unset variable rather than recording an
// empty value it never had.
func inheritedCommandEnvironment() map[string]string {
	env := map[string]string{}
	for _, key := range inheritedCommandEnvironmentKeys {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}

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
		Version         int               `json:"version"`
		Event           string            `json:"event"`
		At              time.Time         `json:"at"`
		BuildVersion    string            `json:"build_version"`
		BuildCommit     string            `json:"build_commit"`
		EffectiveConfig *config.Config    `json:"effective_config"`
		InheritedEnv    map[string]string `json:"inherited_environment"`
	}{1, event, time.Now().UTC(), buildinfo.Version, buildinfo.Commit, cfg, inheritedCommandEnvironment()})
	if err != nil {
		return fmt.Errorf("serialize command configuration evidence: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open command configuration evidence: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict command configuration evidence permissions: %w", errors.Join(err, file.Close()))
	}
	_, writeErr := file.Write(append(payload, '\n'))
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return fmt.Errorf("record command configuration evidence: %w", err)
	}
	return nil
}

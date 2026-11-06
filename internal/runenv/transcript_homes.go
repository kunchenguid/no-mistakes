package runenv

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TranscriptHomes pins discovery roots, independently of agent authentication.
// Both roots are resolved by the caller, including defaults, so the daemon's
// environment cannot select a different account. Nil denotes a legacy launch.
type TranscriptHomes struct {
	ClaudeConfigDir string `json:"claude_config_dir"`
	CodexHome       string `json:"codex_home"`
}

func CaptureTranscriptHomes() *TranscriptHomes {
	home, _ := os.UserHomeDir()
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" && home != "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" && home != "" {
		codex = filepath.Join(home, ".codex")
	}
	return &TranscriptHomes{ClaudeConfigDir: claude, CodexHome: codex}
}

func (h *TranscriptHomes) ValidateClaude() error {
	if h == nil {
		return nil
	}
	return validateTranscriptHome("CLAUDE_CONFIG_DIR", h.ClaudeConfigDir)
}

func (h *TranscriptHomes) ValidateCodex() error {
	if h == nil {
		return nil
	}
	return validateTranscriptHome("CODEX_HOME", h.CodexHome)
}

func validateTranscriptHome(name, path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("%s transcript home must be an absolute path", name)
	}
	return nil
}

func (h TranscriptHomes) Value() (driver.Value, error) {
	b, err := json.Marshal(h)
	return string(b), err
}

func (h *TranscriptHomes) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("invalid persisted transcript homes")
	}
	if err := json.Unmarshal(b, h); err != nil {
		return fmt.Errorf("invalid persisted transcript homes")
	}
	return nil
}

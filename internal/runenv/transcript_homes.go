package runenv

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TranscriptHomes pins the omp discovery root independently of agent
// authentication. The caller resolves it, including the default, so the
// daemon's environment cannot select a different account. Nil denotes a legacy
// launch.
type TranscriptHomes struct {
	OMPHome string `json:"omp_home"`
}

func CaptureTranscriptHomes() *TranscriptHomes {
	home, _ := os.UserHomeDir()
	ompHome := os.Getenv("OMP_HOME")
	if ompHome == "" && home != "" {
		ompHome = filepath.Join(home, ".omp")
	}
	return &TranscriptHomes{OMPHome: ompHome}
}

func (h *TranscriptHomes) Validate() error {
	if h == nil {
		return nil
	}
	return validateTranscriptHome("OMP_HOME", h.OMPHome)
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

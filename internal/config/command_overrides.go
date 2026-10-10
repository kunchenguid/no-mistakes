package config

import (
	"fmt"
	"runtime"
	"strings"
)

// CommandOverride adds machine-local checks and lowers scheduling priority,
// never changing the repository's command string or its environment.
// Additional checks have separate exit statuses.
type CommandOverride struct {
	Nice       int      `yaml:"nice" json:"nice,omitempty"`
	Additional []string `yaml:"additional" json:"additional,omitempty"`
}

func validateCommandOverrides(overrides map[string]CommandOverride) error {
	for name, override := range overrides {
		switch name {
		case "test", "lint":
		case "prepare", "format":
			if len(override.Additional) != 0 {
				return fmt.Errorf("commands.%s.additional is only supported for test and lint checks", name)
			}
		default:
			return fmt.Errorf("unknown command %q (want prepare, test, lint, or format)", name)
		}
		if override.Nice < 0 || override.Nice > 19 {
			return fmt.Errorf("commands.%s.nice must be between 0 and 19", name)
		}
		if runtime.GOOS == "windows" && override.Nice != 0 {
			return fmt.Errorf("commands.%s.nice is not supported on Windows", name)
		}
		for _, command := range override.Additional {
			if strings.TrimSpace(command) == "" || strings.ContainsRune(command, '\x00') {
				return fmt.Errorf("commands.%s.additional checks must be nonempty shell commands without NUL", name)
			}
		}
	}
	return nil
}

func copyCommandOverrides(overrides map[string]CommandOverride) map[string]CommandOverride {
	if overrides == nil {
		return nil
	}
	result := make(map[string]CommandOverride, len(overrides))
	for name, override := range overrides {
		override.Additional = append([]string(nil), override.Additional...)
		result[name] = override
	}
	return result
}

package config

import (
	"fmt"
	"maps"
	"runtime"
	"strings"
)

// CommandOverride changes only the machine's execution context, never the
// repository's command string. Additional checks have separate exit statuses.
type CommandOverride struct {
	Env        map[string]string `yaml:"env" json:"env,omitempty"`
	Nice       int               `yaml:"nice" json:"nice,omitempty"`
	Additional []string          `yaml:"additional" json:"additional,omitempty"`
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
		keys := make(map[string]bool, len(override.Env))
		for key, value := range override.Env {
			if !validCommandEnvKey(key) || strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("commands.%s.env contains an invalid environment key or NUL value", name)
			}
			folded := strings.ToUpper(key)
			if keys[folded] {
				return fmt.Errorf("commands.%s.env contains case-ambiguous keys", name)
			}
			keys[folded] = true
		}
		for _, command := range override.Additional {
			if strings.TrimSpace(command) == "" || strings.ContainsRune(command, '\x00') {
				return fmt.Errorf("commands.%s.additional checks must be nonempty shell commands without NUL", name)
			}
		}
	}
	return nil
}

func validCommandEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func copyCommandOverrides(overrides map[string]CommandOverride) map[string]CommandOverride {
	if overrides == nil {
		return nil
	}
	result := make(map[string]CommandOverride, len(overrides))
	for name, override := range overrides {
		override.Env = maps.Clone(override.Env)
		override.Additional = append([]string(nil), override.Additional...)
		result[name] = override
	}
	return result
}

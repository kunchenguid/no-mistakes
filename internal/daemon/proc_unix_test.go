//go:build !windows

package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestParseProcessStartTimeUsesProvidedLocation(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)

	startedAt, err := parseProcessStartTime("Mon Jan 2 15:04:05 2006", loc)
	if err != nil {
		t.Fatalf("parseProcessStartTime returned error: %v", err)
	}

	if startedAt.Location() != loc {
		t.Fatalf("location = %v, want %v", startedAt.Location(), loc)
	}
	if startedAt.UTC() != time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC) {
		t.Fatalf("utc time = %v, want %v", startedAt.UTC(), time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC))
	}
}

func TestProcessStartTimeCommandForcesCLocale(t *testing.T) {
	cmd := processStartTimeCommand(123)

	if got := strings.Join(cmd.Args, " "); got != "ps -p 123 -o lstart=" {
		t.Fatalf("args = %q, want %q", got, "ps -p 123 -o lstart=")
	}
	if !containsEnvEntry(cmd.Env, "LC_ALL=C") {
		t.Fatalf("expected LC_ALL=C in env, got %v", cmd.Env)
	}
	if !containsEnvEntry(cmd.Env, "LANG=C") {
		t.Fatalf("expected LANG=C in env, got %v", cmd.Env)
	}
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

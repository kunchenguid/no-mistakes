//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// The E2E suite models standalone no-mistakes unless an individual test
	// explicitly supplies managed authorization context. Do not let the
	// orchestrator running `go test` change the child binaries' mode.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PERCH_") || strings.HasPrefix(key, "NO_MISTAKES_AUTHORIZATION_") {
			_ = os.Unsetenv(key)
		}
	}
	os.Exit(m.Run())
}

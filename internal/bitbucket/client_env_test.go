package bitbucket

import (
	"strings"
	"testing"
)

func TestNewClientFromEnvDoesNotReadAmbientEnvironment(t *testing.T) {
	t.Setenv(envEmail, "ambient@example.com")
	t.Setenv(envToken, "ambient-token")
	t.Setenv(envAPIBaseURL, "https://ambient.example")

	_, err := NewClientFromEnv(nil)
	if err == nil {
		t.Fatal("expected missing credentials error")
	}
	if !strings.Contains(err.Error(), envEmail) {
		t.Fatalf("error = %q, want missing %s", err, envEmail)
	}
}

package cli

import (
	"strings"
	"testing"
)

func TestEjectNotInitialized(t *testing.T) {
	setupTestRepo(t)

	_, err := executeCmd("eject")
	if err == nil {
		t.Fatal("eject should fail when not initialized")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

//go:build windows

package daemon

import "testing"

func TestProcessRunningReturnsFalseForMissingPID(t *testing.T) {
	running, err := processRunning(999999)
	if err != nil {
		t.Fatalf("processRunning returned error: %v", err)
	}
	if running {
		t.Fatal("expected missing pid to be reported as not running")
	}
}

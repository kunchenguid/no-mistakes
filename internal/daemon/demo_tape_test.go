package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoTapeForcesDetachedDaemon(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "demo.tape"))
	if err != nil {
		t.Fatalf("read demo.tape: %v", err)
	}
	if !strings.Contains(string(data), `Env NM_TEST_START_DAEMON "1"`) {
		t.Fatal(`demo.tape must force detached daemon startup for demo mode`)
	}
}

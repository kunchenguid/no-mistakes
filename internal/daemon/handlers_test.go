package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestHealthHandler(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Errorf("health status = %q, want %q", result.Status, "ok")
	}
}

func TestShutdownHandler(t *testing.T) {
	p, _ := startTestDaemon(t)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Error("shutdown result should be OK")
	}

	// Wait for socket to disappear.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.Socket()); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("socket still exists after shutdown")
}

func TestPIDFile(t *testing.T) {
	p, _ := startTestDaemon(t)

	pid, err := ReadPID(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

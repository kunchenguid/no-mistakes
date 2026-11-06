//go:build !windows

package callerrestrictions

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCallerRestrictionsCaptureRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "caller.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Capture(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO error = %v; want regular-file rejection", err)
		}
	case <-time.After(5 * time.Second):
		// Release a regressed reader without leaving a blocked goroutine.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
			<-done
		}
		t.Fatal("caller restrictions capture waited for a FIFO writer")
	}
}

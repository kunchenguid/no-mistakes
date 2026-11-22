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

func TestCallerRestrictionsCaptureFollowsRegularFileSymlink(t *testing.T) {
	dir := t.TempDir()
	source, link := filepath.Join(dir, "caller.txt"), filepath.Join(dir, "caller-link.txt")
	const content = "Never install dependencies.\n"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	r, err := Capture(link)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != content || r.SourcePath != link {
		t.Fatalf("symlink capture lost bytes or caller path: %+v", r)
	}
	if err := os.WriteFile(source, []byte("changed instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if r.Content != content {
		t.Fatal("symlink target mutation changed captured restrictions")
	}
}

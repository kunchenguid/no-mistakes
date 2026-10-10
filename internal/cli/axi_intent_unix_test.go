//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAxiRunIntentRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "intent.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "intent-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cmd := newAxiRunCmd()
			if err := cmd.ParseFlags([]string{"--intent-file", path}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := resolveAxiRunIntent(cmd, "", path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("FIFO error = %v; want regular-file rejection", err)
				}
			case <-time.After(5 * time.Second):
				// Release a regressed blocking reader without leaving a goroutine
				// behind; the timeout is a test watchdog, not an input deadline.
				f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err == nil {
					f.Close()
					<-done
				}
				t.Fatal("intent reader waited for a FIFO writer")
			}
		})
	}
}

func TestAxiRunIntentAcceptsSymlinkToRegularFile(t *testing.T) {
	text := "  valid intent 雪\n"
	link := filepath.Join(t.TempDir(), "intent-link")
	if err := os.Symlink(writeIntentFile(t, text), link); err != nil {
		t.Fatal(err)
	}
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--intent-file", link}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAxiRunIntent(cmd, "", link)
	if err != nil || got != text {
		t.Fatalf("resolved = %q, error = %v; want %q", got, err, text)
	}
}

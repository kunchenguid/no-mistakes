//go:build unit

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCLILogWriterReturnsDiscardWhenLogsDirMissing(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(nmHome, "logs", "cli.log")); !os.IsNotExist(err) {
		t.Fatalf("cli.log should not be created when logs dir is missing, stat err = %v", err)
	}

	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}

func TestCLILogWriterAppendsToFileWhenLogsDirExists(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	logsDir := filepath.Join(nmHome, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if c, ok := w.(io.Closer); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	}

	b, err := os.ReadFile(filepath.Join(logsDir, "cli.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("cli.log contents = %q, want %q", string(b), "hello\n")
	}
}

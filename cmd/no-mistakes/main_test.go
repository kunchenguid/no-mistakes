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

func TestDaemonRunRootFromArgs(t *testing.T) {
	t.Setenv("NM_DAEMON", "")

	tests := []struct {
		name     string
		args     []string
		wantRoot string
		wantOK   bool
		wantErr  string
	}{
		{name: "non-daemon command", args: []string{"daemon", "status"}},
		{name: "daemon run no root", args: []string{"daemon", "run"}, wantOK: true},
		{name: "daemon run root flag", args: []string{"daemon", "run", "--root", "/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run root equals", args: []string{"daemon", "run", "--root=/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run missing root value", args: []string{"daemon", "run", "--root"}, wantErr: "missing value for --root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRoot, gotOK, err := daemonRunRootFromArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotRoot != tt.wantRoot || gotOK != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, tt.wantRoot, tt.wantOK)
			}
		})
	}
}

func TestDaemonRunRootFromArgs_EnvForcesDaemonMode(t *testing.T) {
	t.Setenv("NM_DAEMON", "1")

	gotRoot, gotOK, err := daemonRunRootFromArgs([]string{"status"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotRoot != "" || !gotOK {
		t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, "", true)
	}
}

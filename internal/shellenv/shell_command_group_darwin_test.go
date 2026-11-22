package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestDarwinGroupInspectionNoMatchesRequiresAbsentGroup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name   string
		group  int
		output string
		code   int
		wantOK bool
	}{
		{"live group", syscall.Getpgrp(), "", 1, false},
		{"absent group", exitedGroup(t), "", 1, true},
		{"partial output", exitedGroup(t), "unreadable", 1, false},
		{"inspection failure", exitedGroup(t), "", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\nprintf '%s' '" + tc.output + "'\nexit " + strconv.Itoa(tc.code) + "\n"
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := shellCommandGroupPS(context.Background(), tc.group)
			if (err == nil) != tc.wantOK {
				t.Fatalf("inspection err=%v, want success=%t", err, tc.wantOK)
			}
		})
	}
}

func exitedGroup(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("fixture group still exists: %v", err)
	}
	return pgid
}

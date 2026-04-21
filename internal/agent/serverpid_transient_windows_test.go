//go:build windows

package agent

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

func TestIsTransientPIDOpenError_Windows(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "sharing_violation", err: &fs.PathError{Op: "open", Path: "x", Err: syscall.Errno(32)}, want: true},
		{name: "access_denied", err: &fs.PathError{Op: "open", Path: "x", Err: syscall.Errno(5)}, want: false},
		{name: "not_exist", err: os.ErrNotExist, want: false},
		{name: "other_errno", err: &fs.PathError{Op: "open", Path: "x", Err: syscall.Errno(2)}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientPIDOpenError(tc.err); got != tc.want {
				t.Fatalf("isTransientPIDOpenError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestReplaceServerPIDFile_WindowsRetriesTransientRenameError(t *testing.T) {
	prevRename := renameServerPIDFile
	prevSleep := sleepServerPIDRenameRetry
	t.Cleanup(func() {
		renameServerPIDFile = prevRename
		sleepServerPIDRenameRetry = prevSleep
	})

	var calls int
	renameServerPIDFile = func(oldpath, newpath string) error {
		calls++
		if calls < 3 {
			return &fs.PathError{Op: "rename", Path: newpath, Err: syscall.Errno(5)}
		}
		return nil
	}
	sleepServerPIDRenameRetry = func() {}

	if err := replaceServerPIDFile("tmp.json", "dst.json"); err != nil {
		t.Fatalf("replaceServerPIDFile() error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("replaceServerPIDFile() calls = %d, want 3", calls)
	}
}

func TestReplaceServerPIDFile_WindowsStopsOnPermanentRenameError(t *testing.T) {
	prevRename := renameServerPIDFile
	prevSleep := sleepServerPIDRenameRetry
	t.Cleanup(func() {
		renameServerPIDFile = prevRename
		sleepServerPIDRenameRetry = prevSleep
	})

	wantErr := &fs.PathError{Op: "rename", Path: "dst.json", Err: errors.New("boom")}
	renameServerPIDFile = func(oldpath, newpath string) error { return wantErr }
	sleepServerPIDRenameRetry = func() {}

	err := replaceServerPIDFile("tmp.json", "dst.json")
	if !errors.Is(err, wantErr) {
		t.Fatalf("replaceServerPIDFile() error = %v, want %v", err, wantErr)
	}
}

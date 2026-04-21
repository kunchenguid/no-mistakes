//go:build windows

package agent

import (
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
		{name: "access_denied", err: &fs.PathError{Op: "open", Path: "x", Err: syscall.Errno(5)}, want: true},
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

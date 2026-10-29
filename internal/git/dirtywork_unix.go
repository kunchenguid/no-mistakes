//go:build unix

package git

import (
	"os"
	"syscall"
)

func openRegular(full string) (*os.File, error) {
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

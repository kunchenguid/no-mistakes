//go:build !unix

package git

import "os"

func openRegular(full string) (*os.File, error) {
	f, err := os.Open(full)
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
	return f, nil
}

package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func RemoveAll(ctx context.Context, path string) error {
	return removeAll(ctx, path, nil)
}

func RemoveAllIfSame(ctx context.Context, path string, expected os.FileInfo) error {
	if expected == nil {
		return fs.ErrInvalid
	}
	return removeAll(ctx, path, expected)
}

func removeAll(ctx context.Context, path string, expected os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		return &os.PathError{Op: "removeall", Path: path, Err: fs.ErrInvalid}
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeEntry(ctx, parent, name, expected)
}

func removeEntry(ctx context.Context, parent *os.Root, name string, expected os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected != nil && !os.SameFile(info, expected) {
		return fmt.Errorf("directory identity changed: %s: %w", name, fs.ErrInvalid)
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		child, err := parent.OpenRoot(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		opened, err := child.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = child.Close()
			if err != nil {
				return err
			}
			return fmt.Errorf("directory changed during cleanup: %s: %w", name, fs.ErrInvalid)
		}
		err = removeContents(ctx, child)
		closeErr := child.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("entry changed during cleanup: %s: %w", name, fs.ErrInvalid)
	}
	err = parent.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removeContents(ctx context.Context, root *os.Root) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := removeEntry(ctx, root, entry.Name(), nil); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}

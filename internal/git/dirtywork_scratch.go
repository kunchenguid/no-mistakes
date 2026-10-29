package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/fsutil"
)

type dirtyWorkScratch struct {
	path string
	root *os.Root
}

var inactiveDirtyWorkScratch struct {
	sync.Mutex
	dirs []*dirtyWorkScratch
}

func newDirtyWorkScratch(ctx context.Context) (*dirtyWorkScratch, error) {
	_ = ReapDirtyWorkScratch(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp("", "nm-dirty-work-")
	if err != nil {
		return nil, err
	}
	// Open through the parent so the identity handle permits rename/removal
	// on Windows while remaining open throughout ownership verification.
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		_ = fsutil.RemoveAll(ctx, path)
		return nil, err
	}
	root, err := parent.OpenRoot(filepath.Base(path))
	_ = parent.Close()
	if err != nil {
		_ = fsutil.RemoveAll(ctx, path)
		return nil, err
	}
	return &dirtyWorkScratch{path: path, root: root}, nil
}

func (s *dirtyWorkScratch) close(ctx context.Context) {
	inactiveDirtyWorkScratch.Lock()
	inactiveDirtyWorkScratch.dirs = append(inactiveDirtyWorkScratch.dirs, s)
	inactiveDirtyWorkScratch.Unlock()
	_ = ReapDirtyWorkScratch(ctx)
}

func ReapDirtyWorkScratch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inactiveDirtyWorkScratch.Lock()
	dirs := inactiveDirtyWorkScratch.dirs
	inactiveDirtyWorkScratch.dirs = nil
	inactiveDirtyWorkScratch.Unlock()
	var retained []*dirtyWorkScratch
	defer func() {
		inactiveDirtyWorkScratch.Lock()
		inactiveDirtyWorkScratch.dirs = append(inactiveDirtyWorkScratch.dirs, retained...)
		inactiveDirtyWorkScratch.Unlock()
	}()
	var result error
	for i, s := range dirs {
		if err := ctx.Err(); err != nil {
			retained = append(retained, dirs[i:]...)
			return errors.Join(result, err)
		}
		info, err := s.root.Stat(".")
		if err == nil {
			err = fsutil.RemoveAllIfSame(ctx, s.path, info)
		}
		if err == nil || errors.Is(err, fs.ErrInvalid) || errors.Is(err, os.ErrNotExist) {
			_ = s.root.Close()
		} else {
			retained = append(retained, s)
		}
		result = errors.Join(result, err)
	}
	return result
}

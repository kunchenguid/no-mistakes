package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func lockCorpus(ctx context.Context, root string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(root, "corpus.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open eval corpus lock: %w", err)
	}
	for {
		locked, err := tryLockCorpusFile(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock eval corpus: %w", err)
		}
		if locked {
			return func() {
				_ = unlockCorpusFile(file)
				_ = file.Close()
			}, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

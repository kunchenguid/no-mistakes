package quotarouting

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// maxSnapshotBytes bounds the snapshot read. A quota-axi --json document is a
// few kilobytes; the bound exists so a wedged or broken executable cannot turn
// one run setup into a memory event, not to constrain real output.
const maxSnapshotBytes = 4 << 20

// ReadSnapshot runs the quota-axi executable once (--json) and parses its
// output. It is the one place no-mistakes invokes quota-axi, called once per
// run at run setup. Every failure mode - missing executable, timeout, non-zero
// exit, unparseable output - is an error the caller resolves to today's
// configured order with a recorded reason; it never blocks a run.
func ReadSnapshot(ctx context.Context, exe string, timeout time.Duration) (*Snapshot, error) {
	if exe == "" {
		exe = "quota-axi"
	}
	if timeout <= 0 {
		timeout = DefaultReadTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := snapshotCommand(readCtx, exe)
	shellenv.ConfigureShellCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &boundedBuffer{limit: maxSnapshotBytes, inner: &stdout}
	cmd.Stderr = &stderr
	if err := shellenv.RunShellCommand(cmd); err != nil {
		if readCtx.Err() != nil {
			return nil, fmt.Errorf("quota-axi did not finish within %s", timeout)
		}
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return nil, fmt.Errorf("quota-axi failed: %s", fallbackReason(reason))
	}
	snapshot, err := ParseSnapshot(stdout.Bytes())
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// boundedBuffer caps how much of a child's stdout is kept, so a runaway
// executable cannot exhaust memory before its timeout lands.
type boundedBuffer struct {
	inner *bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.inner.Len() >= b.limit {
		return len(p), nil // discard beyond the bound; parse will refuse the truncated document
	}
	room := b.limit - b.inner.Len()
	if room < len(p) {
		b.inner.Write(p[:room])
		return len(p), nil
	}
	return b.inner.Write(p)
}

func fallbackReason(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const max = 160
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "..."
}

// DefaultReadTimeout bounds one quota-axi read. quota-axi may delegate an
// expired session's renewal to the vendor CLI that owns it, so the default is
// generous but still bounded: a slow read must never delay a run's start by
// more than this.
const DefaultReadTimeout = 15 * time.Second

package quota

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// DefaultAXIPath is the quota-axi binary resolved through PATH when the operator
// configures no other path.
const DefaultAXIPath = "quota-axi"

// DefaultReadTimeout bounds one quota-axi invocation. A selection point must not
// hold the pipeline hostage to a wedged vendor read: the timeout is the
// difference between "no evidence" (which the caller refuses to guess around)
// and a run that never starts.
const DefaultReadTimeout = 45 * time.Second

// maxReadErrorBytes bounds the tool's stderr echoed into an error, so a noisy
// failure cannot flood a run record or a log line.
const maxReadErrorBytes = 512

// Reader produces one routing report. It is an interface so the gates can be
// tested against recorded evidence rather than a live vendor read, and so a
// caller can substitute a cached report when it wants one.
type Reader interface {
	Read(ctx context.Context) (Report, error)
}

// AXIReader reads routing evidence by running quota-axi.
//
// It is deliberately a read-only caller: `models --json` is the one surface that
// joins the tool's model catalog to the provider's live quota evidence, so one
// invocation answers all three gates. The tool's own caching applies (including
// the operator's QUOTA_AXI_MAX_AGE): no-mistakes never re-reads on its own
// schedule, it asks for evidence each time a selection is made.
type AXIReader struct {
	// Path is the quota-axi binary or an absolute path to it. Empty means
	// DefaultAXIPath.
	Path    string
	Timeout time.Duration
	// run is the process seam: it exists so the reader's argument shape, output
	// parsing and error mapping can be tested without a vendor tool installed.
	// Production always leaves it nil and spawns the binary.
	run func(ctx context.Context, binary string, args ...string) ([]byte, []byte, error)
}

// NewAXIReader returns a reader over the configured binary path.
func NewAXIReader(path string) *AXIReader { return &AXIReader{Path: path} }

// Read runs one `quota-axi models --json` and parses its report.
func (r *AXIReader) Read(ctx context.Context) (Report, error) {
	binary := strings.TrimSpace(r.Path)
	if binary == "" {
		binary = DefaultAXIPath
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultReadTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, err := r.exec(readCtx, binary, "models", "--json")
	if err != nil {
		return Report{}, fmt.Errorf("read quota evidence from %s: %w%s", binary, err, readErrorDetail(stderr))
	}
	report, err := ParseReport(stdout)
	if err != nil {
		return Report{}, fmt.Errorf("read quota evidence from %s: %w", binary, err)
	}
	return report, nil
}

// exec runs the tool, or the test seam's stand-in for it.
func (r *AXIReader) exec(ctx context.Context, binary string, args ...string) ([]byte, []byte, error) {
	if r.run != nil {
		return r.run(ctx, binary, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	winproc.Harden(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// readErrorDetail renders the tool's own diagnostic, bounded and single-line, so
// a failure explains itself without the caller having to re-run the command.
func readErrorDetail(stderr []byte) string {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		return ""
	}
	if idx := strings.IndexAny(detail, "\r\n"); idx >= 0 {
		detail = detail[:idx]
	}
	if len(detail) > maxReadErrorBytes {
		detail = detail[:maxReadErrorBytes] + "..."
	}
	return ": " + detail
}

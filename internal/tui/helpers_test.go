package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

func ptr[T any](v T) *T { return &v }

func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tui-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func startTestIPCServer(t *testing.T, sock string) *ipc.Server {
	t.Helper()
	srv := ipc.NewServer()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		srv.Close()
		<-errCh
	})
	return srv
}

func testRun() *ipc.RunInfo {
	return &ipc.RunInfo{
		ID:      "run-001",
		RepoID:  "repo-001",
		Branch:  "feature/foo",
		HeadSHA: "abc12345def67890",
		BaseSHA: "000000000000",
		Status:  types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusPending},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
			{ID: "s3", StepName: types.StepLint, StepOrder: 3, Status: types.StepStatusPending},
			{ID: "s4", StepName: types.StepPush, StepOrder: 4, Status: types.StepStatusPending},
			{ID: "s5", StepName: types.StepPR, StepOrder: 5, Status: types.StepStatusPending},
		},
	}
}
func testRunWithCI() *ipc.RunInfo {
	run := testRun()
	run.Steps = append(run.Steps, ipc.StepResultInfo{
		ID: "s6", StepName: types.StepCI, StepOrder: 6, Status: types.StepStatusPending,
	})
	return run
}
func makeManyFindings(n int) string {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"id":"f%d","severity":"warning","file":"file%d.go","line":%d,"description":"finding %d description"}`, i, i, i, i))
	}
	return fmt.Sprintf(`{"summary":"test summary","items":[%s]}`, strings.Join(items, ","))
}
func boxContentLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "│") && strings.HasSuffix(trimmed, "│") {
		inner := trimmed[len("│") : len(trimmed)-len("│")]
		return strings.TrimSpace(inner)
	}
	return ""
}
func visualColumn(line, needle string) int {
	idx := strings.Index(line, needle)
	if idx < 0 {
		return -1
	}
	return lipgloss.Width(line[:idx])
}

func hasLineContainingAll(view string, needles ...string) bool {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		match := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func hasParallelBoxRow(view string) bool {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Count(line, "╭") >= 2 || strings.Count(line, "╯") >= 2 || strings.Count(line, "│") >= 4 {
			return true
		}
	}
	return false
}

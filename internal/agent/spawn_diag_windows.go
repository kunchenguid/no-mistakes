//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// processImageName returns the executable image name for pid via tasklist, so a
// diagnostic run can reveal when the tracked process is the cmd.exe .cmd-shim
// wrapper rather than the native agent (issue #427). Returns "gone" when no
// process matches (it already exited), or an error marker on failure.
func processImageName(pid int) string {
	if pid <= 0 {
		return ""
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "unknown(" + err.Error() + ")"
	}
	line := strings.TrimSpace(string(out))
	// tasklist prints an "INFO: No tasks..." line to stdout when nothing matches.
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return "gone"
	}
	// CSV row: "Image Name","PID","Session Name",...  Take the first field.
	if strings.HasPrefix(line, "\"") {
		if end := strings.IndexByte(line[1:], '"'); end >= 0 {
			return line[1 : 1+end]
		}
	}
	return line
}

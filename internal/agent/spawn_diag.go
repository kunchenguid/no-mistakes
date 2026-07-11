package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// spawnDiagEnv gates verbose agent-spawn diagnostics via an environment
// variable. Set NM_SPAWN_DIAG=1 in the daemon's environment to trace a failing
// agent invocation on Windows/Cygwin (issue #427): the resolved binary, the
// tracked PID and its OS image name (to expose a .cmd wrapper being tracked
// instead of the native process), stdout volume, raw stderr, and the exact exit
// path. Only the exact value 1 enables it; any other value (including 0 or
// false) and unset leave it disabled. With diagnostics off and no sentinel
// present, spawnDiag is a no-op with zero overhead.
//
// WARNING: when enabled, the diagnostic writes the agent's raw stdout head and
// its full stderr to the daemon log verbatim. The argv prompt and JSON schema
// are redacted, but the captured streams are not: stdout reflects the model's
// response (and may carry the structured-output JSON) and stderr can carry
// secrets. Enable this only for a controlled, short-lived debug session, then
// unset the variable or remove the sentinel. Do not leave it on in a shared or
// long-running deployment.
const spawnDiagEnv = "NM_SPAWN_DIAG"

// spawnDiagSentinel is a file under NM_HOME that also enables diagnostics. The
// managed daemon runs with a curated environment, so an env var set in a shell
// does not reach it; dropping this file lets you toggle diagnostics against the
// running service without changing its env or restarting it. Create
// <NM_HOME>/spawn-diag, reproduce the failure, then remove it.
const spawnDiagSentinel = "spawn-diag"

// spawnDiagEnabled reports whether to emit diagnostics for this invocation,
// via either the environment variable or the sentinel file. The sentinel is
// stat-checked once per spawn, which is negligible against launching a process.
func spawnDiagEnabled() bool {
	if os.Getenv(spawnDiagEnv) == "1" {
		return true
	}
	if root := diagHome(); root != "" {
		if _, err := os.Stat(filepath.Join(root, spawnDiagSentinel)); err == nil {
			return true
		}
	}
	return false
}

// diagHome mirrors paths.New: NM_HOME, or ~/.no-mistakes. Kept inline so the
// diagnostic stays a leaf with no dependency on the paths package.
func diagHome() string {
	if env := os.Getenv("NM_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".no-mistakes")
}

// spawnDiag records diagnostics for one native agent invocation. The zero value
// is a valid disabled instance; every method is safe to call on it.
type spawnDiag struct {
	enabled bool
	agent   string
	onChunk func(string)
	start   time.Time
	capture *diagCapture
}

// newSpawnDiag returns an enabled diagnostic only when NM_SPAWN_DIAG is set,
// and logs the resolved binary and (redacted) argv up front. bin is the
// configured binary name; args is the full argv passed to it.
func newSpawnDiag(agentName, bin string, args []string, opts RunOpts) *spawnDiag {
	if !spawnDiagEnabled() {
		return &spawnDiag{}
	}
	d := &spawnDiag{
		enabled: true,
		agent:   agentName,
		onChunk: opts.OnChunk,
		start:   time.Now(),
		capture: &diagCapture{limit: 8192},
	}
	resolved, lookErr := exec.LookPath(bin)
	d.emit(fmt.Sprintf("bin=%q resolved=%q lookErr=%s argv=[%s]", bin, resolved, errStr(lookErr), redactArgv(args)))
	return d
}

func (d *spawnDiag) logStarted(pid int) {
	if !d.enabled {
		return
	}
	d.emit(fmt.Sprintf("started pid=%d image=%q", pid, processImageName(pid)))
}

func (d *spawnDiag) logStartError(err error) {
	if !d.enabled {
		return
	}
	d.emit("start-error: " + errStr(err))
}

// wrapStdout tees the agent's stdout through a capped capture so the exit log
// can report how much was produced and show a leading snippet. When disabled it
// returns the reader unchanged.
func (d *spawnDiag) wrapStdout(r io.Reader) io.Reader {
	if !d.enabled {
		return r
	}
	return io.TeeReader(r, d.capture)
}

// logExit records how the invocation ended: which branch (ok, no-result-event,
// parse-error, wait-error), the error, whether a result was parsed, stdout
// volume and head, and full stderr. It does not report the process image name:
// by the time any exit branch runs the child has already been reaped (every
// path follows started.wait or waitAfterParseError), so a lookup would report
// "gone" or, worse, a reused PID. The live image is captured by logStarted.
//
// The stdout head and stderr are logged verbatim; see the spawnDiagEnv warning
// about enabling this only for a controlled debug session.
func (d *spawnDiag) logExit(pid int, path string, pathErr error, gotResult bool, stderr []byte) {
	if !d.enabled {
		return
	}
	d.emit(fmt.Sprintf("exit path=%s pid=%d dur=%s err=%s gotResult=%t stdoutBytes=%d",
		path, pid, time.Since(d.start).Round(time.Millisecond), errStr(pathErr), gotResult, d.capture.n))
	d.emit(fmt.Sprintf("stdout-head=%q", d.capture.head()))
	d.emit(fmt.Sprintf("stderr=%q", strings.TrimSpace(string(stderr))))
}

func (d *spawnDiag) emit(msg string) {
	line := "spawn-diag[" + d.agent + "] " + msg
	if d.onChunk != nil {
		d.onChunk(line)
	}
	slog.Warn(line)
}

func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// redactArgv renders argv for logging while replacing the prompt and schema
// values (which follow -p and --json-schema) with their lengths, so a
// diagnostic run never dumps prompt content or the schema into logs.
func redactArgv(args []string) string {
	parts := make([]string, 0, len(args))
	redactNext := false
	for _, arg := range args {
		if redactNext {
			parts = append(parts, fmt.Sprintf("<redacted len=%d>", len(arg)))
			redactNext = false
			continue
		}
		parts = append(parts, arg)
		if arg == "-p" || arg == "--json-schema" {
			redactNext = true
		}
	}
	return strings.Join(parts, " ")
}

// diagCapture counts every byte written and retains the first limit bytes.
type diagCapture struct {
	n     int64
	buf   []byte
	limit int
}

func (c *diagCapture) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if len(c.buf) < c.limit {
		room := c.limit - len(c.buf)
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *diagCapture) head() string {
	if c == nil {
		return ""
	}
	return string(c.buf)
}

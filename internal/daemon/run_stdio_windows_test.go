package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"golang.org/x/sys/windows"
)

// Issue #1653: below build 26100 the manifest is ignored and Task Scheduler's
// start gives the daemon a console of its own, which daemon run releases. A
// console shared with a terminal (foreground `daemon run`) must stay, and
// with no console there is nothing to release.
func TestReleaseSoleConsoleFreesOnlyAConsoleOfItsOwn(t *testing.T) {
	originalCount, originalFree := consoleProcessCount, freeConsole
	t.Cleanup(func() { consoleProcessCount, freeConsole = originalCount, originalFree })

	for _, tc := range []struct {
		name         string
		attached     uint32
		freeErr      error
		wantFree     bool
		wantReleased bool
	}{
		{name: "no console", attached: 0},
		{name: "console of its own", attached: 1, wantFree: true, wantReleased: true},
		{name: "console shared with a terminal", attached: 2},
		{name: "release fails", attached: 1, freeErr: errors.New("access denied"), wantFree: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freed := false
			consoleProcessCount = func() uint32 { return tc.attached }
			freeConsole = func() error {
				freed = true
				return tc.freeErr
			}
			if got := releaseSoleConsole(); got != tc.wantReleased {
				t.Fatalf("released = %v, want %v", got, tc.wantReleased)
			}
			if freed != tc.wantFree {
				t.Fatalf("FreeConsole called = %v, want %v", freed, tc.wantFree)
			}
		})
	}
}

// With the manifest, Task Scheduler starts the daemon without a console and
// with NULL standard handles, so anything written before bootstrap capture -
// and the log-sink's inherited stderr - would otherwise be lost.
func TestPrepareRunStdioSendsNullStdHandlesToTheBootstrapLog(t *testing.T) {
	root := t.TempDir()
	runRunStdioHelper(t, root, windows.DETACHED_PROCESS, &windows.StartupInfo{Flags: windows.STARTF_USESTDHANDLES})
	assertBootstrapLogHasRunStdioMarkers(t, root)
}

// Without the manifest the daemon's console of its own is released, and the
// output that pointed at it must follow to the bootstrap log.
func TestPrepareRunStdioReleasesAConsoleOfItsOwnIntoTheBootstrapLog(t *testing.T) {
	root := t.TempDir()
	runRunStdioHelper(t, root, windows.CREATE_NEW_CONSOLE, &windows.StartupInfo{
		Flags:      windows.STARTF_USESHOWWINDOW,
		ShowWindow: windows.SW_HIDE,
	})
	assertBootstrapLogHasRunStdioMarkers(t, root)
}

// runRunStdioHelper starts the run-stdio helper with CreateProcess directly,
// because os/exec always hands a child valid standard handles.
func runRunStdioHelper(t *testing.T, root string, flags uint32, si *windows.StartupInfo) {
	t.Helper()
	t.Setenv("NM_HOME", root)
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "run-stdio")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{exe}))
	if err != nil {
		t.Fatal(err)
	}
	si.Cb = uint32(unsafe.Sizeof(*si))
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, commandLine, nil, nil, false, flags, nil, nil, si, &pi); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	if event, err := windows.WaitForSingleObject(pi.Process, 60_000); event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(pi.Process, 1)
		t.Fatalf("helper did not exit: event %#x, %v", event, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("helper exit code = %d, want 0", code)
	}
}

func assertBootstrapLogHasRunStdioMarkers(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(paths.WithRoot(root).DaemonBootstrapLog())
	if err != nil {
		t.Fatalf("read bootstrap log: %v", err)
	}
	for _, marker := range []string{"stdout-marker\n", "stderr-marker\n", "runtime-stderr-marker\n", "child-stderr-marker\n"} {
		if !strings.Contains(string(data), marker) {
			t.Fatalf("bootstrap log = %q, want %q", data, marker)
		}
	}
}

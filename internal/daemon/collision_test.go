package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestSplitCommandLineTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "plain", in: `/x/no-mistakes daemon run --root /a/b`, want: []string{"/x/no-mistakes", "daemon", "run", "--root", "/a/b"}},
		{name: "equals form", in: `/x/no-mistakes daemon run --root=/a/b`, want: []string{"/x/no-mistakes", "daemon", "run", "--root=/a/b"}},
		{name: "quoted spaced root", in: `"/x no-mistakes" daemon run --root "/a b/c"`, want: []string{"/x no-mistakes", "daemon", "run", "--root", "/a b/c"}},
		{name: "escaped quote", in: `daemon run --root a\"b`, want: []string{"daemon", "run", "--root", `a"b`}},
		{name: "empty", in: ``, want: []string(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCommandLineTokens(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitCommandLineTokens(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitCommandLineTokens(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLooksLikeDaemonRunCommand(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{`/x/no-mistakes daemon run --root /a`, true},
		{`/x/no-mistakes run daemon --root /a`, true}, // order-insensitive
		{`/x/no-mistakes`, false},                     // detached: bare exe
		{`postgres --root /a daemon`, false},          // missing "run"
		{`no-mistakes daemon --root /a`, false},       // missing "run"
		{``, false},
	}
	for _, tc := range tests {
		if got := looksLikeDaemonRunCommand(tc.in); got != tc.want {
			t.Errorf("looksLikeDaemonRunCommand(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestExtractRootFromCommand(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOk bool
	}{
		{`daemon run --root /a/b`, "/a/b", true},
		{`daemon run --root=/a/b`, "/a/b", true},
		{`daemon run --root "/a b/c"`, "/a b/c", true},
		{`daemon run`, "", false},
		{`daemon run --root`, "", false}, // flag with no value
	}
	for _, tc := range tests {
		got, ok := extractRootFromCommand(tc.in)
		if got != tc.want || ok != tc.wantOk {
			t.Errorf("extractRootFromCommand(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

func TestParseDaemonProcessOutput(t *testing.T) {
	output := strings.Join([]string{
		"1234 /home/u/no-mistakes daemon run --root /home/u/.no-mistakes",
		"  5678 \t/usr/bin/no-mistakes daemon run --root=/tmp/link",
		`9 "/x no-mistakes" daemon run --root "/a b/c"`,
		`10 /x/no-mistakes`,             // detached daemon, must be skipped
		`11 other --root /x daemon run`, // recovered but still daemon-run; root=/x
		`12 postgres daemon`,            // not daemon-run
		`bogus not a pid line`,          // unparseable pid
		``,                              // blank
	}, "\n")
	splitter := func(line string) (int, string, bool) {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			return 0, "", false
		}
		sep := strings.IndexAny(line, " \t")
		if sep < 0 {
			return 0, "", false
		}
		pid, err := strconv.Atoi(line[:sep])
		if err != nil || pid <= 0 {
			return 0, "", false
		}
		return pid, strings.TrimLeft(line[sep:], " \t"), true
	}
	got := parseDaemonProcessOutput(output, splitter)
	wantRoots := map[int]string{
		1234: "/home/u/.no-mistakes",
		5678: "/tmp/link",
		9:    "/a b/c",
		11:   "/x",
	}
	if len(got) != len(wantRoots) {
		t.Fatalf("parseDaemonProcessOutput returned %d entries %v, want %d", len(got), got, len(wantRoots))
	}
	for _, info := range got {
		if want, ok := wantRoots[info.PID]; !ok || want != info.Root {
			t.Errorf("entry %+v mismatch, want root %q", info, want)
		}
	}
}

// --- reconcileCollidingDaemons ---

// stubCollisionVars replaces the process enumeration, health check, and kill
// seams with closures backed by the caller's maps. It returns a cleanup that
// restores the originals. It deliberately does NOT touch serviceManagerBypassed
// (tests that need the managed path use stubServiceRuntime).
func stubCollisionVars(t *testing.T, procs []daemonProcessInfo, enumErr error, health func(root string) bool, killErr error) (*map[int]bool, func()) {
	t.Helper()
	oldList := daemonListDaemonProcesses
	oldHealth := daemonHealthCheck
	oldKill := daemonKillPID
	killed := map[int]bool{}
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) { return procs, enumErr }
	daemonHealthCheck = func(p *paths.Paths) (bool, error) {
		if health == nil {
			return false, nil
		}
		return health(p.Root()), nil
	}
	daemonKillPID = func(pid int) error {
		killed[pid] = true
		return killErr
	}
	return &killed, func() {
		daemonListDaemonProcesses = oldList
		daemonHealthCheck = oldHealth
		daemonKillPID = oldKill
	}
}

func TestReconcileCollidingDaemons_NoMatchIsNoOp(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	procs := []daemonProcessInfo{{PID: 4242, Root: filepath.Join(t.TempDir(), "other")}}
	_, restore := stubCollisionVars(t, procs, nil, nil, nil)
	defer restore()

	killed := false
	daemonKillPID = func(int) error { killed = true; return nil }

	if err := reconcileCollidingDaemons(p); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if killed {
		t.Fatal("no matching root, but a process was killed")
	}
}

func TestReconcileCollidingDaemons_SelfPidExcluded(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	procs := []daemonProcessInfo{{PID: selfPID(), Root: root}}
	_, restore := stubCollisionVars(t, procs, nil, func(string) bool { return true }, nil)
	defer restore()

	// Self is healthy but must not trip the "already running" refusal.
	if err := reconcileCollidingDaemons(p); err != nil {
		t.Fatalf("self pid must be excluded, got %v", err)
	}
}

func TestReconcileCollidingDaemons_RefusesWhenHealthyStrayExists(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	p := paths.WithRoot(root) // canonical start uses the real path
	strayRoot := link         // stray was started via the symlink spelling

	procs := []daemonProcessInfo{{PID: 99999, Root: strayRoot}}
	_, restore := stubCollisionVars(t, procs, nil, func(r string) bool { return r == strayRoot }, nil)
	defer restore()

	err := reconcileCollidingDaemons(p)
	if !errors.Is(err, errDaemonCollisionHealthy) {
		t.Fatalf("expected errDaemonCollisionHealthy, got %v", err)
	}
}

func TestReconcileCollidingDaemons_ReapsStaleStrayAndResetsManagedUnit(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// A socket file the stale stray left behind; cleanup must remove it.
	if err := os.WriteFile(p.Socket(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	procs := []daemonProcessInfo{{PID: 8888, Root: root}}
	killed, restore := stubCollisionVars(t, procs, nil, func(string) bool { return false }, nil)
	defer restore()

	// Engage the real managed-service plumbing so stopManagedService actually
	// issues systemctl commands we can observe, and so resetFailed runs.
	svcRestore := stubServiceRuntime(t)
	defer svcRestore()
	runtimeGOOS = "linux"
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	// Pretend the managed unit is installed so stopManagedService reaches the
	// systemctl stop call instead of short-circuiting.
	if err := os.MkdirAll(filepath.Dir(systemdUserServicePath(p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemdUserServicePath(p), []byte("[Service]\nExecStart=/x daemon run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var svcCmds []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		svcCmds = append(svcCmds, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := reconcileCollidingDaemons(p); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if !(*killed)[8888] {
		t.Error("stale stray pid 8888 was not killed")
	}
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Errorf("stale socket should have been cleaned up, stat err=%v", err)
	}
	unit := systemdServiceName(p)
	if !containsCmd(svcCmds, "systemctl --user stop "+unit) {
		t.Errorf("expected systemctl stop for %s in %v", unit, svcCmds)
	}
	if !containsCmd(svcCmds, "systemctl --user reset-failed "+unit) {
		t.Errorf("expected systemctl reset-failed for %s in %v", unit, svcCmds)
	}
}

func TestReconcileCollidingDaemons_FailsOpenOnEnumerationError(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	_, restore := stubCollisionVars(t, nil, errors.New("ps missing"), nil, nil)
	defer restore()

	if err := reconcileCollidingDaemons(p); err != nil {
		t.Fatalf("enumeration error must fail open, got %v", err)
	}
}

func TestReconcileCollidingDaemons_EmptyRootIsNoOp(t *testing.T) {
	called := false
	daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) {
		called = true
		return nil, nil
	}
	defer func() { daemonListDaemonProcesses = listDaemonProcesses }()

	if err := reconcileCollidingDaemons(paths.WithRoot("")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if called {
		t.Fatal("empty root must skip enumeration")
	}
}

func containsCmd(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

func selfPID() int { return os.Getpid() }

// TestListDaemonProcesses_FindsSpawnedDaemonRunHelper spawns the test binary
// with an argv that looks exactly like a managed daemon (`daemon run --root X`)
// and confirms the real ps-based collector rediscovers it. Unix-only: Windows
// enumerates via PowerShell CIM.
func TestListDaemonProcesses_FindsSpawnedDaemonRunHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps-based enumeration is unix-only")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("ps unavailable: %v", err)
	}

	root := t.TempDir()
	// TestMain in helpers_test.go parks processes with NM_DAEMON_HELPER_PROCESS
	// set, so the helper stays alive (sleeps) regardless of its argv. Its
	// command line still reads "<testbin> daemon run --root <root>".
	helper := exec.Command(os.Args[0], "daemon", "run", "--root", root)
	helper.Env = append(os.Environ(), "NM_DAEMON_HELPER_PROCESS=block")
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) && !found {
		infos, err := listDaemonProcesses()
		if err != nil {
			t.Fatalf("listDaemonProcesses: %v", err)
		}
		for _, info := range infos {
			if info.PID == helper.Process.Pid {
				if info.Root != root {
					t.Fatalf("found helper pid %d but root %q != %q", info.PID, info.Root, root)
				}
				found = true
				break
			}
		}
		if !found {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("ps enumeration did not find helper pid %d with root %q", helper.Process.Pid, root)
	}
}

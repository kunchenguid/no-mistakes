//go:build linux

package procreap

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRescueLinuxCWDLookupMissingLiveProcessRefusesQuiescence(t *testing.T) {
	f := &fakeSystem{procs: []Process{{PID: 2147483647, PPID: 1, PGID: 2147483647}}, cwds: map[int]string{}}
	f.install(t)
	processCWDsFunc = processCWDs
	if err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: t.TempDir(), RepoID: "repo", RunID: "run"}}}); err == nil {
		t.Fatal("missing live process CWD was accepted as confirmed absence")
	}
}

func TestRescueLinuxUnreadableLiveCWDRefusesQuiescence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRescueLinuxUnreadableCWDHelper$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "NM_RESCUE_UNREADABLE_CWD_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("unreadable CWD fixture did not start")
	}
	f := &fakeSystem{procs: []Process{{PID: cmd.Process.Pid, PPID: os.Getpid(), PGID: cmd.Process.Pid}}, cwds: map[int]string{}}
	f.install(t)
	processCWDsFunc = processCWDs
	if err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: cmd.Dir, RepoID: "repo", RunID: "run"}}}); err == nil {
		t.Fatal("unreadable live CWD was accepted as confirmed absence")
	}
}

func TestRescueLinuxUnreadableCWDHelper(t *testing.T) {
	if os.Getenv("NM_RESCUE_UNREADABLE_CWD_HELPER") != "1" {
		return
	}
	if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0, 0, 0, 0); err != 0 {
		t.Fatal(err)
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Minute)
	}
}

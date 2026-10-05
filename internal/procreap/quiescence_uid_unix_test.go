//go:build unix

package procreap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// uidProcess is fixture identity data, never a real cross-account process.
type uidProcess struct {
	pid, parent, group, uid int
}

// installUIDProcessTable executes a ps fixture that applies the requested UID
// filter before emitting the requested columns. Only identity/state metadata
// may be requested without that filter; foreign command lines are forbidden.
func installUIDProcessTable(t *testing.T, rows []uidProcess, incomplete bool) {
	t.Helper()
	body := "#!/bin/sh\nselected=''\ncolumns=''\nwhile [ $# -gt 0 ]; do\n" +
		"case \"$1\" in\n-u) selected=$2; shift 2;;\n-o|-eo) columns=$2; shift 2;;\n-ww) shift;;\n*) exit 2;;\nesac\ndone\n" +
		"case \"$columns\" in\n" +
		"pid=,ppid=,pgid=,etime=,command=)\n[ -n \"$selected\" ] || exit 2\n"
	for _, p := range rows {
		body += fmt.Sprintf("if [ \"$selected\" = %d ]; then printf '%d %d %d 00:01 fixture\\n'; fi\n", p.uid, p.pid, p.parent, p.group)
	}
	body += ";;\npid=,ppid=,pgid=,stat=)\n"
	for _, p := range rows {
		body += fmt.Sprintf("if [ -z \"$selected\" ] || [ \"$selected\" = %d ]; then printf '%d %d %d S\\n'; fi\n", p.uid, p.pid, p.parent, p.group)
	}
	if incomplete {
		body += "printf 'unreadable identity\\n'\n"
	}
	body += ";;\n*) exit 2;;\nesac\n"
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	listProcessesFunc = listProcesses
}

func TestRescueQuiescenceAccountsForChangedUIDOwnedWriters(t *testing.T) {
	for _, relation := range []string{"descendant", "led-group"} {
		for _, changed := range []bool{false, true} {
			t.Run(relation+"/changed="+strconv.FormatBool(changed), func(t *testing.T) {
				uid := os.Geteuid()
				writerUID := uid
				if changed {
					writerUID++
				}
				dir := t.TempDir()
				rows := []uidProcess{{900001, 1, 900001, uid}, {900002, 900001, 900002, writerUID}, {900003, 1, 900003, uid + 2}}
				if relation == "led-group" {
					rows[1].parent = 1
					rows[1].group = rows[0].group
				}
				f := &fakeSystem{cwds: map[int]string{900001: dir, 900002: "/fixture-outside"}}
				f.install(t)
				installUIDProcessTable(t, rows, false)
				lookup := processCWDsFunc
				processCWDsFunc = func(pids []int) (map[int]string, error) {
					for _, pid := range pids {
						if pid == 900003 || (changed && pid == 900002) {
							t.Fatalf("foreign CWD lookup: %d", pid)
						}
					}
					return lookup(pids)
				}
				if changed {
					signalProcessFunc = func(pid int, sig procSignal) error {
						if pid == 900002 {
							f.signals = append(f.signals, signalRecord{pid: pid, sig: sig})
							return syscall.EPERM
						}
						f.record(pid, false, sig)
						return nil
					}
					signalGroupFunc = func(pgid int, sig procSignal) error {
						// A group signal cannot override the fixture's UID denial.
						if pgid == 900002 {
							f.signals = append(f.signals, signalRecord{pid: pgid, group: true, sig: sig})
							return syscall.EPERM
						}
						f.record(pgid, true, sig)
						return nil
					}
				}
				err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: dir, RepoID: "repo", RunID: "run"}}, Scopes: []string{dir}, Grace: time.Millisecond})
				if (err != nil) != changed {
					t.Fatalf("shutdown=%v; want refusal=%v for UID %d", err, changed, writerUID)
				}
				if f.sentTo(900001, sigTerm) == changed || f.anySignalTo(900003) {
					t.Fatalf("owned/unrelated signal policy: %v", f.signals)
				}
				if changed && f.anySignalTo(900002) {
					t.Fatalf("unverifiable owner must retain without foreign signals: %v", f.signals)
				}
				if !changed && !f.sentTo(900002, sigTerm) {
					t.Fatalf("same-UID owned writer was missed: %v", f.signals)
				}
			})
		}
	}
}

func TestRescueQuiescenceRefusesIncompleteIdentityMetadata(t *testing.T) {
	f := &fakeSystem{cwds: map[int]string{}}
	f.install(t)
	installUIDProcessTable(t, []uidProcess{{900001, 1, 900001, os.Geteuid()}}, true)
	if err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: t.TempDir(), RepoID: "repo", RunID: "run"}}}); err == nil {
		t.Fatal("incomplete all-account identity metadata accepted as shutdown proof")
	}
}

func TestSweepChangedUIDIdentityKeepsOwnershipAndProtection(t *testing.T) {
	uid := os.Geteuid()
	dir := t.TempDir()
	rows := []uidProcess{
		{os.Getpid(), 900009, os.Getpid(), uid},
		{900009, 900008, 900009, uid + 1}, // foreign ancestor bridges protection
		{900008, 1, 900008, uid},
		{900001, 1, 900007, uid}, // inherited group must not expand
		{900002, 900001, 900002, uid + 1},
		{900004, 900002, 900004, uid + 1},
		{900005, 900004, 900005, uid}, // same-account child beyond foreign parents
		{900007, 1, 900007, uid + 1},
	}
	f := &fakeSystem{cwds: map[int]string{900001: dir, 900008: dir, 900005: "/fixture-outside"}}
	f.install(t)
	installUIDProcessTable(t, rows, false)
	lookup := processCWDsFunc
	processCWDsFunc = func(pids []int) (map[int]string, error) {
		for _, pid := range pids {
			if pid != 900001 && pid != 900005 {
				t.Fatalf("foreign/protected CWD lookup: %d", pid)
			}
		}
		return lookup(pids)
	}
	victims, err := Sweep(Options{Worktrees: []Worktree{{Dir: dir, RepoID: "repo", RunID: "run"}}, Scopes: []string{dir}, Grace: time.Millisecond})
	if err == nil || len(f.signals) != 0 {
		t.Fatalf("unverifiable descendants must retain before signals: %v %v", err, f.signals)
	}
	for _, pid := range []int{900001, 900002, 900004, 900005} {
		if !containsPID(victims, pid) {
			t.Fatalf("owned descendant %d missed: victims=%v signals=%v", pid, victimPIDs(victims), f.signals)
		}
	}
	for _, pid := range []int{os.Getpid(), 900009, 900008, 900007} {
		if containsPID(victims, pid) || f.anySignalTo(pid) {
			t.Fatalf("unrelated/protected identity %d reached: %v", pid, f.signals)
		}
	}
}

func TestRescueChangedUIDPreSweepCannotEraseShutdownOwnership(t *testing.T) {
	uid := os.Geteuid()
	dir := t.TempDir()
	rows := []uidProcess{{os.Getpid(), 1, os.Getpid(), uid}, {900001, 1, 900001, uid}, {900002, 900001, 900002, uid + 1}}
	f := &fakeSystem{cwds: map[int]string{900001: dir}}
	f.install(t)
	installUIDProcessTable(t, rows, false)
	signalProcessFunc = func(pid int, sig procSignal) error {
		if pid == 900002 {
			f.signals = append(f.signals, signalRecord{pid: pid, sig: sig})
			return syscall.EPERM
		}
		f.record(pid, false, sig)
		return nil
	}
	signalGroupFunc = func(pgid int, sig procSignal) error {
		if pgid == 900002 {
			f.signals = append(f.signals, signalRecord{pid: pgid, group: true, sig: sig})
			return syscall.EPERM
		}
		f.record(pgid, true, sig)
		return nil
	}
	opts := Options{Worktrees: []Worktree{{Dir: dir, RepoID: "repo", RunID: "run"}}, Scopes: []string{dir}, Grace: time.Millisecond}
	_, sweepErr := Sweep(opts)
	// Like the kernel's next snapshot, remove processes the pre-sweep killed.
	var remaining []uidProcess
	for _, row := range rows {
		if !f.dead[row.pid] {
			remaining = append(remaining, row)
		}
	}
	installUIDProcessTable(t, remaining, false)
	shutdownErr := Quiesce(context.Background(), opts)
	if shutdownErr == nil {
		t.Fatal("pre-sweep erased ownership and authorized cleanup with a live changed-UID writer")
	}
	if sweepErr == nil || len(f.signals) != 0 {
		t.Fatalf("pre-sweep must preserve the owned ancestry before signalling: %v %v", sweepErr, f.signals)
	}
}

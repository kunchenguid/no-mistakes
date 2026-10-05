//go:build unix

package procreap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRescueIncompleteLookupReapsKnownWriterAndRefusesQuiescence(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSystem{procs: []Process{{PID: 9999, PPID: 1, PGID: 9999}, {PID: 9998, PPID: 1, PGID: 9998}}, cwds: map[int]string{}}
	f.install(t)
	lookupErr := errors.New("one live CWD is unreadable")
	processCWDsFunc = func([]int) (map[int]string, error) { return map[int]string{9999: dir}, lookupErr }
	err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: dir, RepoID: "repo", RunID: "run"}}, Scopes: []string{dir}})
	if !errors.Is(err, lookupErr) || !f.sentTo(9999, sigTerm) || f.anySignalTo(9998) {
		t.Fatalf("partial lookup shutdown=%v signals=%v", err, f.signals)
	}
}

func TestRescueQuiescenceRequiresCompleteCWDLookup(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing", "failed", "timed-out", "partial", "complete", "vanished"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fakeSystem{procs: []Process{{PID: 9999, PPID: 1, PGID: 9999}}, cwds: map[int]string{}}
			f.install(t)
			bin := t.TempDir()
			body := "#!/bin/sh\n"
			switch scenario {
			case "failed":
				body += "printf 'p9999\\nn/elsewhere\\n'; exit 2\n"
			case "timed-out":
				body += "exec " + strconv.Quote(sleep) + " 11\n"
			case "partial":
				body += "printf 'p9999\\nfcwd\\n'\n"
			case "complete":
				body += "printf 'p9999\\nfcwd\\nn/elsewhere\\n'\n"
			case "vanished":
				body += "exit 1\n"
				f.dead[9999] = true
			}
			if scenario != "missing" {
				if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			processCWDsFunc = lsofCWDs
			err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: t.TempDir()}}})
			wantError := scenario != "complete" && scenario != "vanished"
			if (err != nil) != wantError {
				t.Fatalf("%s lookup shutdown result=%v wantError=%v", scenario, err, wantError)
			}
		})
	}
}

func TestRescueQuiescenceRefusesPartialProcessTable(t *testing.T) {
	f := &fakeSystem{procs: []Process{}, cwds: map[int]string{}}
	f.install(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nprintf '9999 1 9999 00:01 fixture\\nmalformed process\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	listProcessesFunc = listProcesses
	if err := Quiesce(context.Background(), Options{Worktrees: []Worktree{{Dir: t.TempDir(), RepoID: "repo", RunID: "run"}}}); err == nil {
		t.Fatal("partial population accepted as verified")
	}
}

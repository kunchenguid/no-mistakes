package daemon

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStartInstallsWindowsTaskAndStartsManagedDaemon(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	serviceExecutablePath = func() (string, error) { return exe, nil }

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		// Simulate fresh-install: the legacy unsuffixed task is absent, so
		// the pre-install cleanup query fails and cleanupLegacyWindowsTask
		// returns without issuing End/Delete.
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			return nil, fmt.Errorf("task not found")
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatal(err)
	}

	wantTaskCommand := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	wantQueryLegacy := "schtasks /Query /TN " + legacyWindowsTaskName + " /XML"
	wantCreate := "schtasks /Create /TN " + windowsTaskName(p) +
		" /SC ONLOGON /RL LIMITED /F /TR " + wantTaskCommand
	wantRun := "schtasks /Run /TN " + windowsTaskName(p)
	if len(commands) != 3 {
		t.Fatalf("expected schtasks create, legacy query, and run, got %v", commands)
	}
	if commands[0] != wantCreate {
		t.Fatalf("create command = %q, want %q", commands[0], wantCreate)
	}
	if commands[1] != wantQueryLegacy {
		t.Fatalf("legacy query command = %q, want %q", commands[1], wantQueryLegacy)
	}
	if commands[2] != wantRun {
		t.Fatalf("run command = %q, want %q", commands[2], wantRun)
	}
}

func TestInstallWindowsTaskDoesNotRemoveLegacyTaskForDifferentRoot(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			otherRoot := filepath.Join(t.TempDir(), "other-nm-home")
			return []byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>daemon run --root ` + otherRoot + `</Arguments></Exec></Task>`), nil
		}
		return nil, nil
	}

	if err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("install should not end or delete unrelated legacy task, got commands %v", commands)
	}
	if commands[1] != "schtasks /Query /TN "+legacyWindowsTaskName+" /XML" {
		t.Fatalf("legacy query command = %q", commands[1])
	}
}

func TestInstallWindowsTaskKeepsLegacyTaskOnCreateFailure(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			return nil, fmt.Errorf("create failed")
		}
		return nil, nil
	}

	err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`)
	if err == nil {
		t.Fatal("installWindowsTask should fail when schtasks create fails")
	}
	for _, command := range commands {
		if strings.Contains(command, "/End /TN "+legacyWindowsTaskName) || strings.Contains(command, "/Delete /TN "+legacyWindowsTaskName+" /F") {
			t.Fatalf("legacy cleanup should not run before successful scoped install, got %q", command)
		}
	}
}

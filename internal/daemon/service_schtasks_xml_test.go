package daemon

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const stubWindowsTaskAccount = `HOST\tester`

func stubWindowsTaskUser() (*user.User, error) {
	return &user.User{Username: stubWindowsTaskAccount}, nil
}

// readWindowsTaskDefinition decodes the UTF-16 task XML that a schtasks
// /Create /XML invocation points at, while the file still exists.
func readWindowsTaskDefinition(t *testing.T, args []string) string {
	t.Helper()
	path := windowsTaskDefinitionPath(args)
	if path == "" {
		t.Fatalf("schtasks create has no /XML argument: %v", args)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read task definition: %v", err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE || len(data)%2 != 0 {
		t.Fatalf("task definition is not UTF-16LE with BOM: % x", data[:min(len(data), 8)])
	}
	units := make([]uint16, 0, len(data)/2-1)
	for i := 2; i < len(data); i += 2 {
		units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

func windowsTaskDefinitionPath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "/XML" {
			return args[i+1]
		}
	}
	return ""
}

type windowsTaskDefinition struct {
	LogonTriggers []struct {
		Enabled string `xml:"Enabled"`
		UserID  string `xml:"UserId"`
	} `xml:"Triggers>LogonTrigger"`
	Principal struct {
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Command   string `xml:"Actions>Exec>Command"`
	Arguments string `xml:"Actions>Exec>Arguments"`
}

func parseWindowsTaskDefinition(t *testing.T, definition string) windowsTaskDefinition {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(definition))
	// The text is already decoded; the UTF-16 declaration only describes the file.
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	var task windowsTaskDefinition
	if err := decoder.Decode(&task); err != nil {
		t.Fatalf("parse task definition: %v\n%s", err, definition)
	}
	return task
}

// Issue #1192: an any-user ONLOGON trigger cannot be registered from a
// non-elevated token, so the trigger must be scoped to the installing user and
// the task must still run on that user's limited interactive token.
func TestRenderWindowsTaskXMLScopesLogonTriggerToInstallingUser(t *testing.T) {
	t.Parallel()

	exe := `C:\Program Files\R&D <tools>\no-mistakes.exe`
	root := `C:\Users\a&b\nm home`
	account := `CORP\a&b`
	task := parseWindowsTaskDefinition(t, renderWindowsTaskXML(exe, root, account))

	if len(task.LogonTriggers) != 1 {
		t.Fatalf("logon triggers = %d, want 1", len(task.LogonTriggers))
	}
	if got := task.LogonTriggers[0]; got.UserID != account || got.Enabled != "true" {
		t.Fatalf("logon trigger = %+v, want enabled and scoped to %s", got, account)
	}
	if task.Principal.UserID != account || task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" {
		t.Fatalf("principal = %+v, want %s on a limited interactive token", task.Principal, account)
	}
	if task.Command != exe {
		t.Fatalf("command = %q, want %q", task.Command, exe)
	}
	if want := "daemon run --root " + strconv.Quote(root); task.Arguments != want {
		t.Fatalf("arguments = %q, want %q", task.Arguments, want)
	}
}

func TestInstallWindowsTaskRemovesDefinitionFile(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	serviceCurrentUser = stubWindowsTaskUser

	for _, createErr := range []error{nil, fmt.Errorf("create failed")} {
		var definitionPath string
		serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
			if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
				definitionPath = windowsTaskDefinitionPath(args)
				return nil, createErr
			}
			return nil, fmt.Errorf("task not found")
		}
		_ = installWindowsTask(p, `C:\nm.exe`)
		if definitionPath == "" {
			t.Fatal("schtasks create was not given a definition file")
		}
		if _, err := os.Stat(definitionPath); !os.IsNotExist(err) {
			t.Fatalf("definition file %s left behind (create error %v): %v", definitionPath, createErr, err)
		}
	}
}

func TestInstallWindowsTaskFailsClosedWithoutCurrentUser(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	for name, current := range map[string]func() (*user.User, error){
		"lookup error":  func() (*user.User, error) { return nil, fmt.Errorf("no user") },
		"empty account": func() (*user.User, error) { return &user.User{Username: "  "}, nil },
	} {
		serviceCurrentUser = current
		var commands []string
		serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
			commands = append(commands, name+" "+strings.Join(args, " "))
			return nil, nil
		}
		if err := installWindowsTask(p, `C:\nm.exe`); err == nil {
			t.Fatalf("%s: installWindowsTask succeeded without a current user", name)
		}
		if len(commands) != 0 {
			t.Fatalf("%s: install must not fall back to an unscoped trigger, ran %v", name, commands)
		}
	}
}

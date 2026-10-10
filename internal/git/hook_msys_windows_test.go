//go:build windows && e2e

package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This test requires Windows and an existing loopback administrative share.
// Missing prerequisites fail rather than silently dropping either path case.
func TestReceiveHooksGitForWindows(t *testing.T) {
	shell := os.Getenv("NM_TEST_GIT_WINDOWS_SHELL")
	if shell == "" {
		t.Fatal("NM_TEST_GIT_WINDOWS_SHELL must name the Git for Windows bash.exe")
	}
	ctx := t.Context()
	testedHead := run(t, "", "git", "rev-parse", "HEAD")
	base := t.TempDir()
	cli := filepath.Join(base, "owner's native CLI.exe")
	build := exec.CommandContext(ctx, "go", "build", "-o", cli, "./testdata/receive-recorder")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recording native CLI: %v: %s", err, out)
	}
	cli = canonicalHookGate(t, cli)
	volume := filepath.VolumeName(base)
	if len(volume) != 2 || volume[1] != ':' {
		t.Fatalf("drive-letter fixture requires a drive path, got %q", base)
	}
	unc := `\\localhost\` + strings.ToLower(volume[:1]) + "$" + base[2:]
	if _, err := os.Stat(unc); err != nil {
		t.Fatalf("existing loopback UNC share %q is required (no share is created): %v", unc, err)
	}
	source := initTestRepo(t)
	head := run(t, source, "git", "rev-parse", "HEAD")
	for _, tc := range []struct {
		name string
		root string
	}{
		{"drive", filepath.Join(base, "drive owner")},
		{"UNC", filepath.Join(unc, "UNC owner")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(tc.root, "repos", "owned.git")
			other := filepath.Join(tc.root, "other owner", "repos", "copied.git")
			for _, dir := range []string{gate, other} {
				if err := InitBare(ctx, dir); err != nil {
					t.Fatal(err)
				}
				run(t, base, cli, "enroll", dir)
				run(t, base, cli, "current", dir)
			}
			gate = canonicalHookGate(t, gate)
			other = canonicalHookGate(t, other)
			if tc.name == "UNC" && !strings.HasPrefix(gate, `\\`) {
				t.Fatalf("UNC enrollment unexpectedly became a drive path: %q", gate)
			}
			record := filepath.Join(base, tc.name+"-calls.ndjson")
			poison := filepath.Join(base, tc.name+"-poison")
			if err := os.Mkdir(poison, 0o755); err != nil {
				t.Fatal(err)
			}
			otherComparison, err := windowsGateComparisonPath(other)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(poison, "cygpath"), "#!/bin/sh\nprintf '%s\\n' "+shellSingleQuote(otherComparison)+"\n")
			cliBytes, err := os.ReadFile(cli)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(poison, "no-mistakes.exe"), cliBytes, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MSYS2_ARG_CONV_EXCL", "*")
			t.Setenv("GIT_PUSH_OPTION_COUNT", "0")
			t.Setenv("NM_RECEIVE_RECORD", record)
			t.Setenv("NM_HOME", filepath.Join(base, "unrelated runtime"))
			t.Setenv("PWD", "unrelated-relative-directory")
			t.Setenv("PATH", poison+string(os.PathListSeparator)+os.Getenv("PATH"))
			input := strings.Repeat("0", 40) + " " + head + " refs/heads/live\n"
			for _, kind := range []string{"pre", "post"} {
				out, err := runMSYSHook(ctx, shell, gate, kind, input)
				if err != nil {
					t.Fatalf("enrolled %s hook: %v: %s", kind, err, out)
				}
				assertNativeHookCalls(t, record, cli, gate, head, []string{kind})
			}
			push := func(destination string) ([]byte, error) {
				cmd := exec.CommandContext(ctx, "git", "-c", "protocol.file.allow=always", "push", destination, "HEAD:refs/heads/live")
				cmd.Dir = source
				return cmd.CombinedOutput()
			}
			if out, err := push(gate); err != nil {
				t.Fatalf("push to enrolled gate: %v: %s", err, out)
			}
			assertNativeHookCalls(t, record, cli, gate, head, []string{"pre", "post"})
			if got, err := RunBare(ctx, gate, "rev-parse", "refs/heads/live"); err != nil || got != head {
				t.Fatalf("enrolled push ref = %q, want %q: %v", got, head, err)
			}
			for _, rel := range []string{"hooks/pre-receive", "hooks/post-receive", gateConfigStampFile} {
				content, err := os.ReadFile(filepath.Join(gate, rel))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(other, rel), content, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			current := exec.CommandContext(ctx, cli, "current", other)
			if out, err := current.CombinedOutput(); err == nil || !strings.Contains(string(out), "does not match enrolled owner") {
				t.Fatalf("copied enrollment stamp accepted: %v: %s", err, out)
			}
			if out, err := push(other); err == nil || !strings.Contains(string(out), "does not match enrolled gate") {
				t.Fatalf("copied hook push was not refused by gate equality: %v: %s", err, out)
			}
			if out, err := runMSYSHook(ctx, shell, other, "post", input); err != nil || !strings.Contains(string(out), "does not match enrolled gate") {
				t.Fatalf("copied notification was not refused: %v: %s", err, out)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("copied hooks invoked a CLI: %v", err)
			}
			if refs, err := RunBare(ctx, other, "for-each-ref", "--format=%(refname)"); err != nil || refs != "" {
				t.Fatalf("ref mutation after refused push: %q: %v", refs, err)
			}
			log, err := os.ReadFile(filepath.Join(other, "notify-push.log"))
			if err != nil || !strings.Contains(string(log), "does not match enrolled gate") {
				t.Fatalf("copied notification refusal missing from receiving gate log: %v: %s", err, log)
			}
			t.Logf("tested_head=%s case=%s result=pass gate=%q: enrolled push accepted with pinned native arguments; copied admission, notification and stamp refused; copied refs unchanged", testedHead, tc.name, gate)
		})
	}
}

func runMSYSHook(ctx context.Context, shell, gate, kind, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, shell, "--noprofile", "--norc", "-c", `exec /bin/sh "$1"`, "receive-hook-test", filepath.ToSlash(filepath.Join(gate, "hooks", kind+"-receive")))
	cmd.Dir = gate
	cmd.Env = append(os.Environ(), "GIT_DIR=.")
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}

func assertNativeHookCalls(t *testing.T, record, cli, gate, head string, kinds []string) {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("native CLI did not record the invocation: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(kinds) {
		t.Fatalf("native CLI recorded %d calls, want %d: %s", len(lines), len(kinds), data)
	}
	for i, kind := range kinds {
		var call struct {
			Args       []string `json:"args"`
			Home       string   `json:"home"`
			Helper     string   `json:"helper"`
			Executable string   `json:"executable"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &call); err != nil {
			t.Fatal(err)
		}
		want := []string{"daemon", "admit-push", "--gate", gate}
		if kind == "post" {
			want = []string{"daemon", "notify-push", "--gate", gate, "--ref", "refs/heads/live", "--old", strings.Repeat("0", 40), "--new", head}
		}
		if !reflect.DeepEqual(call.Args, want) || call.Home != filepath.Dir(filepath.Dir(gate)) || call.Helper != "1" || call.Executable != cli {
			t.Fatalf("native CLI call = %+v, want args %q and enrolled executable/root", call, want)
		}
		t.Logf("MSYS2_ARG_CONV_EXCL=*: native CLI recorded %s", lines[i])
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
}

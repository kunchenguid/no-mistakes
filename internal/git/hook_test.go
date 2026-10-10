package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func canonicalHookGate(t *testing.T, gate string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(gate)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func hookTestGate(t *testing.T) string {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	return gate
}

func TestWindowsGateComparisonPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate string
		want string
	}{
		{name: "drive letter", gate: `C:\Users\runner\nm\repos\repo.git`, want: "/c/Users/runner/nm/repos/repo.git"},
		{name: "UNC", gate: `\\server\share\nm\repos\repo.git`, want: "//server/share/nm/repos/repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowsGateComparisonPath(tc.gate)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("windowsGateComparisonPath(%q) = %q, want %q", tc.gate, got, tc.want)
			}
		})
	}
	for _, path := range []string{`relative\repos\repo.git`, `\\server`} {
		if _, err := windowsGateComparisonPath(path); err == nil {
			t.Errorf("windowsGateComparisonPath(%q) unexpectedly succeeded", path)
		}
	}
}

// The selected hook owner must survive a pushing shell's unrelated runtime
// and PATH. Execute the generated shell rather than checking only its text.
func TestReceiveHooksKeepEnrolledOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("receive hooks require /bin/sh")
	}
	for _, kind := range []string{"pre", "post"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "owner's runtime")
			gate := filepath.Join(root, "repos", "test.git")
			if err := InitBare(context.Background(), gate); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(base, "owner.txt")
			bin := filepath.Join(base, "owner's binary")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$NM_HOME\" > "+shellSingleQuote(record)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			script := preReceiveHookScript(bin, canonicalHookGate(t, gate), canonicalHookGate(t, gate))
			if kind == "post" {
				script = postReceiveHookScript(bin, canonicalHookGate(t, gate), canonicalHookGate(t, gate))
			}
			hook := filepath.Join(gate, "hooks", kind+"-receive")
			if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", hook)
			cmd.Dir = gate
			cmd.Env = append(os.Environ(), "NM_HOME="+filepath.Join(base, "unrelated"), "GIT_DIR="+gate)
			cmd.Stdin = strings.NewReader("old new refs/heads/feature\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hook: %v: %s", err, out)
			}
			got, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			want, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != want {
				t.Fatalf("executed root %q, want enrolled %q", got, want)
			}
		})
	}
}

func TestReceiveHooksRefuseCopiedHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("receive hooks require /bin/sh")
	}
	for _, kind := range []string{"pre", "post"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			ownedGate := filepath.Join(base, "owner", "repos", "owned.git")
			receivingGate := filepath.Join(base, "other", "repos", "receiving.git")
			for _, gate := range []string{ownedGate, receivingGate} {
				if err := InitBare(context.Background(), gate); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(base, "owner-executed")
			poisonDir := filepath.Join(base, "bin")
			if err := os.Mkdir(poisonDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(poisonDir, "cygpath"), []byte("#!/bin/sh\nprintf '%s\\n' "+shellSingleQuote(canonicalHookGate(t, receivingGate))+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(base, "fake-no-mistakes")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > "+shellSingleQuote(marker)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			script := preReceiveHookScript(bin, canonicalHookGate(t, ownedGate), canonicalHookGate(t, ownedGate))
			if kind == "post" {
				script = postReceiveHookScript(bin, canonicalHookGate(t, ownedGate), canonicalHookGate(t, ownedGate))
			}
			hook := filepath.Join(receivingGate, "hooks", kind+"-receive")
			if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("/bin/sh", hook)
			cmd.Dir = receivingGate
			cmd.Env = []string{"GIT_DIR=" + receivingGate, "PATH=" + poisonDir + string(os.PathListSeparator) + os.Getenv("PATH")}
			cmd.Stdin = strings.NewReader("old new refs/heads/feature\n")
			out, err := cmd.CombinedOutput()
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("copied hook invoked the enrolled binary: %v; output: %s", statErr, out)
			}
			if !strings.Contains(string(out), "does not match enrolled gate") {
				t.Fatalf("copied hook did not report the gate mismatch: %s", out)
			}
			if kind == "pre" && err == nil {
				t.Fatalf("copied pre-receive hook admitted the update: %s", out)
			}
			if kind == "post" {
				if err != nil {
					t.Fatalf("post-receive must preserve an accepted update: %v: %s", err, out)
				}
				log, readErr := os.ReadFile(filepath.Join(receivingGate, "notify-push.log"))
				if readErr != nil || !strings.Contains(string(log), "does not match enrolled gate") {
					t.Fatalf("copied post-receive failure was not logged to its receiving gate: %v: %s", readErr, log)
				}
			}
		})
	}
}

func TestReceiveHooksNeverSelectGlobalFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("receive hooks require /bin/sh")
	}
	for _, kind := range []string{"pre", "post"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			gate := filepath.Join(base, "repos", "test.git")
			if err := InitBare(context.Background(), gate); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(base, "global-executed")
			poison := filepath.Join(base, "bin")
			if err := os.Mkdir(poison, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(poison, "no-mistakes"), []byte("#!/bin/sh\ntouch "+shellSingleQuote(marker)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			missing := filepath.Join(base, "removed-owner")
			script := preReceiveHookScript(missing, canonicalHookGate(t, gate), canonicalHookGate(t, gate))
			if kind == "post" {
				script = postReceiveHookScript(missing, canonicalHookGate(t, gate), canonicalHookGate(t, gate))
			}
			hook := filepath.Join(gate, "hooks", kind+"-receive")
			if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", hook)
			cmd.Dir = gate
			cmd.Env = append(os.Environ(), "PATH="+poison+string(os.PathListSeparator)+os.Getenv("PATH"), "GIT_DIR="+gate)
			cmd.Stdin = strings.NewReader("old new refs/heads/feature\n")
			out, err := cmd.CombinedOutput()
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("global binary was selected: %v", statErr)
			}
			if kind == "pre" && err == nil {
				t.Fatalf("missing owner admitted push: %s", out)
			}
			if kind == "post" {
				if err != nil {
					t.Fatalf("post-receive must preserve accepted update: %v", err)
				}
				if strings.Contains(string(out), "Pipeline started") {
					t.Fatalf("failed notification reported launch: %s", out)
				}
				log, readErr := os.ReadFile(filepath.Join(gate, "notify-push.log"))
				if readErr != nil || !strings.Contains(string(log), "notify-push failed") {
					t.Fatalf("missing durable failure: %v: %s", readErr, log)
				}
			}
		})
	}
}

func TestGateConfigStampBindsEnrolledRoot(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a", "repos", "test.git")
	b := filepath.Join(base, "b", "repos", "test.git")
	for _, gate := range []string{a, b} {
		if err := InitBare(context.Background(), gate); err != nil {
			t.Fatal(err)
		}
		if err := RefreshManagedGateHooks(gate); err != nil {
			t.Fatal(err)
		}
		if err := MarkGateConfigCurrent(gate); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{gateConfigStampFile, "hooks/pre-receive", "hooks/post-receive"} {
		bytes, err := os.ReadFile(filepath.Join(a, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b, rel), bytes, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if GateConfigCurrent(b) {
		t.Fatal("another runtime's hook stamp was accepted")
	}
}

func TestReceiveHooksResolveSymlinkedGateAtEnrollment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("receive hooks require /bin/sh")
	}
	base := t.TempDir()
	root := filepath.Join(base, "enrolled")
	gate := filepath.Join(root, "repos", "test.git")
	if err := InitBare(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	record := filepath.Join(base, "invocation.txt")
	bin := filepath.Join(base, "owner binary")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$NM_HOME\" > " + shellSingleQuote(record) + "\nprintf '%s\\n' \"$@\" >> " + shellSingleQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := PostReceiveHookScript(filepath.Join(link, "repos", "test.git"))
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, "NM_BIN="+shellSingleQuote(exe), "NM_BIN="+shellSingleQuote(bin), 1)
	hook := filepath.Join(gate, "hooks", "post-receive")
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", hook)
	cmd.Dir = gate
	cmd.Env = append(os.Environ(), "GIT_DIR=.")
	cmd.Stdin = strings.NewReader("old new refs/heads/feature\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook: %v: %s", err, out)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	physicalGate := canonicalHookGate(t, gate)
	physicalRoot := filepath.Dir(filepath.Dir(physicalGate))
	if !strings.Contains(string(got), physicalRoot) || !strings.Contains(string(got), physicalGate) {
		t.Fatalf("hook did not invoke enrolled owner for %s: %s", physicalGate, got)
	}
}

func TestRefreshManagedPreReceiveHookPreservesCustomHook(t *testing.T) {
	bare := hookTestGate(t)
	hooks := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := []byte("#!/bin/sh\necho custom\n")
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), custom, 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := RefreshManagedPreReceiveHook(bare)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !changed {
		t.Fatal("expected managed wrapper installation")
	}
	preserved, err := os.ReadFile(filepath.Join(hooks, preservedPreReceiveHook))
	if err != nil {
		t.Fatalf("read preserved hook: %v", err)
	}
	if string(preserved) != string(custom) {
		t.Fatalf("preserved hook changed: %q", preserved)
	}
	managed, err := os.ReadFile(filepath.Join(hooks, "pre-receive"))
	if err != nil || !isManagedPreReceiveHook(managed) {
		t.Fatalf("managed admission hook missing: %v", err)
	}
}

func TestGateConfigCurrentRejectsMissingOrTamperedAdmissionHook(t *testing.T) {
	bare := hookTestGate(t)
	if err := RefreshManagedGateHooks(bare); err != nil {
		t.Fatalf("install hooks: %v", err)
	}
	if err := MarkGateConfigCurrent(bare); err != nil {
		t.Fatalf("mark current: %v", err)
	}
	if !GateConfigCurrent(bare) {
		t.Fatal("fresh managed admission hook should be current")
	}
	pre := filepath.Join(bare, "hooks", "pre-receive")
	if err := os.WriteFile(pre, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if GateConfigCurrent(bare) {
		t.Fatal("tampered admission hook must invalidate the gate stamp")
	}
}

func TestPostReceiveHookScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("receive hooks require /bin/sh")
	}
	base := t.TempDir()
	gate := hookTestGate(t)
	argsPath := filepath.Join(base, "args.txt")
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellSingleQuote(argsPath) + "\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		gate string
	}{
		{name: "POSIX", gate: canonicalHookGate(t, gate)},
		{name: "Windows drive", gate: `C:\Users\runner\nm\repos\repo.git`},
		{name: "Windows UNC", gate: `\\server\share\nm\repos\repo.git`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The real shell resolves the local receiving gate; the recording
			// CLI must receive the separately pinned native spelling. This
			// exercises argument selection without requiring a Windows host.
			hookPath := filepath.Join(base, "post-receive")
			if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin, tc.gate, canonicalHookGate(t, gate))), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", hookPath)
			cmd.Dir = gate
			cmd.Env = append(os.Environ(), "GIT_DIR=.", "MSYS2_ARG_CONV_EXCL=*")
			cmd.Stdin = strings.NewReader("old new refs/heads/main\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("run post-receive hook: %v: %s", err, out)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := gateArgFromArgv(string(args)); got != tc.gate {
				t.Fatalf("hook passed gate %q, want pinned native gate %q", got, tc.gate)
			}
			t.Logf("MSYS2_ARG_CONV_EXCL=*: CLI received --gate %q", gateArgFromArgv(string(args)))
		})
	}
}

func TestShellSingleQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "/usr/bin/no-mistakes", "'/usr/bin/no-mistakes'"},
		{"spaces", "/opt/No Mistakes/bin", "'/opt/No Mistakes/bin'"},
		{"single_quote", "/opt/it's/bin", "'/opt/it'\"'\"'s/bin'"},
		{"multiple_quotes", "a'b'c", "'a'\"'\"'b'\"'\"'c'"},
		{"empty", "", "''"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellSingleQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellSingleQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPostReceiveHookScriptDoesNotEvaluatePushOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}

	base := t.TempDir()
	bare := filepath.Join(base, "repos", "test.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	argsPath := filepath.Join(base, "args.txt")
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\nprintf '%s\n' \"$@\" > " + shellSingleQuote(argsPath) + "\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(base, "post-receive")
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin, canonicalHookGate(t, bare), canonicalHookGate(t, bare))), 0o755); err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(base, "pwned")
	cmd := exec.Command("/bin/sh", hookPath)
	cmd.Dir = bare
	cmd.Stdin = strings.NewReader("oldrev newrev refs/heads/main\n")
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+bare,
		"GIT_PUSH_OPTION_COUNT=1",
		"GIT_PUSH_OPTION_0=ok; touch "+markerPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v: %s", err, out)
	}

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("hook should not evaluate push option shell syntax, stat err: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "ok; touch "+markerPath) {
		t.Fatalf("hook should forward push option literally, got:\n%s", args)
	}
}

func TestInstallPostReceiveHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	if err := InstallPostReceiveHook(bare); err != nil {
		t.Fatalf("InstallPostReceiveHook failed: %v", err)
	}

	hookPath := filepath.Join(bare, "hooks", "post-receive")

	// verify file exists
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("hook file not found: %v", err)
	}

	// verify executable permission
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("hook should be executable, got mode %v", info.Mode())
	}

	// verify content matches template
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolvedExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != postReceiveHookScript(resolvedExe, canonicalHookGate(t, bare), canonicalHookGate(t, bare)) {
		t.Fatal("hook content doesn't match template")
	}
}

func TestRefreshManagedPostReceiveHookPreservesCustomHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(bare, "hooks", "post-receive")
	custom := "#!/bin/sh\necho custom hook\n"
	if err := os.WriteFile(hookPath, []byte(custom), 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshManagedPostReceiveHook(bare)
	if err != nil {
		t.Fatalf("RefreshManagedPostReceiveHook: %v", err)
	}
	if changed {
		t.Fatal("custom hook should not be overwritten")
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Fatalf("custom hook changed to:\n%s", data)
	}
}

func TestRefreshManagedPostReceiveHookInstallsMissingHook(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshManagedPostReceiveHook(bare)
	if err != nil {
		t.Fatalf("RefreshManagedPostReceiveHook: %v", err)
	}
	if !changed {
		t.Fatal("missing managed hook should be installed")
	}
	info, err := os.Stat(filepath.Join(bare, "hooks", "post-receive"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("hook should be executable, got mode %v", info.Mode())
	}
}

// TestPostReceiveHook_PinsEnrolledGate covers issue #269 with a poisoned PWD.
// The hook resolves Git's receiving gate and confirms it matches the canonical
// gate recorded at enrollment before notifying the daemon.
func TestPostReceiveHook_PinsEnrolledGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()

	base := t.TempDir()
	bare := filepath.Join(base, "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Fake no-mistakes binary: record the notify-push argv so we can inspect
	// the --gate value the hook actually computed.
	argsPath := filepath.Join(base, "args.txt")
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellSingleQuote(argsPath) + "\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(bare, "hooks", "post-receive")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin, canonicalHookGate(t, bare), canonicalHookGate(t, bare))), 0o755); err != nil {
		t.Fatal(err)
	}

	// Poison PWD and invoke from the bare repo. The embedded enrollment path
	// must remain the selected gate.
	cmd := exec.Command("/bin/sh", hookPath)
	cmd.Dir = bare
	cmd.Stdin = strings.NewReader("oldrev newrev refs/heads/main\n")
	cmd.Env = append(os.Environ(), "PWD=.", "GIT_DIR="+bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v: %s", err, out)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("fake binary should have recorded argv: %v", err)
	}
	gate := gateArgFromArgv(string(args))
	if gate == "" {
		t.Fatalf("hook did not pass --gate; recorded argv:\n%s", args)
	}
	if gate == "." || !filepath.IsAbs(gate) {
		t.Fatalf("--gate must be an absolute path, got %q (issue #269); argv:\n%s", gate, args)
	}
	// The resolved gate dir must be the bare repo (compare physical paths to
	// tolerate macOS /tmp -> /private/tmp symlink resolution).
	wantAbs, err := filepath.EvalSymlinks(bare)
	if err != nil {
		wantAbs = bare
	}
	gotAbs, err := filepath.EvalSymlinks(gate)
	if err != nil {
		gotAbs = gate
	}
	if gotAbs != wantAbs {
		t.Fatalf("--gate = %q (resolved %q), want bare dir %q", gate, gotAbs, wantAbs)
	}
}

func TestPostReceiveHookRefusesWhenReceivingGateCannotBeResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()

	base := t.TempDir()
	bare := filepath.Join(base, "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	argsPath := filepath.Join(base, "args.txt")
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellSingleQuote(argsPath) + "\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakePath := filepath.Join(base, "bin")
	if err := os.MkdirAll(fakePath, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(fakePath, "git")
	gitMarker := filepath.Join(base, "git-executed")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > "+shellSingleQuote(gitMarker)+"\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hookPath := filepath.Join(bare, "hooks", "post-receive")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin, canonicalHookGate(t, bare), canonicalHookGate(t, bare))), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", hookPath)
	cmd.Dir = bare
	cmd.Stdin = strings.NewReader("oldrev newrev refs/heads/main\n")
	cmd.Env = []string{
		"PATH=" + fakePath,
		"PWD=.",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run hook: %v: %s", err, out)
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("hook invoked its binary without a resolved receiving gate: %v", err)
	}
	if _, err := os.Stat(gitMarker); !os.IsNotExist(err) {
		t.Fatalf("hook trusted PATH-selected git to discover its receiving gate: %v", err)
	}
	if !strings.Contains(string(out), "GIT_DIR is unavailable") || strings.Contains(string(out), "Pipeline started") {
		t.Fatalf("unresolved receiving gate was not reported as a failed notification: %s", out)
	}
	log, err := os.ReadFile(filepath.Join(bare, "notify-push.log"))
	if err != nil || !strings.Contains(string(log), "GIT_DIR is unavailable") {
		t.Fatalf("unresolved receiving gate failure was not logged: %v: %s", err, log)
	}
}

// gateArgFromArgv returns the value following the first "--gate" token in a
// newline-separated argv dump, or "" if absent.
func gateArgFromArgv(argv string) string {
	fields := strings.Split(strings.TrimSpace(argv), "\n")
	for i, f := range fields {
		if f == "--gate" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// TestPostReceiveHook_SurfacesNotifyFailures covers issue #122 defect 2:
// when notify-push fails (daemon down, missing-hook state, etc.), the user
// must see the failure on stderr instead of getting a clean-looking push.
// We also persist failures to <bareDir>/notify-push.log so they survive past
// the terminal scrollback.
//
// Note: post-receive's exit code is ignored by git, so we can't make
// `git push` exit non-zero. The wizard's push-confirmation step (defect 3)
// is responsible for surfacing the failure to the user as an error.
func TestPostReceiveHook_SurfacesNotifyFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()

	base := t.TempDir()
	bare := filepath.Join(base, "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.email", "t@t.com").CombinedOutput(); err != nil {
		t.Fatalf("config email: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.name", "T").CombinedOutput(); err != nil {
		t.Fatalf("config name: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "remote", "add", "gate", bare).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}

	// Fake no-mistakes binary that always fails notify-push with a
	// distinctive marker on stderr.
	fakeBin := filepath.Join(base, "fake-no-mistakes")
	fakeScript := "#!/bin/sh\necho 'TESTMARKER notify failed' >&2\nexit 7\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Install the real hook generated against the fake binary.
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	if err := os.WriteFile(hookPath, []byte(postReceiveHookScript(fakeBin, canonicalHookGate(t, bare), canonicalHookGate(t, bare))), 0o755); err != nil {
		t.Fatal(err)
	}

	// Push. We don't care whether `git push` exits zero (post-receive
	// exit code is ignored by git); we care that the failure surfaced.
	pushOut, _ := exec.Command("git", "-C", work, "push", "gate", "HEAD:refs/heads/main").CombinedOutput()

	if !strings.Contains(string(pushOut), "TESTMARKER notify failed") {
		t.Errorf("push output should surface notify-push stderr to the client, got:\n%s", pushOut)
	}

	logPath := filepath.Join(bare, "notify-push.log")
	logContent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("notify-push.log should exist at %s: %v", logPath, err)
	}
	if !strings.Contains(string(logContent), "TESTMARKER notify failed") {
		t.Errorf("notify-push.log should contain notify-push stderr, got:\n%s", logContent)
	}
}

func TestIsolateHooksPath_OverridesPoisonedSharedConfig(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Simulate husky writing core.hookspath into the bare's shared local
	// config (this is what `git config core.hookspath .husky/_` does when
	// invoked from a linked worktree).
	if out, err := exec.Command("git", "-C", bare, "config", "core.hookspath", ".husky/_").CombinedOutput(); err != nil {
		t.Fatalf("seed shared core.hookspath: %v: %s", err, out)
	}

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	out, err := exec.Command("git", "-C", bare, "config", "--get", "core.hookspath").Output()
	if err != nil {
		t.Fatalf("get core.hookspath: %v", err)
	}
	want, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("effective core.hookspath = %q, want %q (per-worktree should win over poisoned shared)", got, want)
	}

	// Verify the shared poisoning is still observable in the local scope -
	// we don't try to clean it up because husky will just re-add it on the
	// next pipeline run. Per-worktree is what protects us.
	out, err = exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hookspath").Output()
	if err != nil {
		t.Fatalf("get local core.hookspath: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != ".husky/_" {
		t.Errorf("local core.hookspath = %q, want %q", got, ".husky/_")
	}
}

// TestIsolateHooksPath_LinkedWorktreeCanRebase covers the regression where
// enabling extensions.worktreeConfig on a bare repo while leaving
// core.bare=true in shared config caused `git rebase` to fail inside linked
// worktrees with "fatal: this operation must be run in a work tree". Git
// requires core.bare to live in per-worktree scope once worktreeConfig is on.
func TestIsolateHooksPath_LinkedWorktreeCanRebase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree + shell pipeline is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Seed the bare with one commit on main so worktree add has a ref to check out.
	seedGate(t, base, bare)

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// The bare repo itself must still look bare to git, otherwise other
	// operations (ls-remote, receive-pack) regress.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("bare repo should still report is-bare-repository=true, got %q", got)
	}

	// Hook resolution must still point at the bare's hooks dir (the whole
	// point of IsolateHooksPath).
	wantHooks, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "config", "--get", "core.hookspath")); got != wantHooks {
		t.Fatalf("core.hookspath = %q, want %q", got, wantHooks)
	}

	// Create a linked worktree and confirm it actually behaves as a work tree.
	wt := filepath.Join(base, "wt")
	runGitOrFatal(t, bare, "worktree", "add", "--detach", wt, "refs/heads/main")

	if got := strings.TrimSpace(runGitOrFatal(t, wt, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatalf("linked worktree should report is-inside-work-tree=true, got %q", got)
	}

	// Rebase onto the bare's main. Before the fix this errored with
	// "fatal: this operation must be run in a work tree" because core.bare=true
	// leaked from shared config into the linked worktree.
	runGitOrFatal(t, wt, "fetch", bare, "main")
	if out, err := exec.Command("git", "-C", wt, "rebase", "FETCH_HEAD").CombinedOutput(); err != nil {
		t.Fatalf("rebase in linked worktree should succeed, got err=%v output:\n%s", err, out)
	}
}

// TestIsolateHooksPath_PushToGateStillWorks guards the other critical path:
// after IsolateHooksPath moves core.bare to per-worktree scope, a normal
// `git push` from a working repo to the gate must still land, and the
// post-receive hook (resolved via the pinned core.hookspath) must still fire.
func TestIsolateHooksPath_PushToGateStillWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	work := seedGate(t, base, bare)

	// Install a trivial post-receive hook that writes a marker file so we can
	// confirm hookspath resolution still finds the bare's hooks dir.
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "hook-ran")
	hookScript := "#!/bin/sh\necho ran > " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// Make a second commit and push it. Push must succeed and the ref on the
	// bare must advance to the new HEAD.
	runGitOrFatal(t, work, "commit", "--allow-empty", "-m", "second")
	newSHA := strings.TrimSpace(runGitOrFatal(t, work, "rev-parse", "HEAD"))
	if out, err := exec.Command("git", "-C", work, "push", "gate", "HEAD:refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("push to gate should succeed, got err=%v output:\n%s", err, out)
	}
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "refs/heads/main")); got != newSHA {
		t.Fatalf("bare refs/heads/main = %q, want %q (push did not advance ref)", got, newSHA)
	}

	// Hook fired via the per-worktree hookspath.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post-receive hook should have run, marker missing: %v", err)
	}
}

// seedGate creates a working repo under base/work, wires it to the bare as a
// remote, makes an initial commit, and pushes it as refs/heads/main. Returns
// the working repo path.
func seedGate(t *testing.T, base, bare string) string {
	t.Helper()
	work := filepath.Join(base, "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	runGitOrFatal(t, work, "config", "user.email", "t@t.com")
	runGitOrFatal(t, work, "config", "user.name", "T")
	runGitOrFatal(t, work, "remote", "add", "gate", bare)
	runGitOrFatal(t, work, "commit", "--allow-empty", "-m", "init")
	runGitOrFatal(t, work, "push", "gate", "HEAD:refs/heads/main")
	return work
}

func runGitOrFatal(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestIsolateHooksPath_MigratesFromPreFixState covers the upgrade path for
// bare repos created by the previous version of IsolateHooksPath, which left
// core.bare=true in shared config. Running the new IsolateHooksPath on that
// state must relocate core.bare to per-worktree scope so linked worktrees
// stop inheriting core.bare=true.
func TestIsolateHooksPath_MigratesFromPreFixState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree + shell pipeline is /bin/sh-only")
	}
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-fix state: extensions.worktreeConfig on, hookspath
	// pinned per-worktree, but core.bare still in shared config.
	hooksDir, err := filepath.Abs(filepath.Join(bare, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, bare, "config", "extensions.worktreeConfig", "true")
	runGitOrFatal(t, bare, "config", "--worktree", "core.hookspath", hooksDir)

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// core.bare must no longer be in shared config.
	if out, err := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.bare").CombinedOutput(); err == nil {
		t.Fatalf("core.bare should have been removed from shared config, still set to %q", strings.TrimSpace(string(out)))
	}
	// core.bare must now be in per-worktree config with value true.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "config", "--worktree", "--get", "core.bare")); got != "true" {
		t.Fatalf("core.bare in config.worktree = %q, want %q", got, "true")
	}
	// Bare still reports as bare.
	if got := strings.TrimSpace(runGitOrFatal(t, bare, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("bare repo should still report is-bare-repository=true, got %q", got)
	}
}

func TestIsolateHooksPath_Idempotent(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("second call should be a no-op: %v", err)
	}
}

func TestIsolateHooksPath_SkipsIsolationWhenWorktreeConfigUnsupported(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repos", "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	originalRunGit := runGit
	runGit = func(_ context.Context, dir string, args ...string) (string, error) {
		t.Helper()
		if dir != bare {
			t.Fatalf("run dir = %q, want %q", dir, bare)
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "--worktree" {
			return "", exec.ErrNotFound
		}
		return originalRunGit(ctx, dir, args...)
	}
	defer func() { runGit = originalRunGit }()

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath should tolerate missing --worktree support: %v", err)
	}

	out, err := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hookspath").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("local core.hookspath should remain unset without worktree support, got %q", strings.TrimSpace(string(out)))
	}

	out, err = exec.Command("git", "-C", bare, "config", "--local", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("extensions.worktreeConfig should remain unset without worktree support, got %q", strings.TrimSpace(string(out)))
	}
}

// TestIsolateHooksPath_LinkedWorktreeResolvesRepoForCLI reproduces the CI
// step's working directory: a detached linked worktree of the gate bare repo,
// which records origin = the GitHub upstream. The CI watch shells out to `gh`
// from this directory, and gh resolves the repository by running git there.
// This guards the invariant that such a worktree is a real work tree with no
// leaked core.bare and a resolvable origin - i.e. gh can determine the repo
// from cwd alone. See issue #255: when this breaks, `gh pr checks` fails every
// poll and the CI step hangs until ci_timeout. No real gh or network is
// needed; the failure is purely in git's repo resolution, which gh depends on.
func TestIsolateHooksPath_LinkedWorktreeResolvesRepoForCLI(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)

	// The gate records origin = upstream so worktrees can resolve the repo
	// (gate.provisionGate does this for exactly this reason).
	const upstream = "https://github.com/test/repo.git"
	if err := EnsureRemote(ctx, bare, "origin", upstream); err != nil {
		t.Fatalf("EnsureRemote: %v", err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath: %v", err)
	}

	// IsolateHooksPath is best-effort: on a git too old for `config --worktree`
	// it cannot relocate core.bare, and the invariant below genuinely cannot
	// hold. Skip there - that limitation is the subject of
	// TestLinkedWorktreeLeaksCoreBareWithoutRelocation.
	if !worktreeConfigIsolatesCoreBare(t, bare) {
		t.Skip("git lacks `config --worktree`; core.bare cannot be isolated on this host")
	}

	wt := filepath.Join(base, "wt")
	if err := WorktreeAdd(ctx, bare, wt, "refs/heads/main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	if got := strings.TrimSpace(runGitOrFatal(t, wt, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatalf("CI-step worktree is-inside-work-tree = %q, want true", got)
	}
	// core.bare must not leak into the worktree. If it resolves true here,
	// stricter git refuses work-tree operations and gh fails to resolve the
	// repo from cwd - the #255 hang.
	if out, err := exec.Command("git", "-C", wt, "config", "--get", "core.bare").CombinedOutput(); err == nil {
		if got := strings.TrimSpace(string(out)); got == "true" {
			t.Fatalf("core.bare leaked into linked worktree (=%q); gh repo resolution breaks on stricter git", got)
		}
	}
	// gh resolves the repo from the remotes of cwd; origin must be reachable.
	if got := strings.TrimSpace(runGitOrFatal(t, wt, "remote", "get-url", "origin")); got != upstream {
		t.Fatalf("origin url in worktree = %q, want %q", got, upstream)
	}
	// gh also runs `git rev-parse --absolute-git-dir`; it must succeed.
	if _, err := exec.Command("git", "-C", wt, "rev-parse", "--absolute-git-dir").Output(); err != nil {
		t.Fatalf("rev-parse --absolute-git-dir in worktree failed: %v", err)
	}
}

// TestLinkedWorktreeLeaksCoreBareWithoutRelocation pins the failure mechanism
// behind issue #255 for a git too old for per-worktree config. When
// IsolateHooksPath cannot relocate core.bare (no `config --worktree`), the bare
// repo's core.bare=true stays in shared config and leaks into every linked
// worktree. A worktree that reports core.bare=true is treated as bare by
// stricter git, so git - and therefore gh - fail to operate from it, and that
// worktree is the CI step's cwd. The old-git path is simulated deterministically
// via the runGit stub so this reproduces the reporter's config state on any git.
func TestLinkedWorktreeLeaksCoreBareWithoutRelocation(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)

	// Make `config --worktree` look unsupported so IsolateHooksPath takes its
	// best-effort no-op path, leaving core.bare=true in shared config - exactly
	// the state an old git leaves on the reporter's host.
	originalRunGit := runGit
	runGit = func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "config" && args[1] == "--worktree" {
			return "", exec.ErrNotFound
		}
		return originalRunGit(ctx, dir, args...)
	}
	defer func() { runGit = originalRunGit }()

	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatalf("IsolateHooksPath should no-op without worktree support: %v", err)
	}

	wt := filepath.Join(base, "wt")
	if err := WorktreeAdd(ctx, bare, wt, "refs/heads/main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// The leak: core.bare=true resolves inside the linked worktree, which is
	// what makes gh (and git) treat the CI step's cwd as bare on stricter git.
	got := strings.TrimSpace(runGitOrFatal(t, wt, "config", "--get", "core.bare"))
	if got != "true" {
		t.Fatalf("expected core.bare to leak into linked worktree (=true) without relocation, got %q", got)
	}
}

// worktreeConfigIsolatesCoreBare reports whether the host git supports
// per-worktree config, i.e. whether IsolateHooksPath was able to relocate
// core.bare into per-worktree scope. When false, the host cannot isolate
// core.bare at all (best-effort path).
func worktreeConfigIsolatesCoreBare(t *testing.T, bare string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "config", "--worktree", "--get", "core.bare").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func TestInstallPostReceiveHookCreatesDir(t *testing.T) {
	// hooks dir might not exist in some bare repos; installer should create it
	dir := t.TempDir()
	bareDir := filepath.Join(dir, "repos", "test.git")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := InstallPostReceiveHook(bareDir); err != nil {
		t.Fatalf("InstallPostReceiveHook should create hooks dir: %v", err)
	}

	hookPath := filepath.Join(bareDir, "hooks", "post-receive")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("hook file not found: %v", err)
	}
}

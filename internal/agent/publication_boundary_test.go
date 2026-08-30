package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type publicationBoundaryFixture struct {
	boundary *PublicationCodexBoundaryV1
	paths    map[PublicationExecutableRole]string
	opts     PublicationCodexBoundaryOptions
}

func writePublicationBoundaryExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func newPublicationBoundaryFixture(t *testing.T) publicationBoundaryFixture {
	t.Helper()
	dir := t.TempDir()
	logical := writePublicationBoundaryExecutable(t, dir, "codex", `exit 97`)
	native := writePublicationBoundaryExecutable(t, dir, "codex-native", `
if [ "$1" = "--version" ]; then
  printf '%s\n' 'codex-cli 0.150.1'
  exit 0
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
`)
	helper := writePublicationBoundaryExecutable(t, dir, "codex-linux-sandbox", `exec "$@"`)
	sentinel := writePublicationBoundaryExecutable(t, dir, "sentinel", `sleep "$1"`)
	bwrap := writePublicationBoundaryExecutable(t, dir, "bwrap", `
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
[ "$#" -gt 0 ] && shift
exec "$@"
`)
	opts := PublicationCodexBoundaryOptions{
		LogicalEntryPath:       logical,
		NativeCodexPath:        native,
		SandboxHelperPath:      helper,
		BubblewrapPath:         bwrap,
		PermissionProfile:      []byte(`{"sandbox":"publication-v1","network":false}`),
		ProbeFixedArgs:         []string{"sandbox", "linux", "--profile-inline", "publication-v1"},
		ExecFixedArgs:          []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--json"},
		CommandFixedArgs:       []string{"sandbox", "linux", "--profile-inline", "publication-v1", "--"},
		BootstrapDir:           filepath.Join(dir, "bootstrap"),
		ConfigHomeDir:          filepath.Join(dir, "config-home"),
		SentinelExecutablePath: sentinel,
	}
	boundary, err := NewPublicationCodexBoundaryV1(context.Background(), opts)
	if err != nil {
		t.Fatalf("bind publication Codex boundary: %v", err)
	}
	return publicationBoundaryFixture{
		boundary: boundary,
		opts:     opts,
		paths: map[PublicationExecutableRole]string{
			PublicationExecutableLogicalEntry:  logical,
			PublicationExecutableNativeCodex:   native,
			PublicationExecutableSandboxHelper: helper,
			PublicationExecutableBubblewrap:    bwrap,
			PublicationExecutableSentinel:      sentinel,
		},
	}
}

func TestPublicationCodexBoundaryBindsCompleteOrderedExecutableClosure(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	manifest := fixture.boundary.Manifest()
	wantRoles := []PublicationExecutableRole{
		PublicationExecutableLogicalEntry,
		PublicationExecutableNativeCodex,
		PublicationExecutableSandboxHelper,
		PublicationExecutableBubblewrap,
		PublicationExecutableSentinel,
	}
	if len(manifest.ExecutableClosure) != len(wantRoles) {
		t.Fatalf("closure length=%d, want %d: %#v", len(manifest.ExecutableClosure), len(wantRoles), manifest.ExecutableClosure)
	}
	for index, wantRole := range wantRoles {
		entry := manifest.ExecutableClosure[index]
		if entry.Role != wantRole {
			t.Fatalf("closure[%d].role=%q, want %q", index, entry.Role, wantRole)
		}
		wantRealpath, err := filepath.EvalSymlinks(fixture.paths[wantRole])
		if err != nil {
			t.Fatal(err)
		}
		if entry.RealPath != wantRealpath || len(entry.RawSHA256) != 64 || entry.FileIdentity == "" || entry.OwnerIdentity == "" || entry.Mode&0o111 == 0 {
			t.Fatalf("closure[%d]=%#v, want realpath/raw SHA/mode/file identity", index, entry)
		}
	}
	if manifest.NativeVersion != "codex-cli 0.150.1" || manifest.GOOS != runtime.GOOS || manifest.GOARCH != runtime.GOARCH {
		t.Fatalf("native/platform binding=%#v", manifest)
	}
	for name, digest := range map[string]string{
		"policy template": manifest.PolicyTemplateSHA256,
		"probe":           manifest.ProbeArgvSHA256,
		"exec":            manifest.ExecArgvSHA256,
		"command":         manifest.CommandArgvSHA256,
	} {
		if len(digest) != 64 {
			t.Errorf("%s digest=%q, want raw SHA-256", name, digest)
		}
	}
}

func TestPublicationCodexBoundaryCopiesPolicyAndEveryFixedArgv(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	before := fixture.boundary.Manifest()
	fixture.opts.PermissionProfile[0] ^= 0xff
	fixture.opts.ProbeFixedArgs[0] = "mutated-probe"
	fixture.opts.ExecFixedArgs[0] = "mutated-exec"
	fixture.opts.CommandFixedArgs[0] = "mutated-command"
	after := fixture.boundary.Manifest()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("caller mutation changed immutable boundary manifest:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestPublicationCodexBoundaryRevalidatesEveryClosureRoleBeforeEveryLaunch(t *testing.T) {
	launches := []PublicationLaunchKind{PublicationLaunchProbe, PublicationLaunchExec, PublicationLaunchCommand}
	roles := []PublicationExecutableRole{
		PublicationExecutableLogicalEntry,
		PublicationExecutableNativeCodex,
		PublicationExecutableSandboxHelper,
		PublicationExecutableBubblewrap,
		PublicationExecutableSentinel,
	}
	for _, role := range roles {
		for _, launch := range launches {
			t.Run(string(role)+"/"+string(launch), func(t *testing.T) {
				fixture := newPublicationBoundaryFixture(t)
				path := fixture.paths[role]
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 41\n# swapped\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				err := fixture.boundary.RevalidateForLaunch(launch)
				if !errors.Is(err, ErrPublicationConfinementUnavailable) {
					t.Fatalf("%s swap before %s launch error=%v, want confinement_unavailable", role, launch, err)
				}
			})
		}
	}
}

func TestPublicationCodexBoundaryDirectlyExecutesPinnedNativeWithClosedBootstrapPath(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	container := t.TempDir()
	candidate := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	source := t.TempDir()
	for _, dir := range []string{candidate, scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	view, err := fixture.boundary.BindView(candidate, source, scratch)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := view.AgentCommand(context.Background(), "")
	if err != nil {
		t.Fatalf("build protected Codex command: %v", err)
	}
	wantBwrap, err := filepath.EvalSymlinks(fixture.paths[PublicationExecutableBubblewrap])
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != wantBwrap || cmd.Args[0] != wantBwrap {
		t.Fatalf("command path/argv0=%q/%q, want pinned lifecycle wrapper %q", cmd.Path, cmd.Args[0], wantBwrap)
	}
	wantNative, err := filepath.EvalSymlinks(fixture.paths[PublicationExecutableNativeCodex])
	if err != nil {
		t.Fatal(err)
	}
	wantLifecycle := []string{
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session",
		"--bind", "/", "/", "--proc", "/proc", "--block-fd", "3", "--info-fd", "4", "--", wantNative,
	}
	wantPrefix := append([]string{wantBwrap}, wantLifecycle...)
	if len(cmd.Args) < len(wantPrefix) || !reflect.DeepEqual(cmd.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("lifecycle prefix=%v, want exact %v", cmd.Args, wantPrefix)
	}
	foundNative := false
	for _, arg := range cmd.Args {
		if arg == wantNative {
			foundNative = true
		}
		if arg == fixture.paths[PublicationExecutableLogicalEntry] || arg == "node" || filepath.Base(arg) == "node" {
			t.Fatalf("protected launch traverses logical JS/Node entry: %v", cmd.Args)
		}
	}
	if !foundNative {
		t.Fatalf("pinned lifecycle wrapper does not directly execute native Codex: %v", cmd.Args)
	}
	manifest := fixture.boundary.Manifest()
	if got, want := manifest.ProbeArgvSHA256, hashArgv(append(append([]string(nil), wantLifecycle...), fixture.opts.ProbeFixedArgs...)); got != want {
		t.Fatalf("probe argv digest=%q, want literal-vector digest %q", got, want)
	}
	if got, want := manifest.ExecArgvSHA256, hashArgv(append(append([]string(nil), wantLifecycle...), fixture.opts.ExecFixedArgs...)); got != want {
		t.Fatalf("exec argv digest=%q, want literal-vector digest %q", got, want)
	}
	if got, want := manifest.CommandArgvSHA256, hashArgv(append(append([]string(nil), wantLifecycle...), fixture.opts.CommandFixedArgs...)); got != want {
		t.Fatalf("command argv digest=%q, want literal-vector digest %q", got, want)
	}
	wantBootstrap := "PATH=" + fixture.boundary.bootstrapPath
	found := false
	for _, entry := range cmd.Env {
		if entry == wantBootstrap {
			found = true
		}
		if strings.HasPrefix(entry, "PATH=") && entry != wantBootstrap {
			t.Fatalf("bootstrap PATH=%q, want closed %q", entry, wantBootstrap)
		}
	}
	if !found {
		t.Fatalf("protected launch env lacks closed bootstrap %q: %v", wantBootstrap, cmd.Env)
	}
	entries, err := os.ReadDir(fixture.boundary.bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bwrap" {
		t.Fatalf("bootstrap entries=%v, want exactly the pinned bwrap entry", entries)
	}
}

func TestPublicationLifecycleArgvHashHasAnIndependentLiteralOracle(t *testing.T) {
	prefix := []string{
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session",
		"--bind", "/", "/", "--proc", "/proc", "--block-fd", "3", "--info-fd", "4", "--",
		"/opt/no-mistakes/pinned/codex-native",
	}
	cases := []struct {
		name    string
		actual  []string
		literal []string
		want    string
	}{
		{"probe", productionPublicationProbeFixedArgs(), []string{"sandbox"}, "377e6bbc68f6ac6835c2387079892d0040d9a6a50c6548b6b97cc0c040f78525"},
		{"exec", productionPublicationExecFixedArgs(), []string{
			"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--strict-config", "--json", "--color", "never",
			"-c", "project_doc_max_bytes=0", "-c", "mcp_servers={}", "-c", `approval_policy="never"`,
			"-c", `web_search="disabled"`, "-c", "features.apps=false", "-c", "features.auth_elicitation=false",
			"-c", "features.browser_use=false", "-c", "features.browser_use_external=false",
			"-c", "features.browser_use_full_cdp_access=false", "-c", "features.computer_use=false",
			"-c", "features.enable_mcp_apps=false", "-c", "features.in_app_browser=false",
			"-c", "features.plugins=false", "-c", "features.plugin_sharing=false", "-c", "features.remote_plugin=false",
		}, "8ad1c7df99c519978cced5dc8313fbcefd4e391e532393698b3b1475f32ac689"},
		{"command", productionPublicationCommandFixedArgs(), []string{"sandbox"}, "377e6bbc68f6ac6835c2387079892d0040d9a6a50c6548b6b97cc0c040f78525"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.actual, test.literal) {
				t.Fatalf("production %s argv=%q, want exact literal %q", test.name, test.actual, test.literal)
			}
			if got := hashArgv(append(append([]string(nil), prefix...), test.actual...)); got != test.want {
				t.Fatalf("canonical argv hash=%q, want externally frozen %q", got, test.want)
			}
		})
	}
}

func TestPublicationCodexViewBindsExactPathsAndRejectsSchemaEscapes(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	bind := func(root string) *PublicationCodexView {
		t.Helper()
		candidate := filepath.Join(root, "candidate")
		scratch := filepath.Join(root, "scratch")
		for _, dir := range []string{candidate, scratch} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		view, err := fixture.boundary.BindView(candidate, t.TempDir(), scratch)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	first := bind(filepath.Join(t.TempDir(), "first"))
	second := bind(filepath.Join(t.TempDir(), "second"))
	if first.PolicySHA256() == second.PolicySHA256() {
		t.Fatal("two different candidate/scratch bindings produced the same policy hash")
	}
	for _, path := range []string{first.CandidateDir(), first.SourceDir(), first.ScratchDir()} {
		if !strings.Contains(strings.Join(first.profileArgs, "\n"), path) {
			t.Fatalf("bound profile does not name exact path %q: %v", path, first.profileArgs)
		}
	}

	schema := filepath.Join(first.ScratchDir(), "schema.json")
	if err := os.WriteFile(schema, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AgentCommand(context.Background(), schema); err != nil {
		t.Fatalf("schema inside exact scratch rejected: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AgentCommand(context.Background(), outside); !errors.Is(err, ErrPublicationConfinementUnavailable) {
		t.Fatalf("outside-scratch schema error=%v, want confinement_unavailable", err)
	}
	link := filepath.Join(first.ScratchDir(), "linked-schema.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AgentCommand(context.Background(), link); !errors.Is(err, ErrPublicationConfinementUnavailable) {
		t.Fatalf("symlink schema error=%v, want confinement_unavailable", err)
	}
}

func TestPublicationCodexViewKeepsCredentialsOutOfProbeAndCommandBootstrap(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	container := t.TempDir()
	candidate := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	for _, dir := range []string{candidate, scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	view, err := fixture.boundary.BindView(candidate, t.TempDir(), scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "must-not-reach-model-free-launches")
	operatorCodexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", operatorCodexHome)
	operatorHome := filepath.Join(t.TempDir(), "operator-home")
	t.Setenv("HOME", operatorHome)
	for name, build := range map[string]func() (*exec.Cmd, error){
		"probe": func() (*exec.Cmd, error) {
			cmd, err := view.probeCommand(context.Background(), []string{"/bin/true"})
			if err != nil {
				return nil, err
			}
			return cmd.Cmd, nil
		},
		"command": func() (*exec.Cmd, error) {
			cmd, err := view.SandboxCommand(context.Background(), []string{"/bin/true"})
			if err != nil {
				return nil, err
			}
			return cmd.Cmd, nil
		},
	} {
		cmd, err := build()
		if err != nil {
			t.Fatalf("%s command: %v", name, err)
		}
		foundPrivateHome := false
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, "OPENAI_API_KEY=") || entry == "CODEX_HOME="+operatorCodexHome || entry == "HOME="+operatorHome {
				t.Fatalf("%s bootstrap inherited a provider credential: %q", name, entry)
			}
			if entry == "HOME="+view.configHome.path || entry == "CODEX_HOME="+view.configHome.path {
				foundPrivateHome = true
			}
		}
		if !foundPrivateHome {
			t.Fatalf("%s bootstrap does not use the private config home: %v", name, cmd.Env)
		}
		separator := -1
		for index, arg := range cmd.Args {
			if arg == "--" {
				separator = index
			}
		}
		if separator < 0 || separator+1 >= len(cmd.Args) || cmd.Args[separator+1] != "/bin/true" {
			t.Fatalf("%s payload is not behind a fixed -- boundary: %v", name, cmd.Args)
		}
	}
}

func TestPublicationCodexViewRefusesPolicyPathAndConfigHomeDriftBeforeLaunch(t *testing.T) {
	newView := func(t *testing.T) (*PublicationCodexView, string) {
		t.Helper()
		fixture := newPublicationBoundaryFixture(t)
		container := t.TempDir()
		candidate := filepath.Join(container, "candidate")
		scratch := filepath.Join(container, "scratch")
		for _, dir := range []string{candidate, scratch} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		view, err := fixture.boundary.BindView(candidate, t.TempDir(), scratch)
		if err != nil {
			t.Fatal(err)
		}
		return view, view.configHome.path
	}

	t.Run("rendered argv", func(t *testing.T) {
		view, _ := newView(t)
		view.profileArgs[0] = "--mutated-policy"
		if _, err := view.AgentCommand(context.Background(), ""); !errors.Is(err, ErrPublicationConfinementUnavailable) {
			t.Fatalf("mutated rendered policy error=%v, want confinement_unavailable", err)
		}
	})
	t.Run("directory identity", func(t *testing.T) {
		view, _ := newView(t)
		old := view.candidateDir + ".old"
		if err := os.Rename(view.candidateDir, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(view.candidateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := view.SandboxCommand(context.Background(), []string{"/bin/true"}); !errors.Is(err, ErrPublicationConfinementUnavailable) {
			t.Fatalf("replaced candidate directory error=%v, want confinement_unavailable", err)
		}
	})
	t.Run("operator config injection", func(t *testing.T) {
		view, configHome := newView(t)
		if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte("[permissions.publication]\nnetwork.enabled=true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := view.SandboxCommand(context.Background(), []string{"/bin/true"}); !errors.Is(err, ErrPublicationConfinementUnavailable) {
			t.Fatalf("mutated private config home error=%v, want confinement_unavailable", err)
		}
	})
}

func TestPublicationCodexBoundaryRejectsUnenumerableExecutableClosure(t *testing.T) {
	fixture := newPublicationBoundaryFixture(t)
	for _, mutate := range []func(*PublicationCodexBoundaryOptions){
		func(opts *PublicationCodexBoundaryOptions) {
			opts.NativeCodexPath = filepath.Join(t.TempDir(), "missing-native")
		},
		func(opts *PublicationCodexBoundaryOptions) {
			opts.SandboxHelperPath = filepath.Join(t.TempDir(), "missing-helper")
		},
		func(opts *PublicationCodexBoundaryOptions) {
			opts.BubblewrapPath = filepath.Join(t.TempDir(), "missing-bwrap")
		},
	} {
		opts := fixture.opts
		mutate(&opts)
		if _, err := NewPublicationCodexBoundaryV1(context.Background(), opts); !errors.Is(err, ErrPublicationConfinementUnavailable) {
			t.Fatalf("unenumerable closure error=%v, want confinement_unavailable", err)
		}
	}
}

func TestPublicationCodexBoundaryNonLinuxProbeFailsBeforeCanaryLaunch(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux fail-closed contract")
	}
	fixture := newPublicationBoundaryFixture(t)
	container := t.TempDir()
	candidate := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	for _, dir := range []string{candidate, scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	err := fixture.boundary.Probe(context.Background(), PublicationCodexProbeOptions{
		CandidateDir: candidate, SourceDir: t.TempDir(), ScratchDir: scratch,
	})
	if !errors.Is(err, ErrPublicationConfinementUnavailable) {
		t.Fatalf("non-Linux probe error=%v, want confinement_unavailable", err)
	}
	if _, statErr := os.Stat(filepath.Join(scratch, "positive-control")); !os.IsNotExist(statErr) {
		t.Fatal("non-Linux probe launched a canary instead of failing before process effects")
	}
}

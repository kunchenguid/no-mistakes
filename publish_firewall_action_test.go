package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const publishFirewallActionDir = ".github/actions/publish-firewall"

// capturedValues are live-shaped literals planted in the diff, the PR title,
// and the PR body. None of them may reach the action's stdout/stderr, which
// GitHub publishes as a job log on a public repository.
var capturedValues = []string{
	"sw-core-a7.northfabrik.internal",
	"10.42.7.19",
	"00:1b:44:11:3a:b7",
	"FDO24110ABC",
	"nina.oyelaran@northfabrik.co",
	"northfabrik",
}

// checkScriptEnv is one hermetic simulation of the Actions runner check.sh
// executes in: a git repository with a base and a head commit, a pull_request
// event payload, RUNNER_TEMP, GITHUB_OUTPUT, and a PATH carrying only the
// tools the script is allowed to find.
type checkScriptEnv struct {
	dir        string
	binDir     string
	eventPath  string
	outputPath string
	base       string
	head       string
	path       string
}

func TestPublishFirewallAction_IsACompositeActionThatRunsTheScannedCheck(t *testing.T) {
	action := loadRequireAction(t, publishFirewallActionDir)
	if action.Name == "" {
		t.Fatal("action name missing")
	}
	if action.Runs.Using != "composite" {
		t.Fatalf("using=%s, want composite", action.Runs.Using)
	}
	if _, ok := action.Outputs["conclusion"]; !ok {
		t.Fatalf("action must expose a conclusion output, got %v", action.Outputs)
	}
}

// TestPublishFirewallAction_PublicOutputStaysGeneric runs the real check.sh
// against a pull request whose diff, title, and body all carry live-shaped
// values, and asserts that nothing the scanner saw reaches the public job log.
// The private JSON the script asks for is written and never echoed.
func TestPublishFirewallAction_PublicOutputStaysGeneric(t *testing.T) {
	env := newCheckScriptEnv(t)
	privateLeak := "private-json-marker-" + capturedValues[0]
	env.fakeScanner(t, 1, privateLeak)

	stdout, code := env.run(t, map[string]string{
		"NM_PORTAL_URL": "http://portal.invalid:8787",
	})

	if code == 0 {
		t.Fatalf("a violating scan must fail the check, exit=%d output=%q", code, stdout)
	}
	if !strings.Contains(stdout, "publish-policy violation") {
		t.Fatalf("public output must carry the generic phrase, got %q", stdout)
	}
	for _, v := range append(capturedValues, privateLeak, "fixtures/switch_inventory.yaml") {
		if strings.Contains(stdout, v) {
			t.Fatalf("public job log leaked a private value %q; output=%q", v, stdout)
		}
	}
	// Every non-empty public line is either the generic phrase or the portal URL.
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "publish-policy violation" || line == "http://portal.invalid:8787" {
			continue
		}
		t.Fatalf("unexpected public line %q in %q", line, stdout)
	}
	if got := env.stepOutput(t); got["conclusion"] != "failure" {
		t.Fatalf("conclusion=%q, want failure", got["conclusion"])
	}
	// The scanner really was handed the private surfaces, so the absence above
	// is redaction rather than the script never collecting anything.
	args := env.scannerArgs(t)
	for _, flag := range []string{"--diff", "--title-file", "--body-file", "--commits-file", "--private-json"} {
		if !strings.Contains(args, flag) {
			t.Fatalf("check.sh did not pass %s to the scanner: %s", flag, args)
		}
	}
	diff, err := os.ReadFile(filepath.Join(env.dir, "runner-temp", "nm-firewall.diff"))
	if err != nil {
		t.Fatalf("read collected diff: %v", err)
	}
	if !strings.Contains(string(diff), capturedValues[0]) {
		t.Fatalf("collected diff is missing the planted value; check.sh scanned nothing")
	}
	title, err := os.ReadFile(filepath.Join(env.dir, "runner-temp", "nm-firewall.title"))
	if err != nil {
		t.Fatalf("read collected title: %v", err)
	}
	if !strings.Contains(string(title), capturedValues[0]) {
		t.Fatalf("check.sh did not collect the PR title as a scanned surface")
	}
}

// TestPublishFirewallAction_CleanScanPassesWithoutLeaking pins the other half
// of the boundary: a clean verdict must exit 0 and still publish nothing but
// the generic ok line.
func TestPublishFirewallAction_CleanScanPassesWithoutLeaking(t *testing.T) {
	env := newCheckScriptEnv(t)
	env.fakeScanner(t, 0, "")

	stdout, code := env.run(t, nil)
	if code != 0 {
		t.Fatalf("clean scan must pass, exit=%d output=%q", code, stdout)
	}
	if strings.TrimSpace(stdout) != "publish-policy ok" {
		t.Fatalf("clean public output = %q, want the generic ok line only", stdout)
	}
	if got := env.stepOutput(t); got["conclusion"] != "success" {
		t.Fatalf("conclusion=%q, want success", got["conclusion"])
	}
}

// TestPublishFirewallAction_FailsClosed covers the invariant that a publish
// cannot be recalled: when the check cannot actually judge the pull request it
// must fail, never certify, and never explain why in public.
func TestPublishFirewallAction_FailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		scanExit   int
		omitBinary bool
		env        map[string]string
		conclusion string
	}{
		{name: "event payload missing", scanExit: 0, env: map[string]string{"GITHUB_EVENT_PATH": ""}, conclusion: "error"},
		{name: "event payload unreadable", scanExit: 0, env: map[string]string{"GITHUB_EVENT_PATH": "/nonexistent/event.json"}, conclusion: "error"},
		{name: "scanner binary absent", omitBinary: true, conclusion: "error"},
		{name: "base sha unknown to git", scanExit: 0, env: map[string]string{"NM_BASE_SHA": "0000000000000000000000000000000000000000"}, conclusion: "error"},
		{name: "no base or head sha", scanExit: 0, env: map[string]string{"NM_BASE_SHA": "", "NM_HEAD_SHA": ""}, conclusion: "error"},
		{name: "scanner reports a violation", scanExit: 1, conclusion: "failure"},
		// A clean scan the firewall could not record (a refused or unreachable
		// LAN portal) still blocks the merge, but it is an error rather than a
		// Hard Rules verdict the scanner actually reached.
		{name: "scanner cannot record the verdict", scanExit: 2, conclusion: "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCheckScriptEnv(t)
			if !tc.omitBinary {
				env.fakeScanner(t, tc.scanExit, "")
			}
			stdout, code := env.run(t, tc.env)
			if code == 0 {
				t.Fatalf("check must fail closed, exit=0 output=%q", stdout)
			}
			if !strings.Contains(stdout, "publish-policy violation") {
				t.Fatalf("public output = %q, want the generic phrase", stdout)
			}
			for _, v := range capturedValues {
				if strings.Contains(stdout, v) {
					t.Fatalf("fail-closed output leaked %q: %q", v, stdout)
				}
			}
			if got := env.stepOutput(t); got["conclusion"] != tc.conclusion {
				t.Fatalf("conclusion=%q, want %q", got["conclusion"], tc.conclusion)
			}
		})
	}
}

func TestPublishFirewallWorkflow_SelfHostedNoPublicVIP(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/publish-firewall.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Name   string `yaml:"name"`
			RunsOn any    `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	var checkName string
	var labels []string
	for _, job := range wf.Jobs {
		if job.Name != "publish-policy" {
			continue
		}
		checkName = job.Name
		labels = runsOnLabels(t, job.RunsOn)
	}
	if checkName == "" {
		t.Fatal("no job publishes the stable check name publish-policy")
	}
	for _, want := range []string{"self-hosted", "no-mistakes"} {
		if !contains(labels, want) {
			t.Fatalf("publish-policy runs-on=%v, must include %q", labels, want)
		}
	}
	for _, label := range labels {
		if strings.Contains(label, "ubuntu") || strings.Contains(label, "windows") || strings.Contains(label, "macos") {
			t.Fatalf("publish-policy must not use a GitHub-hosted runner, got %v", labels)
		}
	}
}

func TestPublishFirewallCluster_NoPublicVIP(t *testing.T) {
	type k8sObject struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Type string `yaml:"type"`
		} `yaml:"spec"`
	}

	entries, err := os.ReadDir("deploy/no-mistakes")
	if err != nil {
		t.Fatal(err)
	}
	var objects []k8sObject
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("deploy/no-mistakes", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var obj k8sObject
			if err := dec.Decode(&obj); err != nil {
				break
			}
			if obj.Kind == "" {
				continue
			}
			objects = append(objects, obj)
		}
	}
	if len(objects) == 0 {
		t.Fatal("no manifests parsed from deploy/no-mistakes")
	}

	var sawNamespace, sawPortalService bool
	for _, obj := range objects {
		switch obj.Kind {
		case "Namespace":
			if obj.Metadata.Name != "no-mistakes" {
				t.Fatalf("namespace is %q, want no-mistakes", obj.Metadata.Name)
			}
			sawNamespace = true
			continue
		case "Ingress":
			t.Fatalf("%s/%s exposes an Ingress; the portal is LAN-only", obj.Kind, obj.Metadata.Name)
		case "Service":
			if obj.Spec.Type == "LoadBalancer" || obj.Spec.Type == "NodePort" {
				t.Fatalf("Service %s is %s; the portal must stay ClusterIP", obj.Metadata.Name, obj.Spec.Type)
			}
			if obj.Metadata.Name == "no-mistakes-portal" {
				if obj.Spec.Type != "ClusterIP" {
					t.Fatalf("portal Service type=%q, want ClusterIP", obj.Spec.Type)
				}
				sawPortalService = true
			}
		}
		if obj.Metadata.Namespace != "no-mistakes" {
			t.Fatalf("%s/%s is in namespace %q, want no-mistakes", obj.Kind, obj.Metadata.Name, obj.Metadata.Namespace)
		}
	}
	if !sawNamespace {
		t.Fatal("deploy/no-mistakes does not declare the no-mistakes namespace")
	}
	if !sawPortalService {
		t.Fatal("deploy/no-mistakes does not declare the ClusterIP portal Service")
	}
}

func runsOnLabels(t *testing.T, v any) []string {
	t.Helper()
	switch val := v.(type) {
	case string:
		return []string{val}
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				t.Fatalf("runs-on label %v is not a string", item)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("unsupported runs-on shape %T", v)
		return nil
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func newCheckScriptEnv(t *testing.T) *checkScriptEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("check.sh is a POSIX shell script executed on Linux runners")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hermetic PATH: only the tools check.sh is documented to depend on, so
	// a no-mistakes binary installed on the developer's machine cannot satisfy
	// the "scanner binary absent" case.
	for _, tool := range []string{"bash", "git", "python3", "env", "cat", "mkdir", "rm", "sed", "grep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("check.sh needs %s on PATH: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(binDir, tool)); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	if _, err := exec.LookPath("no-mistakes"); err == nil {
		// Guard the hermetic assumption rather than silently trusting PATH.
		if _, err := os.Stat(filepath.Join(binDir, "no-mistakes")); err == nil {
			t.Fatal("hermetic bin dir unexpectedly contains no-mistakes")
		}
	}

	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", ".")
	write(t, filepath.Join(repo, "fixtures", "switch_inventory.yaml"), "devices: []\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	write(t, filepath.Join(repo, "fixtures", "switch_inventory.yaml"), strings.Join([]string{
		"devices:",
		"  - hostname: " + capturedValues[0],
		"    mgmt_ip: " + capturedValues[1],
		"    mac: " + capturedValues[2],
		"    serial: " + capturedValues[3],
		"    owner_email: " + capturedValues[4],
		"",
	}, "\n"))
	git("add", "-A")
	git("commit", "-qm", "import inventory from "+capturedValues[0])
	head := git("rev-parse", "HEAD")

	event := map[string]any{
		"pull_request": map[string]any{
			"number": 4211,
			"title":  "import " + capturedValues[0] + " inventory",
			"body":   "Imported from the lab collector; contact " + capturedValues[4],
			"base":   map[string]any{"sha": base},
			"head":   map[string]any{"sha": head, "ref": "feat/import"},
		},
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(dir, "event.json")
	write(t, eventPath, string(payload))

	if err := os.MkdirAll(filepath.Join(dir, "runner-temp"), 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "github_output")
	write(t, outputPath, "")

	return &checkScriptEnv{
		dir:        dir,
		binDir:     binDir,
		eventPath:  eventPath,
		outputPath: outputPath,
		base:       base,
		head:       head,
		path:       binDir,
	}
}

// fakeScanner installs a `no-mistakes` stub that records its arguments, writes
// the caller-supplied private payload to --private-json, and exits with the
// given code. The stub stands in for the Go scanner, whose own redaction is
// covered by internal/firewall and internal/cli; what is under test here is
// whether the shell wrapper publishes anything it was handed.
func (e *checkScriptEnv) fakeScanner(t *testing.T, exit int, privatePayload string) {
	t.Helper()
	script := `#!/usr/bin/env bash
printf '%s\n' "$*" > "` + filepath.Join(e.dir, "scanner-args.txt") + `"
private=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--private-json" ]; then private="$2"; fi
  shift
done
if [ -n "$private" ] && [ -n "` + privatePayload + `" ]; then
  printf '%s\n' "` + privatePayload + `" > "$private"
fi
if [ ` + strconv.Itoa(exit) + ` -ne 0 ]; then
  printf 'publish-policy violation\n'
  if [ -n "${NM_PORTAL_URL:-}" ]; then printf '%s\n' "${NM_PORTAL_URL}"; fi
  exit ` + strconv.Itoa(exit) + `
fi
printf 'publish-policy ok\n'
`
	path := filepath.Join(e.binDir, "no-mistakes")
	write(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (e *checkScriptEnv) run(t *testing.T, overrides map[string]string) (string, int) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join(publishFirewallActionDir, "check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"HOME":              e.dir,
		"PATH":              e.path,
		"NM_HOME":           filepath.Join(e.dir, "nmhome"),
		"RUNNER_TEMP":       filepath.Join(e.dir, "runner-temp"),
		"GITHUB_EVENT_PATH": e.eventPath,
		"GITHUB_OUTPUT":     e.outputPath,
		"NM_REPO":           "owner/product",
		"NM_PR_NUMBER":      "4211",
		"NM_BASE_SHA":       e.base,
		"NM_HEAD_SHA":       e.head,
		"NM_HEAD_REF":       "feat/import",
	}
	for k, v := range overrides {
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Join(e.dir, "repo")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run check.sh: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

func (e *checkScriptEnv) stepOutput(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(e.outputPath)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

func (e *checkScriptEnv) scannerArgs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.dir, "scanner-args.txt"))
	if err != nil {
		t.Fatalf("scanner was never invoked: %v", err)
	}
	return string(data)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

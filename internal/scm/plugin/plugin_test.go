package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// fakeTransport replaces the plugin process: each call pops the next scripted
// outcome and records the argv the adapter built, plus the content of the
// --body-file while it still exists.
type fakeTransport struct {
	t        *testing.T
	calls    []recordedCall
	outcomes []invocation
}

type recordedCall struct {
	argv     []string
	body     string
	bodyPath string
	bodyMode os.FileMode
}

func (f *fakeTransport) run(_ context.Context, args []string, _ int) (invocation, error) {
	f.t.Helper()
	call := recordedCall{argv: append([]string(nil), args...)}
	for _, arg := range args {
		if path, ok := strings.CutPrefix(arg, "--body-file="); ok {
			info, err := os.Stat(path)
			if err != nil {
				f.t.Fatalf("body file %s missing during the call: %v", path, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				f.t.Fatal(err)
			}
			call.body, call.bodyPath, call.bodyMode = string(data), path, info.Mode().Perm()
		}
	}
	f.calls = append(f.calls, call)
	if len(f.outcomes) == 0 {
		f.t.Fatalf("unexpected plugin invocation %v", args)
	}
	next := f.outcomes[0]
	f.outcomes = f.outcomes[1:]
	return next, nil
}

func (f *fakeTransport) reply(stdout ...string) {
	for _, s := range stdout {
		f.outcomes = append(f.outcomes, invocation{stdout: []byte(s)})
	}
}

func (f *fakeTransport) last() recordedCall {
	f.t.Helper()
	if len(f.calls) == 0 {
		f.t.Fatal("no call recorded")
	}
	return f.calls[len(f.calls)-1]
}

// operationArgv returns the last argv without the trailing context flags and
// --json, which every call carries and TestStatus_ArgvShape pins.
func (f *fakeTransport) operationArgv() []string {
	f.t.Helper()
	argv := f.last().argv
	cut := len(argv) - len(testContextArgs) - 1
	if cut < 0 || !reflect.DeepEqual(argv[cut:], append(append([]string(nil), testContextArgs...), "--json")) {
		f.t.Fatalf("argv does not end with the context flags and --json: %q", argv)
	}
	var out []string
	for _, arg := range argv[:cut] {
		if strings.HasPrefix(arg, "--body-file=") {
			arg = "--body-file=<file>"
		}
		out = append(out, arg)
	}
	return out
}

func (f *fakeTransport) wantArgv(want ...string) {
	f.t.Helper()
	if got := f.operationArgv(); !reflect.DeepEqual(got, want) {
		f.t.Fatalf("argv = %q, want %q", got, want)
	}
}

var testContextArgs = []string{
	"--plugin=ssm",
	"--repo=team/repo",
	"--host=git.example.com",
	"--raw-host=git.example.com",
	"--remote-url=https://git.example.com/team/repo.git",
}

const statusAllCaps = `{"protocol_version":1,"capabilities":{"mergeable_state":true,"failed_check_logs":true,"merged_proof":true,"set_pr_base_branch":true},"max_pr_body_chars":0}`

const statusNoCaps = `{"protocol_version":1,"max_pr_body_chars":0}`

func newTestHost(t *testing.T, opts Options) (*Host, *fakeTransport) {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "ssm"
	}
	if opts.Executable == "" {
		opts.Executable = "nm-ssm"
	}
	if opts.CommandFactory == nil {
		opts.CommandFactory = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			t.Fatal("test host must not spawn a process")
			return nil
		}
	}
	if opts.Repository == (Repository{}) {
		opts.Repository = Repository{RemoteURL: "https://git.example.com/team/repo.git", Host: "git.example.com", RawHost: "git.example.com", Path: "team/repo"}
	}
	if opts.BodyDir == "" {
		opts.BodyDir = t.TempDir()
	}
	host := New(opts)
	transport := &fakeTransport{t: t}
	host.run = transport.run
	return host, transport
}

func availableHost(t *testing.T, status string, opts Options) (*Host, *fakeTransport) {
	t.Helper()
	host, transport := newTestHost(t, opts)
	transport.reply(status)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	return host, transport
}

func testPR() *scm.PR {
	return &scm.PR{Number: "7", URL: "https://git.example.com/team/repo/pulls/7", HeadSHA: "abc123"}
}

func TestStatus_ArgvShapeAndCapabilities(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	transport.reply(`{"protocol_version":1,"capabilities":{"mergeable_state":true,"merged_proof":true},"max_pr_body_chars":5000}`)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	want := append(append([]string{"status"}, testContextArgs...), "--json")
	if got := transport.last().argv; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	caps := host.Capabilities()
	if !caps.MergeableState || caps.FailedCheckLogs || !caps.MergedProof {
		t.Fatalf("capabilities = %+v", caps)
	}
	if host.MaxPRBodyChars() != 5000 {
		t.Fatalf("MaxPRBodyChars = %d", host.MaxPRBodyChars())
	}
	if got := scm.HostMaxPRBodyChars(host); got != 5000 {
		t.Fatalf("HostMaxPRBodyChars = %d", got)
	}
	if host.Provider() != scm.PluginProvider("ssm") {
		t.Fatalf("Provider = %q", host.Provider())
	}
}

func TestStatus_FailsClosed(t *testing.T) {
	tests := []struct {
		name         string
		stdout       string
		runErr       error
		wantErr      string
		wantProtocol bool
	}{
		{name: "protocol mismatch", stdout: `{"protocol_version":2}`, wantErr: "speaks protocol version 2", wantProtocol: true},
		{name: "missing protocol version", stdout: `{}`, wantErr: "speaks protocol version 0", wantProtocol: true},
		{name: "legacy result envelope", stdout: `{"result":{"protocol_version":1}}`, wantErr: "speaks protocol version 0", wantProtocol: true},
		{name: "negative body limit", stdout: `{"protocol_version":1,"max_pr_body_chars":-1}`, wantErr: "negative max_pr_body_chars", wantProtocol: true},
		// A missing limit must not decode to the zero that means unlimited.
		{name: "missing body limit", stdout: `{"protocol_version":1}`, wantErr: "did not include max_pr_body_chars", wantProtocol: true},
		{name: "null body limit", stdout: `{"protocol_version":1,"max_pr_body_chars":null}`, wantErr: "did not include max_pr_body_chars", wantProtocol: true},
		{name: "empty stdout", stdout: "", wantErr: "empty stdout", wantProtocol: true},
		{name: "non-JSON", stdout: "hello", wantErr: "invalid JSON output", wantProtocol: true},
		{name: "trailing document", stdout: statusNoCaps + "\n{}", wantErr: "unexpected data after the JSON document", wantProtocol: true},
		{name: "array document", stdout: `[]`, wantErr: "not a JSON object", wantProtocol: true},
		{name: "null document", stdout: `null`, wantErr: "not a JSON object", wantProtocol: true},
		{name: "exit 0 with error object", stdout: `{"error":{"code":"unauthenticated","message":"run gcloud auth login"}}`, wantErr: "exited 0 but reported an error: run gcloud auth login", wantProtocol: true},
		{name: "plugin refusal", stdout: `{"error":{"code":"unauthenticated","message":"run gcloud auth login"}}`, runErr: errors.New("exit status 1"), wantErr: "run gcloud auth login (unauthenticated)"},
		{name: "plugin unsupported", stdout: `{"error":{"code":"unsupported","message":"no"}}`, runErr: errors.New("exit status 1"), wantErr: "no (unsupported)", wantProtocol: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, transport := newTestHost(t, Options{})
			transport.outcomes = append(transport.outcomes, invocation{stdout: []byte(tt.stdout), runErr: tt.runErr})
			err := host.Available(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Available error = %v, want %q", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrProtocol); got != tt.wantProtocol {
				t.Fatalf("errors.Is(err, ErrProtocol) = %v, want %v (err %v)", got, tt.wantProtocol, err)
			}
			if errors.Is(err, ErrTimeout) {
				t.Fatalf("only a timeout may match ErrTimeout: %v", err)
			}
			if host.Capabilities() != (scm.Capabilities{}) {
				t.Fatalf("failed handshake left capabilities %+v", host.Capabilities())
			}
		})
	}
}

func TestStatus_MissingExecutableNeverSpawns(t *testing.T) {
	host, _ := newTestHost(t, Options{ExecutableAvailable: func(string) bool { return false }})
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), `command "nm-ssm" not found`) || errors.Is(err, ErrProtocol) {
		t.Fatalf("Available error = %v", err)
	}
}

func TestCall_NonZeroExitWithoutFailureDocumentReportsRedactedStderr(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	transport.outcomes = append(transport.outcomes, invocation{
		stdout: []byte("partial output"),
		stderr: "fetch https://user:s3cret@git.example.com/x failed",
		runErr: errors.New("exit status 3"),
	})
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaked a credential: %v", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("a plugin's own failure is not a protocol violation: %v", err)
	}
}

func TestCall_FailureCodesMapToSentinels(t *testing.T) {
	host, transport := availableHost(t, statusAllCaps, Options{})
	transport.outcomes = append(transport.outcomes, invocation{stdout: []byte(`{"error":{"code":"unsupported","message":"no merge queue"}}`), runErr: errors.New("exit status 1")})
	_, err := host.GetMergeableState(context.Background(), testPR())
	if !errors.Is(err, ErrProtocol) || errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("a declared capability refused as unsupported must be a protocol violation, got %v", err)
	}
	transport.outcomes = append(transport.outcomes, invocation{stdout: []byte(`{"error":{"code":"head_changed","message":"moved"}}`), runErr: errors.New("exit status 1")})
	_, err = host.GetMergedProof(context.Background(), testPR(), "abc123")
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("head_changed error = %v", err)
	}
	var pluginErr *Error
	if !errors.As(err, &pluginErr) || pluginErr.Code != "head_changed" || pluginErr.Operation != CmdPRMerged {
		t.Fatalf("plugin error = %#v", pluginErr)
	}
}

func TestCall_ErrorMessageIsBounded(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	long := strings.Repeat("é", maxErrorMessageBytes)
	payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": long}})
	transport.outcomes = append(transport.outcomes, invocation{stdout: payload, runErr: errors.New("exit status 1")})
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "…(truncated)") {
		t.Fatalf("error = %v", err)
	}
	if len(err.Error()) > maxErrorMessageBytes+200 {
		t.Fatalf("error message is %d bytes", len(err.Error()))
	}
}

func TestCall_OutputOverLimitFails(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	transport.outcomes = append(transport.outcomes, invocation{stdout: []byte(statusNoCaps), exceeded: true})
	if err := host.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "output exceeded") || !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
}

func TestFindPR(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		want    *scm.PR
		wantErr string
	}{
		{name: "none", stdout: `{"pr":null}`},
		{
			name:   "string number",
			stdout: `{"pr":{"number":"7","url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main","head_sha":"abc"}}`,
			want:   &scm.PR{Number: "7", URL: "https://git.example.com/team/repo/pulls/7", HeadSHA: "abc", BaseBranch: "main"},
		},
		{
			name:   "integer number",
			stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`,
			want:   &scm.PR{Number: "7", URL: "https://git.example.com/team/repo/pulls/7", BaseBranch: "main"},
		},
		{name: "explicit null", stdout: `{"pr":null}`},
		// Only an explicit null means "no open PR"; reading {} as one would
		// let the PR step open a duplicate.
		{name: "missing pr key", stdout: `{}`, wantErr: `did not include "pr"`},
		{name: "pr not an object", stdout: `{"pr":7}`, wantErr: "invalid pr"},
		{
			name:   "web host differs from git host, case-insensitive path",
			stdout: `{"pr":{"number":7,"url":"https://web.example.com/Team/Repo/pulls/7","head_branch":"feature","base_branch":"main"}}`,
			want:   &scm.PR{Number: "7", URL: "https://web.example.com/Team/Repo/pulls/7", BaseBranch: "main"},
		},
		{
			name:   "repository under a subpath",
			stdout: `{"pr":{"number":7,"url":"https://git.example.com/forge/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`,
			want:   &scm.PR{Number: "7", URL: "https://git.example.com/forge/team/repo/pulls/7", BaseBranch: "main"},
		},
		{name: "url for another repository", stdout: `{"pr":{"number":7,"url":"https://git.example.com/other/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`, wantErr: `does not name repository "team/repo"`},
		{name: "url naming the repository only as a prefix", stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo2/pulls/7","head_branch":"feature","base_branch":"main"}}`, wantErr: `does not name repository "team/repo"`},
		{name: "wrong head", stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"other","base_branch":"main"}}`, wantErr: `head branch "other"`},
		{name: "wrong base", stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"dev"}}`, wantErr: `targeting "dev"`},
		{name: "url without number", stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls","head_branch":"feature","base_branch":"main"}}`, wantErr: "must end with the PR number 7"},
		{name: "url with other number", stdout: `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/8","head_branch":"feature","base_branch":"main"}}`, wantErr: "must end with the PR number 7"},
		{name: "url with credentials", stdout: `{"pr":{"number":7,"url":"https://tok@git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`, wantErr: "without credentials"},
		{name: "non-http url", stdout: `{"pr":{"number":7,"url":"ssh://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`, wantErr: "http(s) URL"},
		{name: "zero number", stdout: `{"pr":{"number":0,"url":"https://git.example.com/team/repo/pulls/0","head_branch":"feature","base_branch":"main"}}`, wantErr: "positive base-10 integer"},
		{name: "padded number", stdout: `{"pr":{"number":"07","url":"https://git.example.com/team/repo/pulls/07","head_branch":"feature","base_branch":"main"}}`, wantErr: "positive base-10 integer"},
		{name: "missing number", stdout: `{"pr":{"url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`, wantErr: "PR number is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, transport := newTestHost(t, Options{})
			transport.reply(tt.stdout)
			got, err := host.FindPR(context.Background(), "feature", "main")
			transport.wantArgv("pr", "find", "--head=feature", "--base=main")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, ErrProtocol) {
					t.Fatalf("FindPR error = %v, want protocol error %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindPR: %v", err)
			}
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("FindPR = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFindPR_AnyBaseOmitsBase(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"dev"}}`)
	pr, err := host.FindPR(context.Background(), "feature", "")
	if err != nil || pr == nil || pr.BaseBranch != "dev" {
		t.Fatalf("FindPR = %+v, %v", pr, err)
	}
	transport.wantArgv("pr", "find", "--head=feature")
}

func TestCreatePR_PassesBodyThroughAPrivateFileAndChecksIdentity(t *testing.T) {
	host, transport := availableHost(t, statusNoCaps, Options{DraftPullRequests: true})
	transport.reply(`{"pr":{"number":3,"url":"https://git.example.com/team/repo/pulls/3","head_branch":"feature","base_branch":"main"}}`)
	body := "## Intent\n--- a/file\n-not a flag\n"
	pr, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "-feat: x", Body: body})
	if err != nil || pr.Number != "3" {
		t.Fatalf("CreatePR = %+v, %v", pr, err)
	}
	transport.wantArgv("pr", "create", "--head=feature", "--base=main", "--title=-feat: x", "--draft", "--body-file=<file>")
	call := transport.last()
	if call.body != body {
		t.Fatalf("body file content = %q, want %q", call.body, body)
	}
	if runtime.GOOS != "windows" && call.bodyMode != 0o600 {
		t.Fatalf("body file mode = %v, want 0600", call.bodyMode)
	}
	if _, err := os.Stat(call.bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file %s survived the call: %v", call.bodyPath, err)
	}
	for _, arg := range call.argv {
		if strings.Contains(arg, "Intent") {
			t.Fatalf("PR body leaked into argv: %q", call.argv)
		}
	}

	transport.reply(`{"pr":{"number":4,"url":"https://git.example.com/team/repo/pulls/4","head_branch":"feature","base_branch":"dev"}}`)
	if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), `expected "feature" -> "main"`) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("mismatched create error = %v", err)
	}
	transport.reply(`{}`)
	if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "did not include a pr object") {
		t.Fatalf("missing pr error = %v", err)
	}
	transport.outcomes = append(transport.outcomes, invocation{stdout: []byte(`{"error":{"message":"boom"}}`), runErr: errors.New("exit status 1")})
	if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"}); err == nil {
		t.Fatal("failed create accepted")
	}
	if _, err := os.Stat(transport.last().bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file survived a failed call: %v", err)
	}
}

func TestCreateAndUpdatePR_EnforceDeclaredBodyLimitWithoutInvoking(t *testing.T) {
	host, _ := availableHost(t, `{"protocol_version":1,"max_pr_body_chars":5}`, Options{})
	if _, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "123456"}); err == nil || !strings.Contains(err.Error(), "max_pr_body_chars 5") {
		t.Fatalf("CreatePR error = %v", err)
	}
	if _, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Body: "123456"}); err == nil || !strings.Contains(err.Error(), "max_pr_body_chars 5") {
		t.Fatalf("UpdatePR error = %v", err)
	}
}

func TestUpdatePR(t *testing.T) {
	host, transport := availableHost(t, statusNoCaps, Options{})
	transport.reply(`{"pr":{"number":"7","url":"https://git.example.com/team/repo/pulls/7","head_branch":"feature","base_branch":"main"}}`)
	if _, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Body: "new body"}); err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
	transport.wantArgv("pr", "update", "7", "--body-file=<file>")
	if transport.last().body != "new body" {
		t.Fatalf("body = %q", transport.last().body)
	}
	transport.reply(`{"pr":{"number":"7","url":"https://git.example.com/team/repo/pulls/7"}}`)
	if _, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Title: "new title", Body: ""}); err != nil {
		t.Fatalf("UpdatePR with title: %v", err)
	}
	transport.wantArgv("pr", "update", "7", "--title=new title", "--body-file=<file>")
	if transport.last().body != "" {
		t.Fatalf("empty body file content = %q", transport.last().body)
	}
	transport.reply(`{"pr":{"number":"8","url":"https://git.example.com/team/repo/pulls/8"}}`)
	if _, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Body: "b"}); err == nil || !strings.Contains(err.Error(), `returned PR "8", expected PR "7"`) {
		t.Fatalf("number mismatch error = %v", err)
	}
	transport.reply(`{"pr":{"number":"7","url":"https://git.example.com/team/repo/pull/7"}}`)
	if _, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Body: "b"}); err == nil || !strings.Contains(err.Error(), "returned PR url") || !errors.Is(err, ErrProtocol) {
		t.Fatalf("url mismatch error = %v", err)
	}
	for _, pr := range []*scm.PR{nil, {}, {Number: "--json"}, {Number: "07"}} {
		if _, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Body: "b"}); err == nil || !strings.Contains(err.Error(), "no pull request identity") {
			t.Fatalf("identity %+v error = %v", pr, err)
		}
	}
}

func TestViewPR_StateContentAndBase(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	pr := `{"number":7,"url":"https://git.example.com/team/repo/pulls/7","base_branch":"main"}`
	for state, want := range map[string]scm.PRState{"open": scm.PRStateOpen, "MERGED": scm.PRStateMerged, "closed": scm.PRStateClosed} {
		transport.reply(`{"pr":` + pr + `,"state":"` + state + `"}`)
		got, err := host.GetPRState(context.Background(), testPR())
		if err != nil || got != want {
			t.Fatalf("GetPRState(%s) = %q, %v", state, got, err)
		}
	}
	transport.wantArgv("pr", "view", "7")
	transport.reply(`{"pr":` + pr + `,"state":"draft"}`)
	if _, err := host.GetPRState(context.Background(), testPR()); err == nil || !strings.Contains(err.Error(), `unknown PR state "draft"`) {
		t.Fatalf("unknown state error = %v", err)
	}

	transport.reply(`{"pr":` + pr + `,"state":"open","title":"t","body":""}`)
	content, err := host.GetPRContent(context.Background(), testPR())
	if err != nil || content.Title != "t" || content.Body != "" {
		t.Fatalf("GetPRContent = %+v, %v", content, err)
	}
	transport.reply(`{"pr":` + pr + `,"state":"open","title":"t"}`)
	if _, err := host.GetPRContent(context.Background(), testPR()); err == nil || !strings.Contains(err.Error(), "both title and body") {
		t.Fatalf("missing body error = %v", err)
	}
	transport.reply(`{"pr":` + pr + `,"state":"open","title":"t","body":null}`)
	if _, err := host.GetPRContent(context.Background(), testPR()); err == nil {
		t.Fatal("null body was accepted")
	}

	transport.reply(`{"pr":` + pr + `,"state":"open"}`)
	if base, err := host.GetPRBaseBranch(context.Background(), testPR()); err != nil || base != "main" {
		t.Fatalf("GetPRBaseBranch = %q, %v", base, err)
	}
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7"},"state":"open"}`)
	if _, err := host.GetPRBaseBranch(context.Background(), testPR()); err == nil || !strings.Contains(err.Error(), "base_branch") {
		t.Fatalf("missing base error = %v", err)
	}
	transport.reply(`{"pr":{"number":9,"url":"https://git.example.com/team/repo/pulls/9"},"state":"open"}`)
	if _, err := host.GetPRState(context.Background(), testPR()); err == nil || !strings.Contains(err.Error(), `expected PR "7"`) {
		t.Fatalf("number mismatch error = %v", err)
	}
	// The same number in the same repository under a different URL is not
	// the PR the run holds; the run would persist the changed URL.
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/merge_requests/7","base_branch":"main"},"state":"open"}`)
	if _, err := host.GetPRState(context.Background(), testPR()); err == nil || !strings.Contains(err.Error(), "returned PR url") || !errors.Is(err, ErrProtocol) {
		t.Fatalf("url mismatch error = %v", err)
	}
	// Without a held URL (a lookup by number alone) only the number binds.
	transport.reply(`{"pr":` + pr + `,"state":"open"}`)
	if _, err := host.GetPRState(context.Background(), &scm.PR{Number: "7"}); err != nil {
		t.Fatalf("GetPRState without a held URL: %v", err)
	}
}

func TestGetChecks(t *testing.T) {
	host, transport := newTestHost(t, Options{})
	transport.reply(`{"checks":[
		{"name":"build","id":"b1","bucket":"pass","state":"SUCCESS","link":"https://ci/1","started_at":"2026-01-02T03:04:05Z","completed_at":"2026-01-02T03:05:05Z","execution_id":"run-1"},
		{"name":"lint","bucket":"FAIL"},
		{"name":"deploy","bucket":"skipping"}
	]}`)
	checks, err := host.GetChecks(context.Background(), testPR())
	if err != nil {
		t.Fatalf("GetChecks: %v", err)
	}
	transport.wantArgv("pr", "checks", "7", "--head-sha=abc123")
	if len(checks) != 3 {
		t.Fatalf("checks = %+v", checks)
	}
	build := checks[0]
	if build.Name != "build" || build.ProviderID != "b1" || build.Bucket != scm.CheckBucketPass || build.Kind != scm.CheckKindRun || build.ExecutionID != "run-1" || build.Link != "https://ci/1" {
		t.Fatalf("build check = %+v", build)
	}
	if !build.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || build.CompletedAt.IsZero() {
		t.Fatalf("build times = %v %v", build.StartedAt, build.CompletedAt)
	}
	if checks[1].Bucket != scm.CheckBucketFail || checks[2].Bucket != scm.CheckBucketSkip {
		t.Fatalf("buckets = %q %q", checks[1].Bucket, checks[2].Bucket)
	}

	transport.reply(`{"checks":[]}`)
	noHead := testPR()
	noHead.HeadSHA = ""
	if checks, err := host.GetChecks(context.Background(), noHead); err != nil || len(checks) != 0 {
		t.Fatalf("empty checks = %+v, %v", checks, err)
	}
	transport.wantArgv("pr", "checks", "7")

	for name, stdout := range map[string]string{
		"missing checks key": `{}`,
		"null checks":        `{"checks":null}`,
		"unknown bucket":     `{"checks":[{"name":"x","bucket":"success"}]}`,
		"missing name":       `{"checks":[{"bucket":"pass"}]}`,
		"bad time":           `{"checks":[{"name":"x","bucket":"pass","started_at":"yesterday"}]}`,
	} {
		transport.reply(stdout)
		if _, err := host.GetChecks(context.Background(), testPR()); err == nil || !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: GetChecks = %v for %s, want a protocol error", name, err, stdout)
		}
	}
}

func TestOptionalCapabilitiesAreGatedByHandshake(t *testing.T) {
	host, _ := availableHost(t, statusNoCaps, Options{})
	ctx := context.Background()
	if _, err := host.GetMergeableState(ctx, testPR()); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("GetMergeableState error = %v", err)
	}
	if _, err := host.FetchFailedCheckLogs(ctx, testPR(), "feature", "abc", []string{"build"}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("FetchFailedCheckLogs error = %v", err)
	}
	if _, err := host.GetMergedProof(ctx, testPR(), "abc"); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("GetMergedProof error = %v", err)
	}
	if err := host.SetPRBaseBranch(ctx, testPR(), "dev"); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("SetPRBaseBranch error = %v", err)
	}
	// The fake transport fails the test on any further invocation.
}

func TestGetMergeableState(t *testing.T) {
	host, transport := availableHost(t, statusAllCaps, Options{})
	for state, want := range map[string]scm.MergeableState{"mergeable": scm.MergeableOK, "conflicting": scm.MergeableConflict, "pending": scm.MergeablePending, "unknown": scm.MergeableUnknown} {
		transport.reply(`{"state":"` + state + `"}`)
		got, err := host.GetMergeableState(context.Background(), testPR())
		if err != nil || got != want {
			t.Fatalf("GetMergeableState(%s) = %q, %v", state, got, err)
		}
	}
	transport.wantArgv("pr", "mergeability", "7")
	transport.reply(`{"state":"clean"}`)
	if _, err := host.GetMergeableState(context.Background(), testPR()); err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("unknown mergeable state = %v", err)
	}
}

func TestFetchFailedCheckLogs(t *testing.T) {
	host, transport := availableHost(t, statusAllCaps, Options{})
	ctx := context.Background()
	if logs, err := host.FetchFailedCheckLogs(ctx, testPR(), "feature", "abc", nil); err != nil || logs != "" {
		t.Fatalf("no failing names = %q, %v", logs, err)
	}
	transport.reply(`{"logs":"boom"}`)
	logs, err := host.FetchFailedCheckLogs(ctx, testPR(), "feature", "abc", []string{"build", "unit tests"})
	if err != nil || logs != "boom" {
		t.Fatalf("logs = %q, %v", logs, err)
	}
	transport.wantArgv("pr", "check-logs", "7", "--failed", "--branch=feature", "--head-sha=abc", "--check=build", "--check=unit tests")
	transport.reply(`{}`)
	if _, err := host.FetchFailedCheckLogs(ctx, testPR(), "feature", "abc", []string{"build"}); err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("missing logs = %v", err)
	}

	big := strings.Repeat("a", maxLogsBytes) + "THE END"
	payload, _ := json.Marshal(map[string]string{"logs": big})
	transport.reply(string(payload))
	logs, err = host.FetchFailedCheckLogs(ctx, testPR(), "feature", "abc", []string{"build"})
	if err != nil {
		t.Fatalf("big logs: %v", err)
	}
	if len(logs) > maxLogsBytes || !strings.HasSuffix(logs, "THE END") || !strings.HasPrefix(logs, "…(log truncated)") {
		t.Fatalf("big logs kept %d bytes, prefix %q", len(logs), logs[:20])
	}
}

func TestGetMergedProof(t *testing.T) {
	host, transport := availableHost(t, statusAllCaps, Options{})
	ctx := context.Background()
	url := testPR().URL
	transport.reply(`{"merged":true,"number":7,"url":"` + url + `","head_sha":"abc123","merge_commit_sha":"m1","merged_at":"2026-01-02T03:04:05Z","merged_by":"alice"}`)
	proof, err := host.GetMergedProof(ctx, testPR(), "abc123")
	if err != nil || !proof.Merged || proof.MergeCommitSHA != "m1" || proof.MergedBy != "alice" || proof.Number != "7" || proof.MergedAt.IsZero() {
		t.Fatalf("proof = %+v, %v", proof, err)
	}
	transport.wantArgv("pr", "merged", "7", "--expected-head=abc123")

	transport.reply(`{"merged":true,"number":7,"url":"` + url + `","head_sha":"other","merge_commit_sha":"m1","merged_at":"2026-01-02T03:04:05Z"}`)
	if _, err := host.GetMergedProof(ctx, testPR(), "abc123"); !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("head mismatch error = %v", err)
	}
	transport.reply(`{"merged":true,"number":7,"url":"` + url + `","head_sha":"abc123"}`)
	if _, err := host.GetMergedProof(ctx, testPR(), "abc123"); err == nil || !strings.Contains(err.Error(), "merge_commit_sha and merged_at") {
		t.Fatalf("incomplete proof error = %v", err)
	}
	transport.reply(`{"merged":false,"number":7,"url":"https://git.example.com/team/repo/pulls/7?x","head_sha":"abc123"}`)
	if _, err := host.GetMergedProof(ctx, testPR(), "abc123"); err == nil || !strings.Contains(err.Error(), "proof identifies") {
		t.Fatalf("url mismatch error = %v", err)
	}
	transport.reply(`{"merged":false,"number":8,"url":"` + url + `","head_sha":"abc123"}`)
	if _, err := host.GetMergedProof(ctx, testPR(), "abc123"); err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("number mismatch = %v", err)
	}
	if _, err := host.GetMergedProof(ctx, testPR(), " "); err == nil || !strings.Contains(err.Error(), "expected head SHA") {
		t.Fatalf("empty expected head error = %v", err)
	}
}

func TestSetPRBaseBranch(t *testing.T) {
	host, transport := availableHost(t, statusAllCaps, Options{})
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","base_branch":"dev"}}`)
	if err := host.SetPRBaseBranch(context.Background(), testPR(), "dev"); err != nil {
		t.Fatalf("SetPRBaseBranch: %v", err)
	}
	transport.wantArgv("pr", "retarget", "7", "--base=dev")
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","base_branch":"main"}}`)
	if err := host.SetPRBaseBranch(context.Background(), testPR(), "dev"); err == nil || !strings.Contains(err.Error(), `now targets "main"`) {
		t.Fatalf("unchanged base error = %v", err)
	}
	transport.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pull/7","base_branch":"dev"}}`)
	if err := host.SetPRBaseBranch(context.Background(), testPR(), "dev"); err == nil || !strings.Contains(err.Error(), "returned PR url") {
		t.Fatalf("url mismatch error = %v", err)
	}
}

func TestKeepTailRespectsRuneBoundaries(t *testing.T) {
	text := strings.Repeat("é", 100)
	got := keepTail(text, 51)
	if len(got) > 51 || !strings.HasPrefix(got, "…(log truncated)\n") {
		t.Fatalf("keepTail = %q (%d bytes)", got, len(got))
	}
	if !strings.HasSuffix(got, "é") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("keepTail split a rune: %q", got)
	}
}

func TestCappedBufferStopsAtLimit(t *testing.T) {
	buf := &cappedBuffer{limit: 4}
	if n, err := buf.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := buf.Write([]byte("cdef")); n != 2 || err == nil || !buf.exceeded {
		t.Fatalf("second write = %d, %v, exceeded=%v", n, err, buf.exceeded)
	}
	if string(buf.Bytes()) != "abcd" {
		t.Fatalf("buffer = %q", buf.Bytes())
	}
}

func TestPrefixBufferNeverFailsWrites(t *testing.T) {
	buf := &prefixBuffer{limit: 3}
	for _, chunk := range []string{"ab", "cd", "ef"} {
		if n, err := buf.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %q = %d, %v", chunk, n, err)
		}
	}
	if buf.String() != "abc" {
		t.Fatalf("buffer = %q", buf.String())
	}
}

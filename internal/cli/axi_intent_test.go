package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A read error with partial text must not launch a run with the partial intent.
// It also detects accidental stdin reads on the absent/string/file paths.
type intentErrorReader struct{ reads int }

func (r *intentErrorReader) Read(p []byte) (int, error) {
	r.reads++
	return copy(p, "partial intent"), errors.New("intent input failed")
}

func writeIntentFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "intent.md")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAxiRunIntentRejectsInputBeforeOpeningResources(t *testing.T) {
	empty := writeIntentFile(t, "")
	blank := writeIntentFile(t, " \t\r\n\u2003")
	missing := filepath.Join(t.TempDir(), "missing")
	invalid := "goal\xff"
	large := strings.Repeat("x", 49123)
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
		want  string
	}{
		{"string invalid UTF-8", []string{"--intent", invalid}, nil, "must be valid UTF-8"},
		{"file invalid UTF-8", []string{"--intent-file", writeIntentFile(t, invalid)}, nil, "must be valid UTF-8"},
		{"stdin invalid UTF-8", []string{"--intent=-"}, strings.NewReader(invalid), "must be valid UTF-8"},
		{"string oversized", []string{"--intent", large}, nil, "exceeds the 49122-byte"},
		{"file oversized", []string{"--intent-file", writeIntentFile(t, large)}, nil, "exceeds the 49122-byte"},
		{"stdin oversized", []string{"--intent=-"}, strings.NewReader(large), "exceeds the 49122-byte"},
		{"file device", []string{"--intent-file", os.DevNull}, nil, "regular file"},
		{"string empty", []string{"--intent="}, nil, "--intent must not be empty"},
		{"string whitespace", []string{"--intent", " \n\u2003"}, nil, "--intent must not be empty"},
		{"file empty path", []string{"--intent-file="}, nil, "--intent-file requires a file path"},
		{"file missing", []string{"--intent-file", missing}, nil, "read --intent-file"},
		{"file directory", []string{"--intent-file", t.TempDir()}, nil, "read --intent-file"},
		{"file empty", []string{"--intent-file", empty}, nil, "--intent-file must not be empty"},
		{"file whitespace", []string{"--intent-file", blank}, nil, "--intent-file must not be empty"},
		{"stdin empty", []string{"--intent", "-"}, strings.NewReader(""), "--intent stdin must not be empty"},
		{"stdin whitespace", []string{"--intent=-"}, strings.NewReader("\n \t\u2003"), "--intent stdin must not be empty"},
		{"stdin read error", []string{"--intent=-"}, &intentErrorReader{}, "read --intent from stdin: intent input failed"},
		{"both", []string{"--intent", "goal", "--intent-file", missing}, nil, "mutually exclusive"},
		{"both empty", []string{"--intent=", "--intent-file="}, nil, "mutually exclusive"},
		{"empty string and file", []string{"--intent=", "--intent-file", empty}, nil, "mutually exclusive"},
		{"string and empty path", []string{"--intent", "goal", "--intent-file="}, nil, "mutually exclusive"},
		{"stdin and file", []string{"--intent-file", missing, "--intent=-"}, nil, "mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "not-created")
			t.Setenv("NM_HOME", home)
			cmd := newAxiRunCmd()
			cmd.SetArgs(tc.args)
			unread := &intentErrorReader{}
			if tc.stdin != nil {
				cmd.SetIn(tc.stdin)
			} else {
				cmd.SetIn(unread)
			}
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("error = %v, output = %s; want usage error %q", err, out.String(), tc.want)
			}
			if unread.reads != 0 {
				t.Fatal("read stdin without selecting stdin (or before rejecting conflicting flags)")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("invalid input opened app resources: stat = %v", err)
			}
		})
	}
}

func TestAxiRunIntentPreservesTransportBytes(t *testing.T) {
	text := " \tuser's \"goal\": `printf MARKER` $HOME $(printf other) \\ café 中文 🚢\r\nsecond line\n\n"
	path := writeIntentFile(t, text)
	// A relative file called '-' is a literal path, not an alias for stdin.
	chdir(t, t.TempDir())
	if err := os.WriteFile("-", []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, intent, file string
		args               []string
		stdin              io.Reader
		want               string
	}{
		{"absent", "", "", nil, nil, ""},
		{"string", text, "", []string{"--intent", text}, nil, text},
		{"file", "", path, []string{"--intent-file", path}, nil, text},
		{"relative dash file", "", "-", []string{"--intent-file", "-"}, nil, text},
		{"stdin", "-", "", []string{"--intent", "-"}, strings.NewReader(text), text},
		{"literal dash via stdin", "-", "", []string{"--intent=-"}, strings.NewReader("-"), "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAxiRunCmd()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			unread := &intentErrorReader{}
			cmd.SetIn(unread)
			if tc.stdin != nil {
				cmd.SetIn(tc.stdin)
			}
			got, err := resolveAxiRunIntent(cmd, tc.intent, tc.file)
			if err != nil || got != tc.want {
				t.Fatalf("resolved = %q, err = %v; want %q", got, err, tc.want)
			}
			if unread.reads != 0 {
				t.Fatal("unexpected stdin read")
			}
			decoded, err := parseIntentPushOptions([]string{formatIntentPushOption(got)})
			if err != nil || decoded != tc.want {
				t.Fatalf("push option = %q, err = %v; want %q", decoded, err, tc.want)
			}
		})
	}
}

func TestAxiRunIntentAbsentStillRequiresIntentForNewRun(t *testing.T) {
	launched := olderDaemonFixture(t, nil)
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	unread := &intentErrorReader{}
	cmd.SetIn(unread)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), "required to start a run") {
		t.Fatalf("missing input = %v\n%s", err, out.String())
	}
	if unread.reads != 0 || len(*launched) != 0 {
		t.Fatalf("missing input read stdin or launched: reads=%d, launches=%v", unread.reads, *launched)
	}
}

func TestAxiRunIntentReattachDoesNotReplaceIntent(t *testing.T) {
	// Only lookup/drive methods exist: any launch or mutation would fail.
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.completed(), nil })
	text := "a different goal\n"
	path := writeIntentFile(t, text)
	for _, args := range [][]string{nil, {"--intent", text}, {"--intent-file", path}, {"--intent", "-"}} {
		cmd := newAxiRunCmd()
		cmd.SetArgs(args)
		unread := &intentErrorReader{}
		cmd.SetIn(unread)
		if len(args) != 0 && args[1] == "-" {
			cmd.SetIn(strings.NewReader(text))
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "run-timeout") || !strings.Contains(out.String(), "outcome: passed") {
			t.Fatalf("reattach %q = %v\n%s", args, err, out.String())
		}
		if unread.reads != 0 {
			t.Fatal("ordinary reattach read unselected stdin")
		}
	}
	// Presence must also be respected when an active run exists.
	var lookups atomic.Int32
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		lookups.Add(1)
		return fx.running(), nil
	})
	cmd := newAxiRunCmd()
	cmd.SetArgs([]string{"--intent="})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || lookups.Load() != 0 {
		t.Fatalf("empty input became a reattach: err=%v, lookups=%d", err, lookups.Load())
	}
}

func TestAxiRunIntentStrictClaimUsesResolvedBytes(t *testing.T) {
	text := "  preserve `code` and $value\n\n"
	claims := make(chan ipc.ClaimLaunchReceiptParams, 3)
	launched := olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
		srv.Handle(ipc.MethodClaimLaunchReceipt, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.ClaimLaunchReceiptParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			claims <- params
			return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{RunID: "strict-run", IntentDigest: params.IntentDigest, Disposition: "reused"}}, nil
		})
		srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "strict-run", Status: types.RunCompleted}}, nil
		})
	})
	writeGlobalConfig(t, "")
	path := writeIntentFile(t, text)
	for _, args := range [][]string{{"--intent", text}, {"--intent-file", path}, {"--intent", "-"}} {
		cmd := newAxiRunCmd()
		cmd.SetArgs(append(args, "--launch-nonce", "request-1", "--validation-generation", "generation-1"))
		cmd.SetIn(strings.NewReader(text))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		claim := <-claims
		if claim.IntentDigest != digestLaunchIntent(text) || claim.LaunchNonce != "request-1" || claim.ValidationGeneration != "generation-1" {
			t.Fatalf("wrong claim: %+v", claim)
		}
	}
	if len(*launched) != 0 {
		t.Fatalf("strict replay launched another run: %v", *launched)
	}
}

// This crosses the real executable, Cobra, local Git push and IPC boundaries.
// A synthetic no-op gate push reaches AXI's existing rerun fallback. The fake
// server records the request and returns a completed run; no agent or forge runs.
func TestNoMistakesBinary_IntentTransport(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	requests := make(chan ipc.RerunParams, 8)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
		srv.Handle(ipc.MethodRerun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.RerunParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			requests <- params
			return &ipc.RerunResult{RunID: "intent-run"}, nil
		})
		srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "intent-run", Status: types.RunCompleted}}, nil
		})
	})
	writeGlobalConfig(t, "")
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := findRepo(d)
	if err != nil {
		t.Fatal(err)
	}
	local, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, local, "clone", "--bare", local, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
	head := cliGit(t, local, "rev-parse", "HEAD")

	run := func(t *testing.T, args []string, stdin, script, want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"axi", "run"}, args...)...)
		if script != "" {
			cmd = exec.CommandContext(ctx, "sh", "-c", script, "intent-test", bin)
		}
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "outcome: passed") {
			t.Fatalf("binary: %v\n%s", err, out)
		}
		select {
		case request := <-requests:
			if request.Intent != want || request.RepoID != repo.ID || request.CallerHeadSHA != head {
				t.Fatalf("request = %+v; want intent %q, repo %q, head %q", request, want, repo.ID, head)
			}
		default:
			t.Fatal("no run request received")
		}
	}
	text := " \tuser's \"goal\": `printf MARKER` $HOME $(printf other) \\ café 中文 🚢\r\nsecond line\n\n"
	path := writeIntentFile(t, text)
	t.Run("argv without shell", func(t *testing.T) { run(t, []string{"--intent", text}, "", "", text) })
	t.Run("file", func(t *testing.T) { run(t, []string{"--intent-file", path}, "", "", text) })
	t.Run("stdin", func(t *testing.T) { run(t, []string{"--intent", "-"}, text, "", text) })
	if runtime.GOOS != "windows" {
		t.Run("caller shell substitution is not CLI evaluation", func(t *testing.T) {
			run(t, nil, "", "\"$1\" axi run --intent \"restore `printf SHELL_MARKER` guard\"", "restore SHELL_MARKER guard")
			run(t, nil, "", "\"$1\" axi run --intent 'restore `printf SHELL_MARKER` guard'", "restore `printf SHELL_MARKER` guard")
		})
	}
	// Even with a valid repo and fake daemon, rejected input cannot change
	// either local/gate refs or send a fallback run request.
	before := cliGit(t, gateDir, "show-ref")
	for _, args := range [][]string{{"--intent-file", path, "--intent="}, {"--intent", "-"}, {"--intent-file", path + ".missing"}, nil} {
		cmd := exec.Command(bin, append([]string{"axi", "run"}, args...)...)
		cmd.Stdin = strings.NewReader("")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("invalid input accepted: %q\n%s", args, out)
		}
		if len(requests) != 0 || cliGit(t, gateDir, "show-ref") != before || cliGit(t, local, "rev-parse", "HEAD") != head {
			t.Fatalf("invalid input mutated run/custody state: %q", args)
		}
	}
}

// This producer never sends EOF. It reports an error after the permitted probe
// byte so an unbounded implementation fails deterministically without an OOM.
type intentProducingReader struct{ bytes int }

func (r *intentProducingReader) Read(p []byte) (int, error) {
	if r.bytes >= 49123 {
		return 0, errors.New("read beyond limit probe")
	}
	n := min(len(p), 49123-r.bytes)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.bytes += n
	return n, nil
}

func TestAxiRunIntentBoundsProducingStdin(t *testing.T) {
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--intent=-"}); err != nil {
		t.Fatal(err)
	}
	r := &intentProducingReader{}
	cmd.SetIn(r)
	got, err := resolveAxiRunIntent(cmd, "-", "")
	if err == nil || !strings.Contains(err.Error(), "exceeds the 49122-byte") || got != "" || r.bytes != 49123 {
		t.Fatalf("got %d bytes, error %v, consumed %d", len(got), err, r.bytes)
	}
}

func TestAxiRunIntentAcceptsBoundaryThroughEOF(t *testing.T) {
	// Include JSON-escaped text and multibyte UTF-8, including the real
	// replacement character (valid UTF-8, unlike a malformed byte sequence).
	text := strings.Repeat("\x00", 49122-len("雪�\n")) + "雪�\n"
	for _, transport := range []string{"string", "file", "stdin"} {
		t.Run(transport, func(t *testing.T) {
			cmd := newAxiRunCmd()
			intent, path := text, ""
			args := []string{"--intent", text}
			if transport == "file" {
				path = writeIntentFile(t, text)
				args = []string{"--intent-file", path}
			} else if transport == "stdin" {
				intent = "-"
				args = []string{"--intent=-"}
				r, w := io.Pipe()
				defer r.Close()
				cmd.SetIn(r)
				go func() {
					// Split a UTF-8 sequence across writes. Only the complete
					// input should be validated, after EOF.
					_, err := io.WriteString(w, text[:len(text)-2])
					if err == nil {
						_, err = io.WriteString(w, text[len(text)-2:])
					}
					w.CloseWithError(err)
				}()
			}
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			got, err := resolveAxiRunIntent(cmd, intent, path)
			if err != nil || got != text {
				t.Fatalf("got %d bytes, error %v; want original %d bytes", len(got), err, len(text))
			}
			// The serialized push-option contract is bounded by Git's
			// packet payload; one extra input byte must exceed that payload.
			if len(formatIntentPushOption(got)) > 65516 || len(formatIntentPushOption(got+"x")) <= 65516 {
				t.Fatal("intent boundary does not match the Git packet payload")
			}
			wire, err := json.Marshal(ipc.RerunParams{Intent: got})
			if err != nil {
				t.Fatal(err)
			}
			var received ipc.RerunParams
			if err := json.Unmarshal(wire, &received); err != nil || digestLaunchIntent(got) != digestLaunchIntent(received.Intent) {
				t.Fatalf("JSON transport changed receipt bytes: %v", err)
			}
		})
	}
}

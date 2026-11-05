package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiRunPRBodyLimitMatchesTheDaemon(t *testing.T) {
	if maxAxiRunPRBodyBytes != steps.MaxRunPRBodyBytes {
		t.Fatalf("CLI PR body limit %d differs from the daemon's %d", maxAxiRunPRBodyBytes, steps.MaxRunPRBodyBytes)
	}
}

func TestAxiRunPRTextRejectsInputBeforeOpeningResources(t *testing.T) {
	large := strings.Repeat("x", maxAxiRunPRBodyBytes+1)
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
		want  string
	}{
		{"title empty", []string{"--pr-title="}, nil, "--pr-title must not be empty"},
		{"title whitespace", []string{"--pr-title", " \t"}, nil, "--pr-title must not be empty"},
		{"title oversized", []string{"--pr-title", strings.Repeat("t", 4097)}, nil, "--pr-title must not exceed 4096 bytes"},
		{"title multi-line", []string{"--pr-title", "one\ntwo"}, nil, "--pr-title must be one line"},
		{"title bidi", []string{"--pr-title", "a‮b"}, nil, "--pr-title must be one line"},
		{"title invalid UTF-8", []string{"--pr-title", "bad\xff"}, nil, "--pr-title must be valid UTF-8"},
		{"body empty path", []string{"--pr-body-file="}, nil, "--pr-body-file requires a file path"},
		{"body missing", []string{"--pr-body-file", filepath.Join(t.TempDir(), "missing")}, nil, "read --pr-body-file"},
		{"body device", []string{"--pr-body-file", os.DevNull}, nil, "regular file"},
		{"body empty file", []string{"--pr-body-file", writeIntentFile(t, "")}, nil, "--pr-body-file must not be empty"},
		{"body whitespace", []string{"--pr-body-file", writeIntentFile(t, " \n\t")}, nil, "--pr-body-file must not be empty"},
		{"body oversized file", []string{"--pr-body-file", writeIntentFile(t, large)}, nil, "exceeds the 49122-byte PR description transport limit"},
		{"body oversized stdin", []string{"--pr-body-file", "-"}, strings.NewReader(large), "exceeds the 49122-byte PR description transport limit"},
		{"body invalid UTF-8", []string{"--pr-body-file", writeIntentFile(t, "bad\xff")}, nil, "--pr-body-file must be valid UTF-8"},
		{"body control", []string{"--pr-body-file", writeIntentFile(t, "a\x1bb")}, nil, "control characters"},
		{"body bidi", []string{"--pr-body-file", writeIntentFile(t, "a⁦b")}, nil, "bidi override or isolate"},
		{"body marker", []string{"--pr-body-file", writeIntentFile(t, "<!-- no-mistakes-pipeline-attestation:v1 {} -->")}, nil, "reserved no-mistakes publication markers"},
		{"both read stdin", []string{"--intent", "-", "--pr-body-file", "-"}, nil, "cannot both read stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "not-created")
			t.Setenv("NM_HOME", home)
			cmd := newAxiRunCmd()
			cmd.SetArgs(append([]string{"--intent-file", writeIntentFile(t, "goal")}, tc.args...))
			if tc.args[0] == "--intent" {
				cmd.SetArgs(tc.args)
			}
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
				t.Fatal("read stdin without selecting it, or before refusing the stdin conflict")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("invalid input opened app resources: stat = %v", err)
			}
		})
	}
}

func TestPRTextPushOptionsRoundTrip(t *testing.T) {
	text := db.RunPRText{Title: "Add `widget` $HOME", Body: "## Our words\r\n\n- café 中文 🚢\n"}
	options := formatPRTextPushOptions(text)
	if len(options) != 2 {
		t.Fatalf("options = %q", options)
	}
	for _, option := range options {
		if strings.ContainsAny(option, "\n\r") {
			t.Fatalf("push option is not one pkt-line: %q", option)
		}
	}
	got, err := parsePRTextPushOptions(append([]string{"no-mistakes.skip=lint"}, options...))
	if err != nil || got != text {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if formatPRTextPushOptions(db.RunPRText{}) != nil {
		t.Fatal("unsupplied PR text formatted push options")
	}
	conflicting := append(options, formatPRTextPushOptions(db.RunPRText{Title: "Other"})...)
	if _, err := parsePRTextPushOptions(conflicting); err == nil {
		t.Fatal("conflicting PR title push options accepted")
	}
}

func TestConflictingActiveRunPRText(t *testing.T) {
	t.Parallel()
	text := db.RunPRText{Title: "Add widget", Body: "Our words.\n"}
	run := &ipc.RunInfo{ID: "run-text", PRTitleDigest: db.PRTextDigest(text.Title, ""), PRBodyDigest: db.PRTextDigest("", text.Body)}
	for _, requested := range []db.RunPRText{{}, text, {Title: text.Title}, {Body: text.Body}} {
		if err := conflictingActiveRunPRText(run, requested); err != nil {
			t.Fatalf("matching %+v should reattach: %v", requested, err)
		}
	}
	for _, requested := range []db.RunPRText{{Title: "Other"}, {Body: "Other words.\n"}} {
		if err := conflictingActiveRunPRText(run, requested); err == nil || !strings.Contains(err.Error(), "run-text") {
			t.Fatalf("different %+v reattached: %v", requested, err)
		}
	}
	if err := conflictingActiveRunPRText(&ipc.RunInfo{ID: "run-plain"}, db.RunPRText{Title: "Add widget"}); err == nil {
		t.Fatal("PR text reattached to a run without it")
	}
}

func TestAxiRunPRTextRefusesOlderDaemon(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func() (interface{}, error)
	}{
		{name: "probe method unknown"},
		{name: "probe declined", probe: func() (interface{}, error) { return &ipc.ProbeRunPRTextResult{OK: false}, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launched := olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				if tc.probe != nil {
					srv.Handle(ipc.MethodProbeRunPRText, func(context.Context, json.RawMessage) (interface{}, error) { return tc.probe() })
				}
			})
			writeGlobalConfig(t, "")
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			err := runAxiRunWithPRText(cmd, false, nil, "goal", "", false, "", "", defaultAxiWait, db.RunPRText{Title: "Add widget"})
			if err == nil || !strings.Contains(out.String(), "too old to honor --pr-title or --pr-body-file") {
				t.Fatalf("older daemon accepted supplied PR text: %v\n%s", err, out.String())
			}
			if len(*launched) != 0 {
				t.Fatalf("run was started on a daemon that would drop the PR text: %v", *launched)
			}
		})
	}
}

func TestAxiRunPRTextStrictClaimBindsTheResolvedText(t *testing.T) {
	body := "  Our `words` and $value\n\n"
	claims := make(chan ipc.ClaimLaunchReceiptParams, 1)
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodProbeRunPRText, func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.ProbeRunPRTextResult{OK: true}, nil
		})
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
	cmd := newAxiRunCmd()
	cmd.SetArgs([]string{"--intent", "goal", "--pr-title", " Add widget ", "--pr-body-file", "-", "--launch-nonce", "request-1", "--validation-generation", "generation-1"})
	cmd.SetIn(strings.NewReader(body))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if claim := <-claims; claim.PRTextDigest != db.PRTextDigest("Add widget", body) {
		t.Fatalf("claim PR text digest = %q", claim.PRTextDigest)
	}
}

func TestAxiRunRejectsPRTextWithSkipPR(t *testing.T) {
	launched := olderDaemonFixture(t, nil)
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetArgs([]string{"--intent", "goal", "--pr-title", "Add widget", "--skip", "pr"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), "--pr-title and --pr-body-file cannot be combined with --skip pr") {
		t.Fatalf("--pr-title with --skip pr = %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("--pr-title with --skip pr launched: %v", *launched)
	}
}

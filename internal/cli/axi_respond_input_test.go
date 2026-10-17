package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiRespondInputRejectsBeforeDaemon(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	blank := writeIntentFile(t, " \n\t")
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		want  string
	}{
		{"instructions conflict", []string{"--instructions=", "--instructions-file", missing}, "", "mutually exclusive"},
		{"finding conflict", []string{"--add-finding=", "--add-finding-file="}, "", "mutually exclusive"},
		{"missing guidance", []string{"--instructions-file", missing}, "", "read --instructions-file"},
		{"missing finding", []string{"--add-finding-file", missing}, "", "read --add-finding-file"},
		{"empty path", []string{"--instructions-file="}, "", "requires a file path"},
		{"blank file", []string{"--instructions-file", blank}, "", "must not be empty"},
		{"empty stdin", []string{"--instructions", "-"}, "", "must not be empty"},
		{"blank stdin finding", []string{"--add-finding", "-"}, " \n", "must not be empty"},
		{"two stdin", []string{"--instructions", "-", "--add-finding", "-"}, "", "only one respond input may read stdin"},
		{"invalid utf8", []string{"--instructions-file", writeIntentFile(t, "\xff")}, "", "must be valid UTF-8"},
		{"nonregular", []string{"--instructions-file", t.TempDir()}, "", "regular file"},
		{"oversized source file", []string{"--instructions-file", writeIntentFile(t, strings.Repeat("x", 49123))}, "", "read --instructions-file: input exceeds the 49122-byte"},
		{"oversized source stdin", []string{"--instructions", "-"}, strings.Repeat("x", 49123), "read --instructions stdin: input exceeds the 49122-byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAxiRespondCmd()
			cmd.SetArgs(append([]string{"--action", "fix", "--findings", "R1"}, tc.args...))
			cmd.SetIn(strings.NewReader(tc.input))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("error %v, output %s; want %q", err, out.String(), tc.want)
			}
		})
	}
}

func TestAxiRespondStdinHonorsWaitWhilePipeRemainsOpen(t *testing.T) {
	for _, flag := range []string{"--instructions", "--add-finding"} {
		t.Run(flag, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()

			cmd := newAxiRespondCmd()
			cmd.SetArgs([]string{"--action", "fix", "--findings", "R1", flag, "-", "--wait", "50ms"})
			cmd.SetIn(reader)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)

			executed := make(chan error, 1)
			go func() { executed <- cmd.Execute() }()
			written := make(chan error, 1)
			go func() {
				_, writeErr := writer.Write([]byte("partial input"))
				written <- writeErr
			}()
			select {
			case writeErr := <-written:
				if writeErr != nil {
					t.Fatalf("write stdin: %v", writeErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stdin writer did not reach the command")
			}

			select {
			case err := <-executed:
				var ee *exitError
				if !errors.As(err, &ee) || ee.code != 1 {
					t.Fatalf("respond error = %v, output %s; want a wait error", err, out.String())
				}
				if !strings.Contains(out.String(), "wait of 50ms elapsed") {
					t.Fatalf("output = %s; want the structured wait timeout", out.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("respond remained blocked on stdin after --wait elapsed")
			}
		})
	}
}

func TestAxiRespondFileExpandedFrameRefused(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdin_%t", stdin), func(t *testing.T) {
			var responded atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{responded: &responded})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })
			// Each '<' becomes six JSON bytes and guidance is repeated for each ID.
			text := strings.Repeat("<", 49122)
			cmd := newAxiRespondCmd()
			args := []string{"--action", "fix", "--findings", "R1,R2,R3,R4"}
			if stdin {
				args = append(args, "--instructions", "-")
			} else {
				args = append(args, "--instructions-file", writeIntentFile(t, text))
			}
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader(text))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := cmd.Execute(); err == nil {
				t.Fatal("oversized respond succeeded")
			}
			for _, want := range []string{"send respond request: encoded IPC frame is", "bytes, exceeds the 1048576-byte limit"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %s", want, out.String())
				}
			}
			if responded.Load() {
				t.Fatal("oversized frame reached daemon")
			}
		})
	}
}

func TestAxiRespondInputPreservedThroughIPC(t *testing.T) {
	guidance := "  keep `backticks` and 'quotes'\nnext line\n"
	finding := "{\"description\":\"fix `literal` and \\\"quotes\\\"\\nnext\",\"action\":\"auto-fix\"}\n"
	for _, tc := range []struct {
		name     string
		file     bool
		guidance bool
		inline   bool
	}{
		{"instructions file", true, true, false}, {"instructions stdin", false, true, false},
		{"finding file", true, false, false}, {"finding stdin", false, false, false},
		{"instructions inline", false, true, true}, {"finding inline", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ipc.RespondParams
			var responded atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				if err := json.Unmarshal(raw, &got); err != nil {
					return nil, err
				}
				responded.Store(true)
				return &ipc.RespondResult{OK: true}, nil
			}})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				if responded.Load() {
					return fx.completed(), nil
				}
				return fx.awaiting(), nil
			})
			flag, text := "--add-finding", finding
			args := []string{"--action", "fix", "--findings", "R1", "--wait", "3s"}
			if tc.guidance {
				flag, text = "--instructions", guidance
			}
			if tc.inline {
				args = append(args, flag, text)
			} else if tc.file {
				path := filepath.Join(t.TempDir(), "input.txt")
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, flag+"-file", path)
			} else {
				args = append(args, flag, "-")
			}
			cmd := newAxiRespondCmd()
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader(text))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("respond: %v\n%s", err, out.String())
			}
			if tc.guidance {
				want := guidance
				if tc.inline {
					want = strings.TrimSpace(want)
				}
				if got.Instructions["R1"] != want {
					t.Fatalf("guidance = %q, want %q", got.Instructions["R1"], want)
				}
			} else if len(got.AddedFindings) != 1 || got.AddedFindings[0].Description != "fix `literal` and \"quotes\"\nnext" {
				t.Fatalf("added findings = %+v", got.AddedFindings)
			}
		})
	}
}

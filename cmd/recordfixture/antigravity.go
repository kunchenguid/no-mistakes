package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// recordAntigravity captures agy CLI's NDJSON stream-json events. The
// fakeagent replays these envelopes while patching only the agent_response
// text delta and the result's response/structured_output, so the recorded
// content just needs to exercise both flavours: one run with --json-schema
// (structured) and one plain text run.
func recordAntigravity(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "agy")

	if err := captureAgy(ctx, bin, forward,
		"Return JSON with field ok set to true",
		filepath.Join(out, "structured.jsonl"),
		[]string{"--json-schema", `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`},
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if err := captureAgy(ctx, bin, forward,
		"Reply with the literal word OK and nothing else.",
		filepath.Join(out, "plain.jsonl"),
		nil,
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "antigravity fixtures written to %s\n", out)
	return 0
}

func captureAgy(ctx context.Context, bin string, forward []string, prompt, outPath string, extraArgs []string) error {
	cmdArgs := make([]string, 0, len(forward)+len(extraArgs)+6)
	cmdArgs = append(cmdArgs, forward...)
	cmdArgs = append(cmdArgs, "--dangerously-skip-permissions", "--print", prompt)
	cmdArgs = append(cmdArgs, extraArgs...)
	cmdArgs = append(cmdArgs, "--output-format", "stream-json")
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	tmp, err := os.MkdirTemp("", "recordagy-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)
	cmd.Dir = tmp

	// Capture into a temporary sibling so a nonzero exit or cancellation
	// cannot leave an existing fixture truncated or half-written; the
	// destination is replaced only after the run and scrub both succeed.
	staging := outPath + ".recording"
	f, err := os.Create(staging)
	if err != nil {
		return fmt.Errorf("create %s: %w", staging, err)
	}
	defer func() {
		f.Close()
		os.Remove(staging)
	}()

	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	shellenv.ConfigureShellCommand(cmd)
	fmt.Fprintf(os.Stderr, "recording agy → %s\n", outPath)
	if err := shellenv.RunShellCommand(cmd); err != nil {
		return fmt.Errorf("run agy: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", staging, err)
	}
	if err := validateAgyCapture(staging); err != nil {
		return fmt.Errorf("validate %s: %w", staging, err)
	}
	if err := scrubFile(staging); err != nil {
		return fmt.Errorf("scrub %s: %w", staging, err)
	}
	if err := os.Rename(staging, outPath); err != nil {
		return fmt.Errorf("publish %s: %w", outPath, err)
	}
	return nil
}

func validateAgyCapture(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var resultCount int
	decoder := json.NewDecoder(f)
	for {
		var envelope struct {
			Event  string `json:"event"`
			Result *struct {
				Status string `json:"status"`
			} `json:"result"`
		}
		err := decoder.Decode(&envelope)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		if envelope.Event != "result" {
			continue
		}
		if envelope.Result == nil {
			return fmt.Errorf("result event is missing result envelope")
		}
		if status := envelope.Result.Status; status != "SUCCESS" {
			return fmt.Errorf("result status %q is not SUCCESS", status)
		}
		resultCount++
	}
	if resultCount == 0 {
		return fmt.Errorf("capture has no result event")
	}
	return nil
}

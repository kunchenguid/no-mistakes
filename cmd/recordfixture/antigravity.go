package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// recordAntigravity captures agy CLI's NDJSON stream-json events. The
// fakeagent replays these envelopes byte-for-byte and patches only the
// agent_response text delta and the result's response/structured_output,
// so the recorded content just needs to exercise both flavours: one run
// with --json-schema (structured) and one plain text run.
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

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer f.Close()

	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "recording agy → %s\n", outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run agy: %w", err)
	}
	if err := scrubFile(outPath); err != nil {
		return fmt.Errorf("scrub %s: %w", outPath, err)
	}
	return nil
}

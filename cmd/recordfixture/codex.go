package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// recordCodex captures codex CLI's JSONL stream. no-mistakes parses the
// final structured response from agent_message text, so we emulate that
// contract by asking codex to emit a JSON literal.
func recordCodex(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "codex")

	// Structured: ask for a JSON object that satisfies what review
	// expects, returned as the agent_message body.
	if err := captureCodex(ctx, bin, forward,
		`Reply with ONLY this JSON literal and nothing else: {"findings": [], "risk_level": "low", "risk_rationale": "no risks", "summary": "ok"}`,
		filepath.Join(out, "structured.jsonl"),
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Plain text.
	if err := captureCodex(ctx, bin, forward,
		"Reply with the literal word OK and nothing else.",
		filepath.Join(out, "plain.jsonl"),
	); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "codex fixtures written to %s\n", out)
	return 0
}

func captureCodex(ctx context.Context, bin string, forward []string, prompt, outPath string) error {
	cmdArgs := []string{
		"exec",
	}
	cmdArgs = append(cmdArgs, forward...)
	cmdArgs = append(cmdArgs,
		prompt,
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
	)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	tmp, err := os.MkdirTemp("", "recordcodex-*")
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
	fmt.Fprintf(os.Stderr, "recording codex → %s\n", outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run codex: %w", err)
	}
	if err := scrubFile(outPath); err != nil {
		return fmt.Errorf("scrub %s: %w", outPath, err)
	}
	return nil
}

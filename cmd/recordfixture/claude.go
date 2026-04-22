package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// recordClaude invokes the real claude CLI with the same flags
// no-mistakes' agent uses, and saves the JSONL stdout to a fixture file.
// We capture two flavours per session: with a JSON schema (review-style)
// and without (commit-summary-style). Both are kept tiny by asking for
// the smallest possible response.
func recordClaude(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "claude")

	// 1) Structured-output flavour. Schema mirrors review's
	// reviewFindingsSchema closely enough that we exercise
	// structured_output / tool_use plumbing without any code in the
	// repo for claude to inspect.
	schema := `{
  "type": "object",
  "properties": {
    "findings": {"type": "array", "items": {"type": "object"}},
    "risk_level": {"type": "string", "enum": ["low","medium","high"]},
    "risk_rationale": {"type": "string"}
  },
  "required": ["findings","risk_level","risk_rationale"]
}`
	prompt := "Reply with structured JSON: empty findings array, risk_level=low, one short risk_rationale."
	if err := captureClaude(ctx, bin, forward, prompt, schema, filepath.Join(out, "structured.jsonl")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// 2) Plain-text flavour. No schema; tests this codepath even though
	// no-mistakes' claude steps always pass a schema today.
	if err := captureClaude(ctx, bin, forward, "Reply with the literal word OK and nothing else.", "", filepath.Join(out, "plain.jsonl")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "claude fixtures written to %s\n", out)
	return 0
}

func captureClaude(ctx context.Context, bin string, forward []string, prompt, schema, outPath string) error {
	cmdArgs := []string{
		"-p", prompt,
		"--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	cmdArgs = append(cmdArgs, forward...)
	if schema != "" {
		cmdArgs = append(cmdArgs, "--json-schema", schema)
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	// Run in a clean tempdir so claude doesn't dredge up project context
	// from CWD.
	tmp, err := os.MkdirTemp("", "recordclaude-*")
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
	fmt.Fprintf(os.Stderr, "recording claude → %s\n", outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run claude: %w", err)
	}
	if err := scrubFile(outPath); err != nil {
		return fmt.Errorf("scrub %s: %w", outPath, err)
	}
	return nil
}

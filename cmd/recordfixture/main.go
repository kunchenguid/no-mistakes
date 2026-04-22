// recordfixture captures real agent CLI output as fixture files for the
// e2e test suite. It is operated by hand, not by CI: every recording
// burns real API quota.
//
// Examples:
//
//	go run ./cmd/recordfixture claude   --out internal/e2e/fixtures/claude
//	go run ./cmd/recordfixture codex    --out internal/e2e/fixtures/codex
//	go run ./cmd/recordfixture opencode --out internal/e2e/fixtures/opencode
//
// Each agent gets a small set of fixture files (one per pipeline-step
// flavour: review with structured output, plain text, etc). The fake
// agent in cmd/fakeagent replays these byte-for-byte at runtime.
//
// The recorder keeps no schema knowledge of its own — it just shells out
// to the real CLI and tees stdout/stderr/SSE/HTTP responses to disk. If
// the real wire format drifts upstream, re-recording produces the new
// fixture and the fake's replay automatically reflects it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}
	agent := os.Args[1]
	args := os.Args[2:]

	out, args, err := parseOut(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", out, err)
		return 1
	}

	switch agent {
	case "claude":
		return recordClaude(ctx, out, args)
	case "codex":
		return recordCodex(ctx, out, args)
	case "opencode":
		return recordOpencode(ctx, out, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown agent %q (want claude|codex|opencode)\n", agent)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: recordfixture <claude|codex|opencode> --out <dir> [--bin <path>]")
	fmt.Fprintln(os.Stderr, "captures real agent output as e2e fixture files. burns real API quota.")
}

// parseOut pulls --out <path> out of args (the only flag the recorder
// owns; everything else is forwarded to the real binary).
func parseOut(args []string) (string, []string, error) {
	out := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--out":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--out: missing value")
			}
			out = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if out == "" {
		return "", nil, fmt.Errorf("--out is required")
	}
	return out, rest, nil
}

// fakeagent is a deterministic stand-in for the real Claude, Codex, and
// OpenCode CLIs used by no-mistakes' e2e tests. One binary is compiled and
// then symlinked under each agent name; argv[0]'s basename selects which
// wire protocol to speak.
//
// All invocations are appended to $FAKEAGENT_LOG (one JSON object per line)
// so tests can assert on exactly which prompts the pipeline issued.
//
// Behaviour is driven by $FAKEAGENT_SCENARIO (a YAML file). When unset the
// agent returns an "all clean" canned response that satisfies every schema
// no-mistakes asks of it.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args))
}

func run(argv []string) int {
	name := agentNameFromArgv0(argv[0])
	args := argv[1:]

	scenario, err := loadScenario(os.Getenv("FAKEAGENT_SCENARIO"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: scenario: %v\n", err)
		return 1
	}

	switch name {
	case "claude":
		return runClaude(args, scenario)
	case "codex":
		return runCodex(args, scenario)
	case "opencode":
		return runOpencode(args, scenario)
	case "gh":
		return runGhStub(args)
	default:
		fmt.Fprintf(os.Stderr, "fakeagent: invoked under unknown name %q (argv[0]=%q)\n", name, argv[0])
		return 2
	}
}

// runGhStub shadows any system-installed gh during e2e so a stray PR/CI
// step can never reach github.com. It fails closed: `gh auth status`
// returns non-zero (so SCM detection treats GitHub as unauthenticated)
// and any other subcommand prints a clear error.
func runGhStub(args []string) int {
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(os.Stderr, "fakeagent gh: not authenticated (e2e stub)")
		return 1
	}
	fmt.Fprintf(os.Stderr, "fakeagent gh: subcommand not implemented in e2e stub: %v\n", args)
	return 1
}

func agentNameFromArgv0(arg0 string) string {
	base := filepath.Base(arg0)
	base = strings.TrimSuffix(base, ".exe")
	return base
}

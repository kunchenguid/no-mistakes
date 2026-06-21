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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	if os.Getenv("FAKEAGENT_GH_MODE") == "fork-pr" {
		return runGhForkPRStub(args)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(os.Stderr, "fakeagent gh: not authenticated (e2e stub)")
		return 1
	}
	fmt.Fprintf(os.Stderr, "fakeagent gh: subcommand not implemented in e2e stub: %v\n", args)
	return 1
}

type ghStubInvocation struct {
	Time string   `json:"time"`
	Args []string `json:"args"`
	Repo string   `json:"repo,omitempty"`
	Head string   `json:"head,omitempty"`
	Base string   `json:"base,omitempty"`
}

func runGhForkPRStub(args []string) int {
	recordGhStubInvocation(args)

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
		head := argAfter(args, "--head")
		if strings.Contains(head, ":") {
			fmt.Fprintln(os.Stderr, `invalid argument: "--head" does not support "<owner>:<branch>"`)
			return 1
		}
		fmt.Println("[]")
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
		repo := argAfter(args, "--repo")
		if repo == "" {
			repo = os.Getenv("FAKEAGENT_GH_PARENT")
		}
		if repo == "" {
			repo = "parent/repo"
		}
		fmt.Printf("https://github.com/%s/pull/99\n", strings.TrimSuffix(repo, ".git"))
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
		if hasArgValue(args, "--json", "state") {
			fmt.Println("MERGED")
			return 0
		}
		if hasArgValue(args, "--json", "mergeable") {
			fmt.Println("MERGEABLE")
			return 0
		}
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "checks" {
		fmt.Println("[]")
		return 0
	}

	fmt.Fprintf(os.Stderr, "fakeagent gh fork-pr: subcommand not implemented: %v\n", args)
	return 1
}

func recordGhStubInvocation(args []string) {
	logPath := os.Getenv("FAKEAGENT_GH_LOG")
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	inv := ghStubInvocation{
		Time: time.Now().Format(time.RFC3339Nano),
		Args: append([]string(nil), args...),
		Repo: argAfter(args, "--repo"),
		Head: argAfter(args, "--head"),
		Base: argAfter(args, "--base"),
	}
	_ = json.NewEncoder(f).Encode(inv)
}

func argAfter(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasArgValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func agentNameFromArgv0(arg0 string) string {
	base := filepath.Base(arg0)
	base = strings.TrimSuffix(base, ".exe")
	return base
}

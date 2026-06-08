package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__fake_gh__" {
		fakeGH(os.Args[2:])
		return
	}

	ctx := context.Background()
	host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"__fake_gh__", name}, args...)...)
		cmd.Dir = "/"
		return cmd
	}, nil, github.RepoSlug("git@github.com:test/repo.git"))

	checks, err := host.GetChecks(ctx, &scm.PR{Number: "123"})
	if err != nil {
		fmt.Printf("GetChecks failed: %v\n", err)
		os.Exit(1)
	}
	state, err := host.GetPRState(ctx, &scm.PR{Number: "123"})
	if err != nil {
		fmt.Printf("GetPRState failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("non-repo cwd: /")
	fmt.Println("resolved repo slug: test/repo")
	fmt.Println("accepted command: gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt")
	fmt.Println("accepted command: gh pr view 123 --repo test/repo --json state --jq .state")
	fmt.Printf("GetChecks returned %d check named %q\n", len(checks), checks[0].Name)
	fmt.Printf("GetPRState returned %q\n", state)
	fmt.Println("fake gh accepted both commands only because --repo test/repo was present")
}

func fakeGH(args []string) {
	wd, _ := os.Getwd()
	joined := strings.Join(args, " ")

	if wd != "/" {
		fmt.Fprintf(os.Stderr, "expected non-repo cwd /, got %s\n", wd)
		os.Exit(1)
	}
	if !containsRepo(args, "test/repo") {
		fmt.Fprintln(os.Stderr, "missing required --repo test/repo")
		os.Exit(1)
	}

	switch joined {
	case "gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt":
		fmt.Println(`[{"name":"build","state":"SUCCESS","bucket":"pass","completedAt":"2026-06-08T00:00:00Z"}]`)
	case "gh pr view 123 --repo test/repo --json state --jq .state":
		fmt.Println("MERGED")
	default:
		fmt.Fprintf(os.Stderr, "unexpected command: %s\n", joined)
		os.Exit(1)
	}
}

func containsRepo(args []string, want string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--repo" && args[i+1] == want {
			return true
		}
	}
	return false
}

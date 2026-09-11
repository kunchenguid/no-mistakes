package main

// Opt-in provider fault injection for the disposable DLOCK31 process journey.
// Git and the candidate daemon stay real; no GitHub request leaves this stub.
import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runGhDLOCK31(args []string) int {
	root := os.Getenv("DLOCK31_PROVIDER")
	log, err := os.OpenFile(filepath.Join(root, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	_ = json.NewEncoder(log).Encode(args)
	_ = log.Close()
	printJSON := func(v any) int { _ = json.NewEncoder(os.Stdout).Encode(v); return 0 }
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }
	if len(args) < 2 {
		return 1
	}
	if args[0] == "auth" {
		return 0
	}
	if args[0] == "pr" {
		switch args[1] {
		case "list":
			if !exists("body") {
				return printJSON([]any{})
			}
			return printJSON([]any{map[string]any{"number": 99, "url": "https://github.com/dlock31/fixture/pull/99", "baseRefName": "main"}})
		case "create", "edit":
			body, _ := io.ReadAll(os.Stdin)
			if args[1] == "edit" && exists("fail") {
				_ = os.WriteFile(filepath.Join(root, "failed-write"), body, 0600)
				fmt.Fprintln(os.Stderr, "DLOCK31 injected attestation write failure")
				return 1
			}
			if err := os.WriteFile(filepath.Join(root, "body"), body, 0600); err != nil {
				return 1
			}
			fmt.Println("https://github.com/dlock31/fixture/pull/99")
			return 0
		case "view":
			switch argAfter(args, "--json") {
			case "title,body":
				body, _ := os.ReadFile(filepath.Join(root, "body"))
				return printJSON(map[string]string{"title": "DLOCK31 fixture", "body": string(body)})
			case "state":
				fmt.Println("OPEN")
				return 0
			case "mergeable":
				fmt.Println("MERGEABLE")
				return 0
			case "baseRefName":
				fmt.Println("main")
				return 0
			case "headRefOid":
				out, err := exec.Command("git", "--git-dir", os.Getenv("DLOCK31_UPSTREAM"), "rev-parse", "refs/heads/feature/dlock31").Output()
				if err != nil {
					return 1
				}
				fmt.Print(string(out))
				return 0
			}
		}
	}
	if args[0] == "api" {
		if strings.Contains(strings.Join(args, " "), "actions/runs") {
			return printJSON([]any{map[string]any{"total_count": 0, "workflow_runs": []any{}}})
		}
		if strings.Contains(strings.Join(args, " "), "statusCheckRollup") {
			state := "FAILURE"
			greenHead, _ := os.ReadFile(filepath.Join(root, "green"))
			if len(greenHead) > 0 && strings.Contains(strings.Join(args, " "), "oid="+string(greenHead)) {
				state = "SUCCESS"
			}
			node := map[string]any{"__typename": "StatusContext", "id": "fixture-check", "context": "fixture-test", "state": state, "targetUrl": "https://example.invalid/check"}
			return printJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"object": map[string]any{"statusCheckRollup": map[string]any{"contexts": map[string]any{"nodes": []any{node}, "pageInfo": map[string]any{"hasNextPage": false}}}}}}})
		}
	}
	if args[0] == "run" && args[1] == "list" {
		return printJSON([]any{})
	}
	fmt.Fprintf(os.Stderr, "DLOCK31 unsupported gh args: %v\n", args)
	return 1
}

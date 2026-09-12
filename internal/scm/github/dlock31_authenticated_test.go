//go:build e2e

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"
)

// TestDLOCK31AuthenticatedRename calls the unchanged candidate Host.FindPR.
// Only CmdFactory's transport changes: gh-axi list + lossless view provide
// actual GitHub PR bytes, and authenticated gh-axi API provides identity bytes.
// The helper converts TOON to JSON; it fabricates no repository/PR facts.
func TestDLOCK31AuthenticatedRename(t *testing.T) {
	old, canonical := os.Getenv("DLOCK31_GITHUB_OLD"), os.Getenv("DLOCK31_GITHUB_CANONICAL")
	if old == "" || canonical == "" {
		t.Skip("opt-in authenticated disposable repository smoke")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var calls [][]string
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "gh" {
			t.Fatalf("unexpected command %s", name)
		}
		calls = append(calls, append([]string(nil), args...))
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestDLOCK31GhAxiTransport$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "DLOCK31_GH_AXI_HELPER=1")
		return cmd
	}, func() bool { return true }, "github.com", old)
	pr, err := host.FindPR(ctx, "fixture/dlock31", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/" + canonical + "/pull/1"
	if pr == nil || pr.Number != "1" || pr.URL != want || pr.BaseBranch != "main" {
		t.Fatalf("FindPR=%+v want %s", pr, want)
	}
	wantCalls := 1
	if old != canonical {
		wantCalls = 3
	}
	if len(calls) != wantCalls {
		t.Fatalf("calls=%v, want %d (list and two identity reads after rename)", calls, wantCalls)
	}
	if old != canonical {
		for i, slug := range []string{old, canonical} {
			wantArgs := "api --hostname github.com repos/" + slug
			if strings.Join(calls[i+1], " ") != wantArgs {
				t.Fatalf("identity request=%v want %s", calls[i+1], wantArgs)
			}
		}
	}
	t.Logf("candidate Host.FindPR accepted canonical live PR: %+v; exact candidate commands: %v", pr, calls)
}

// This subprocess is a gh-axi wire adapter, not a fake provider. It deliberately
// fails on any unexpected command so a new production request cannot be mocked.
func TestDLOCK31GhAxiTransport(t *testing.T) {
	if os.Getenv("DLOCK31_GH_AXI_HELPER") != "1" {
		t.Skip("subprocess only")
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if err := dlock31GhAxi(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func dlock31GhAxi(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	argAfter := func(flag string) string {
		for i, arg := range args {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if len(args) < 2 {
		return fmt.Errorf("missing candidate command")
	}
	if args[0] == "api" {
		out, err := exec.CommandContext(ctx, "gh-axi", "api", args[len(args)-1], "--hostname", argAfter("--hostname"), "--jq", "{id,full_name}", "--full").CombinedOutput()
		if err != nil {
			return fmt.Errorf("authenticated identity GET failed: %w", err)
		}
		var identity struct {
			ID       int64  `toon:"id" json:"id"`
			FullName string `toon:"full_name" json:"full_name"`
		}
		if err := toon.Unmarshal(out, &identity); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(identity)
	}
	if args[0] != "pr" || args[1] != "list" {
		return fmt.Errorf("unsupported candidate command: %v", args)
	}
	repo := argAfter("--repo")
	out, err := exec.CommandContext(ctx, "gh-axi", "pr", "list", "--repo", repo, "--head", argAfter("--head"), "--base", argAfter("--base"), "--state", "open", "--fields", "url", "--limit", "100").CombinedOutput()
	if err != nil {
		return fmt.Errorf("authenticated PR list: %w", err)
	}
	var list struct {
		Count int `toon:"count"`
		PRs   []struct {
			Number int `toon:"number"`
		} `toon:"pull_requests"`
	}
	// gh-axi's human command-help footer is not part of the facts document.
	facts, _, _ := strings.Cut(string(out), "\nhelp[")
	if err := toon.UnmarshalString(facts, &list); err != nil {
		return err
	}
	if list.Count != len(list.PRs) || list.Count >= 100 {
		return fmt.Errorf("incomplete PR list")
	}
	result := []json.RawMessage{}
	for _, pr := range list.PRs {
		body, err := exec.CommandContext(ctx, "gh-axi", "pr", "view", strconv.Itoa(pr.Number), "--repo", repo, "--json", "number,url,baseRefName").Output()
		if err != nil {
			return err
		}
		if !json.Valid(body) {
			return fmt.Errorf("lossless gh-axi view did not return JSON")
		}
		result = append(result, json.RawMessage(body))
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

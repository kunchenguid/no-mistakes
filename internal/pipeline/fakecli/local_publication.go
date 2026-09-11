package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// fakeLocalPublication implements remote state as a temporary file. It never
// invokes git push or a transport; all other Git operations remain real/local.
func fakeLocalPublication(args []string) {
	remoteFile := os.Getenv("FAKE_CLI_REMOTE_HEAD_FILE")
	ref := "refs/heads/feature"
	switch fakeGitSubcommand(args) {
	case "ls-remote":
		head, err := os.ReadFile(remoteFile)
		if err != nil {
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\n", strings.TrimSpace(string(head)), ref)
		os.Exit(0)
	case "push":
		before, err := os.ReadFile(remoteFile)
		if err != nil {
			os.Exit(1)
		}
		lease := "--force-with-lease=" + ref + ":" + strings.TrimSpace(string(before))
		if !slices.Contains(args, lease) {
			fmt.Fprintln(os.Stderr, "fake publication requires exact lease")
			os.Exit(1)
		}
		source, target, ok := strings.Cut(args[len(args)-1], ":")
		if !ok || target != ref || len(source) != 40 {
			os.Exit(1)
		}
		if err := os.WriteFile(remoteFile, []byte(source), 0o600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	fakeGitForward(args, os.Getenv("FAKE_CLI_REAL_GIT"))
}

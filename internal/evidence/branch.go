// Package evidence publishes pipeline test-evidence artifacts to a dedicated
// orphan branch in the same repository.
//
// Evidence used to be committed into the pushed branch, so every merged PR
// carried its screenshots and logs into the default branch's history forever.
// An orphan branch keeps the artifacts in the same repository - same access
// control, binaries welcome, no external host - while sharing no history with
// the code branches, so nothing lands in the PR branch or the default branch.
//
// PR bodies link artifacts by the evidence COMMIT sha rather than the branch
// name, so a link keeps resolving to the exact bytes that run published even
// after later runs overwrite the same paths at the branch tip.
package evidence

import (
	"fmt"
	"strings"
)

// DefaultBranch is the evidence branch used when the config sets no name.
const DefaultBranch = "no-mistakes/evidence"

// MarkerPath is the file every no-mistakes evidence branch carries at its
// root. Publishing refuses to append to an existing branch that does not have
// it, which is what keeps a misconfigured (or hostile) branch name - "main",
// a release branch, someone's feature branch - from receiving evidence
// commits. The check is on content, not on the name, so it holds regardless of
// where the name came from.
const MarkerPath = ".no-mistakes-evidence"

// MarkerContent is written verbatim at MarkerPath. Keep it byte-stable: a
// changed blob would make every publish rewrite the marker.
const MarkerContent = `This branch holds test evidence published by no-mistakes.

It is an orphan branch: it shares no history with the repository's code
branches, and nothing here is part of any release. Pull request bodies link
into it by commit sha so links stay stable.
`

// NormalizeBranch validates a configured evidence branch name and returns the
// name to use. An empty value resolves to DefaultBranch; anything Git would
// reject as a branch ref is an error, so a typo fails the config closed
// instead of failing later inside a run.
//
// The rules mirror git-check-ref-format(1) applied to a branch name, minus the
// checks that only make sense for a full ref path.
func NormalizeBranch(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return DefaultBranch, nil
	}
	if err := validateBranchName(trimmed); err != nil {
		return "", fmt.Errorf("invalid evidence branch name %q: %w", trimmed, err)
	}
	return trimmed, nil
}

func validateBranchName(name string) error {
	if strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("give a branch name, not a full ref")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("must not start with %q", "-")
	}
	if name == "@" || name == "HEAD" {
		return fmt.Errorf("%q is reserved by Git", name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("must not start or end with %q", "/")
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("must not end with %q", ".")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("must not contain %q", "..")
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("must not contain %q", "@{")
	}
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("must not contain control characters")
		case r == ' ':
			return fmt.Errorf("must not contain spaces")
		case strings.ContainsRune("~^:?*[\\", r):
			return fmt.Errorf("must not contain %q", string(r))
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" {
			return fmt.Errorf("must not contain an empty path component")
		}
		if strings.HasPrefix(component, ".") {
			return fmt.Errorf("path component %q must not start with %q", component, ".")
		}
		if strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("path component %q must not end with %q", component, ".lock")
		}
	}
	return nil
}

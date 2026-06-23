// Package cimonitor is the single source of truth for the CI monitor's
// human-facing log vocabulary and how to read monitoring state back out of it.
//
// The CI step (internal/pipeline/steps) emits these exact log lines while it
// watches an open PR. Two very different consumers read them back: the TUI
// renders a live CI panel, and the agent-facing `axi` commands decide when to
// hand control back to the agent. Keeping the strings and the parser here means
// both consumers interpret a run identically and cannot drift apart.
package cimonitor

import "strings"

// Log messages the CI monitor emits. These are matched exactly (not by
// substring) when deriving the "checks passed" state, so the producer and the
// consumers must reference these constants rather than spelling out literals.
const (
	// ChecksPassedMsg is logged when every CI check has passed but the PR is
	// not yet merged or closed, so the monitor keeps watching subject to its
	// configured timeout.
	ChecksPassedMsg = "all CI checks passed - still monitoring until merged or closed"
	// NoChecksPassedMsg is logged when the PR reports no CI checks at all and
	// the monitor keeps watching for a merge or close, subject to its
	// configured timeout.
	NoChecksPassedMsg = "no CI checks reported - still monitoring until merged or closed"
	// ChecksRunningMsg is logged when checks are (re-)running with no failures
	// yet, which clears any previous passed-checks state.
	ChecksRunningMsg = "CI checks running, waiting for results..."
)

// Activity summarizes what the CI step has been doing, derived from its logs.
type Activity struct {
	CIFixes    int    // number of auto-fix attempts observed
	AutoFixing bool   // an auto-fix is currently in progress
	Ready      bool   // checks have passed; the PR is ready for a human to merge
	LastEvent  string // the most recent recognized log line
}

// ParseActivity extracts structured activity from CI log messages.
//
// Ready reflects the most recent monitoring state: it is true only when the
// latest relevant log line announced passed checks (or no checks at all), and
// any newer event clears it.
func ParseActivity(logs []string) Activity {
	var a Activity
	for _, line := range logs {
		switch {
		case strings.Contains(line, "running agent to fix CI"):
			// Emitted once per fix attempt by autoFixCI in real runs (and by
			// demo mode), so it is the reliable signal for counting fixes.
			a.CIFixes++
			a.AutoFixing = true
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "committed and pushed fixes"):
			a.AutoFixing = false
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "CI failures detected"):
			a.AutoFixing = true
			a.Ready = false
			a.LastEvent = line
		case line == ChecksPassedMsg || line == NoChecksPassedMsg:
			a.AutoFixing = false
			a.Ready = true
			a.LastEvent = line
		case strings.Contains(line, "issues detected"),
			strings.Contains(line, "CI checks running"),
			strings.Contains(line, "mergeable state still pending"),
			strings.Contains(line, "warning: could not check CI"),
			strings.Contains(line, "warning: could not check mergeable state"),
			strings.Contains(line, "warning: could not check PR state"):
			a.AutoFixing = false
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "monitoring CI for PR"):
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "PR has been merged"):
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "PR has been closed"):
			a.Ready = false
			a.LastEvent = line
		case strings.Contains(line, "CI timeout"):
			a.Ready = false
			a.LastEvent = line
		}
	}
	return a
}

// ChecksPassed reports whether the CI monitor's latest state is "checks passed,
// PR ready to merge". It is the agent-facing summary of ParseActivity(logs).Ready.
func ChecksPassed(logs []string) bool {
	return ParseActivity(logs).Ready
}

// Package cimonitor is the single source of truth for the CI monitor's
// human-facing log vocabulary and agent-facing monitoring state.
//
// The CI step (internal/pipeline/steps) emits these exact log lines while it
// watches an open PR. Two very different consumers read them back: the TUI
// renders a live CI panel, and the agent-facing `axi` commands decide when to
// hand control back to the agent. Keeping the strings and the parser here means
// both consumers interpret a run identically and cannot drift apart.
//
// Readiness ownership is split deliberately: the pipeline/config owner decides
// whether a trusted default-branch `no_ci: true` declaration applies, and this
// package combines that authoritative state with the activity log for consumers.
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
	// NoChecksPassedMsg is logged when trusted config explicitly declares no CI.
	NoChecksPassedMsg = "repository declares no CI (no_ci: true) - treating as all checks passed - still monitoring until merged or closed"
	// NoChecksConfiguredMsg is logged when the trusted base tree contains no
	// pull-request check configuration for the detected provider.
	NoChecksConfiguredMsg = "repository has zero configured pull-request checks - treating as no CI - still monitoring until merged or closed"
	// ChecksRunningMsg is logged when checks are (re-)running with no failures
	// yet, which clears any previous passed-checks state.
	ChecksRunningMsg = "CI checks running, waiting for results..."
)

// Activity summarizes what the CI step has been doing, derived from its logs.
type Activity struct {
	CIFixes            int    // number of auto-fix attempts observed
	AutoFixing         bool   // an auto-fix is currently in progress
	Ready              bool   // checks passed or no-CI was established; PR ready to merge
	DeclaredNoCI       bool   // Ready because of an explicit no_ci declaration
	NoChecksConfigured bool   // Ready because the trusted base has no PR-check configuration
	LastEvent          string // the most recent recognized log line
}

// FromAuthoritative combines pipeline-owned readiness with CI activity logs.
// Readiness fields from logs are treated as activity only and never override
// the persisted state supplied by the pipeline.
func FromAuthoritative(ready, declaredNoCI bool, logs []string) Activity {
	a := ParseActivity(logs)
	detectedNoChecks := ready && declaredNoCI && a.NoChecksConfigured
	a.Ready = ready
	a.NoChecksConfigured = detectedNoChecks
	a.DeclaredNoCI = ready && declaredNoCI && !detectedNoChecks
	return a
}

// ParseActivity extracts log-derived activity from CI messages. Its readiness
// fields reflect recognized log vocabulary only; consumers use
// FromAuthoritative for the persisted readiness state.
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
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "committed and pushed fixes"):
			a.AutoFixing = false
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "CI failures detected"):
			a.AutoFixing = true
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case line == ChecksPassedMsg:
			a.AutoFixing = false
			a.Ready = true
			a.DeclaredNoCI = false
			a.LastEvent = line
		case line == NoChecksPassedMsg:
			a.AutoFixing = false
			a.Ready = true
			a.DeclaredNoCI = true
			a.LastEvent = line
		case line == NoChecksConfiguredMsg:
			a.AutoFixing = false
			a.Ready = true
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "issues detected"),
			strings.Contains(line, "CI checks running"),
			strings.Contains(line, "mergeable state still pending"),
			strings.Contains(line, "no CI checks reported"),
			strings.Contains(line, "waiting for checks to register"),
			strings.Contains(line, "warning: could not check CI"),
			strings.Contains(line, "warning: could not check mergeable state"),
			strings.Contains(line, "warning: could not check PR state"):
			a.AutoFixing = false
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "monitoring CI for PR"):
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "PR has been merged"):
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "PR has been closed"):
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		case strings.Contains(line, "CI timeout"):
			a.Ready = false
			a.DeclaredNoCI = false
			a.LastEvent = line
		}
		if a.LastEvent == line {
			a.NoChecksConfigured = line == NoChecksConfiguredMsg
		}
	}
	return a
}

// ChecksPassed reports whether the CI monitor's latest state is "checks passed,
// PR ready to merge". It is the agent-facing summary of ParseActivity(logs).Ready.
// Ready covers all-green checks and both trusted no-CI paths; use
// DeclaredNoCI and NoChecksConfigured to distinguish their help text.
func ChecksPassed(logs []string) bool {
	return ParseActivity(logs).Ready
}

// DeclaredNoCI reports whether the latest ready state is backed by an explicit
// trusted no_ci declaration rather than observed green checks.
func DeclaredNoCI(logs []string) bool {
	return ParseActivity(logs).DeclaredNoCI
}

// NoChecksConfigured reports whether the latest ready state is backed by
// positive evidence that the trusted base tree configures no PR checks.
func NoChecksConfigured(logs []string) bool {
	return ParseActivity(logs).NoChecksConfigured
}

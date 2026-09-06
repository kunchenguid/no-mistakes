package cli

import (
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// trackAxiSurface records a state-changing axi command (run, respond, abort)
// both as a pageview and as a command event. The pageview gives agent usage
// parity with the human surfaces (the TUI emits /tui, the wizard /wizard) so
// agent and human activity show up the same way in analytics; the command
// event, added alongside rather than replacing the pageview, keeps the
// per-command status and duration. It fires at command entry so the surface is
// recorded even when the command later fails. fields may be nil.
//
// Read-only polling surfaces (axi home/status/logs, status, runs) must not
// call this or trackCommand: agent status loops made those events the
// dominant remote telemetry volume. Mutation and lifecycle surfaces stay
// full-fidelity.
func trackAxiSurface(command, path string, fields telemetry.Fields, fn func() error) error {
	telemetry.Pageview(path, fields)
	return trackCommand(command, fn)
}

func sanitizeAxiTelemetryAction(action string) string {
	action = strings.TrimSpace(action)
	switch types.ApprovalAction(action) {
	case types.ActionApprove, types.ActionFix, types.ActionSkip:
		return action
	default:
		return "invalid"
	}
}

func trackCommand(name string, fn func() error) (err error) {
	return trackCommandStatus(name, func() (string, error) {
		if err := fn(); err != nil {
			return "", err
		}
		return "success", nil
	})
}

func trackCommandStatus(name string, fn func() (string, error)) (err error) {
	start := time.Now()
	status, err := fn()
	telemetry.Track("command", telemetry.Fields{
		"command":     name,
		"status":      commandStatus(status, err),
		"duration_ms": time.Since(start).Milliseconds(),
	})
	return err
}

func commandStatus(status string, err error) string {
	if status != "" {
		return status
	}
	if err != nil {
		return "error"
	}
	return "success"
}

package cli

import (
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// trackAxiSurface records a state-changing axi command (run, respond, abort)
// both as a pageview and as a command event. The pageview gives agent usage
// parity with the human surfaces (the TUI emits /tui, the wizard /wizard) so
// agent and human activity show up the same way in analytics; the command
// event, added alongside rather than replacing the pageview, keeps the
// per-command status and duration. It fires at command entry so the surface is
// recorded even when the command later fails. fields may be nil. Read-only
// surfaces must use trackReadSurface instead: their polling volume made the
// pageview+command pair the dominant source of remote telemetry rows.
func trackAxiSurface(command, path string, fields telemetry.Fields, fn func() error) error {
	telemetry.Pageview(path, fields)
	return trackCommand(command, fn)
}

// readSurfaceHeartbeat caps how long an unchanged read-only surface stays
// silent: with no state change its command event is emitted at most this
// often. State changes always emit immediately (see ReadSurfaceGate).
const readSurfaceHeartbeat = 10 * time.Minute

// readSurfaceNow is the read-surface gate's clock, injectable so tests can
// drive the heartbeat window deterministically. nil means time.Now.
var readSurfaceNow func() time.Time

// trackReadSurface records a high-frequency read-only command (axi-status,
// axi-home, axi-logs, status, runs) as a single sampled "command" event.
// Unlike trackAxiSurface it emits no pageview, and the command event is
// suppressed unless the observed state fingerprint changed since the last
// emit or the heartbeat interval elapsed. This is what keeps agent status
// polling loops from flooding remote analytics (one event per poll, forever)
// while every meaningful state transition still lands remotely. Mutation
// surfaces (axi run/respond/abort, run lifecycle) stay full-fidelity.
// fn returns the state fingerprint, an optional soft status, and the error.
func trackReadSurface(command string, fields telemetry.Fields, fn func() (string, string, error)) error {
	start := time.Now()
	fingerprint, status, err := fn()
	resolved := commandStatus(status, err)
	if !shouldEmitReadSurface(command, resolved+"|"+fingerprint) {
		return err
	}
	merged := make(telemetry.Fields, len(fields)+3)
	for k, v := range fields {
		merged[k] = v
	}
	merged["command"] = command
	merged["status"] = resolved
	merged["duration_ms"] = time.Since(start).Milliseconds()
	telemetry.Track("command", merged)
	return err
}

func shouldEmitReadSurface(command, fingerprint string) bool {
	if !telemetry.Enabled() {
		return false
	}
	p, err := paths.New()
	if err != nil {
		return true // fail open: behave like the ungated pre-diet path
	}
	gate := telemetry.NewReadSurfaceGate(p.TelemetryGateFile(), readSurfaceHeartbeat, readSurfaceNow)
	return gate.ShouldEmit(command, fingerprint)
}

func sanitizeAxiTelemetryStep(step string) string {
	step = strings.TrimSpace(step)
	if validStep(types.StepName(step)) {
		return step
	}
	return "invalid"
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

package quotarouting

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// SwitchStore is the persistence seam for mid-run switch records. *db.DB
// satisfies it; the narrow interface keeps this package importable from the
// pipeline executor without a daemon dependency.
type SwitchStore interface {
	GetRunQuotaRouting(id string) (string, error)
	SetRunQuotaRouting(id, payload string) error
}

// RecordSwitch amends a run's stored record with one mid-run quota switch
// (from the lifecycle event's agent to its NextAgent). The switch itself
// already happened in the agent chain, so any failure here is a logged
// observability gap, never a run failure: the run cannot be un-switched and
// continues on the next agent regardless.
func RecordSwitch(store SwitchStore, runID string, event SwitchEvent) {
	payload, err := store.GetRunQuotaRouting(runID)
	if err != nil {
		slog.Warn("failed to read quota routing record for a quota switch", "run", runID, "error", err)
		return
	}
	record, err := ParseRecord(payload)
	if err != nil {
		slog.Warn("run carries an unreadable quota routing record; quota switch not recorded", "run", runID, "error", err)
		return
	}
	if record == nil {
		// A switch can only fire when the chain was built with the quota
		// switch active, which only happens on an enabled record; a missing
		// record means the run predates the feature or its write was lost.
		slog.Warn("quota switch fired without a quota routing record", "run", runID)
		return
	}
	updated := record.AppendSwitch(types.AgentName(event.From), types.AgentName(event.To), event.Reason, event.At)
	encoded, err := json.Marshal(updated)
	if err != nil {
		slog.Warn("failed to encode the amended quota routing record", "run", runID, "error", err)
		return
	}
	if err := store.SetRunQuotaRouting(runID, string(encoded)); err != nil {
		slog.Warn("failed to record quota switch", "run", runID, "error", err)
	}
}

// SwitchEvent is one mid-run move, as the executor observes it.
type SwitchEvent struct {
	From   string
	To     string
	Reason string
	At     time.Time
}

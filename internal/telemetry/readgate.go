package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// maxReadSurfaceEntries bounds the persisted gate state so an unbounded set of
// surface keys cannot grow the state file forever. Oldest entries are pruned.
const maxReadSurfaceEntries = 64

// ReadSurfaceGate bounds telemetry volume for high-frequency read-only
// commands (axi-status, axi-home, status, runs). It emits when the observed
// state fingerprint changed since the last emit, and otherwise at most once
// per heartbeat interval. State persists in a small JSON file so agent
// polling loops - a fresh CLI process per poll - stay bounded across process
// boundaries. Any state-file failure fails open (emit), degrading to the
// pre-gate behavior instead of dropping meaningful events.
type ReadSurfaceGate struct {
	path     string
	interval time.Duration
	now      func() time.Time
}

// NewReadSurfaceGate creates a gate persisting state at path. now is
// injectable for deterministic tests; nil means time.Now.
func NewReadSurfaceGate(path string, interval time.Duration, now func() time.Time) *ReadSurfaceGate {
	if now == nil {
		now = time.Now
	}
	return &ReadSurfaceGate{path: path, interval: interval, now: now}
}

type readSurfaceEntry struct {
	Fingerprint  string `json:"fingerprint"`
	LastEmitUnix int64  `json:"last_emit_unix"`
}

type readSurfaceState struct {
	Surfaces map[string]readSurfaceEntry `json:"surfaces"`
}

// ShouldEmit reports whether the surface's event should be emitted now, and
// records the emit when it returns true. fingerprint is a low-cardinality
// summary of the observed state; any change emits immediately, an unchanged
// fingerprint emits at most once per interval.
func (g *ReadSurfaceGate) ShouldEmit(surface, fingerprint string) bool {
	if g == nil || g.path == "" {
		return true
	}
	lock, err := acquireReadSurfaceLock(g.path + ".lock")
	if err != nil {
		return true
	}
	defer lock.Close()

	now := g.now()
	state := g.load()
	entry, seen := state.Surfaces[surface]
	if seen && entry.Fingerprint == fingerprint && now.Sub(time.Unix(entry.LastEmitUnix, 0)) < g.interval {
		return false
	}

	state.Surfaces[surface] = readSurfaceEntry{Fingerprint: fingerprint, LastEmitUnix: now.Unix()}
	pruneReadSurfaceState(&state)
	g.save(state)
	return true
}

func (g *ReadSurfaceGate) load() readSurfaceState {
	data, err := os.ReadFile(g.path)
	if err == nil {
		if state, parseErr := parseReadSurfaceState(data); parseErr == nil {
			return state
		}
	}
	return readSurfaceState{Surfaces: map[string]readSurfaceEntry{}}
}

func parseReadSurfaceState(data []byte) (readSurfaceState, error) {
	var state readSurfaceState
	if err := json.Unmarshal(data, &state); err != nil {
		return readSurfaceState{}, err
	}
	if state.Surfaces == nil {
		state.Surfaces = map[string]readSurfaceEntry{}
	}
	return state, nil
}

func pruneReadSurfaceState(state *readSurfaceState) {
	if len(state.Surfaces) <= maxReadSurfaceEntries {
		return
	}
	type keyed struct {
		key   string
		entry readSurfaceEntry
	}
	entries := make([]keyed, 0, len(state.Surfaces))
	for key, entry := range state.Surfaces {
		entries = append(entries, keyed{key: key, entry: entry})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].entry.LastEmitUnix > entries[j].entry.LastEmitUnix
	})
	pruned := make(map[string]readSurfaceEntry, maxReadSurfaceEntries)
	for _, e := range entries[:maxReadSurfaceEntries] {
		pruned[e.key] = e.entry
	}
	state.Surfaces = pruned
}

// save writes atomically via rename so a concurrent reader never sees a
// partial file. Failures are ignored: the worst case is an extra emit.
func (g *ReadSurfaceGate) save(state readSurfaceState) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.path), ".telemetry-gate-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, g.path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

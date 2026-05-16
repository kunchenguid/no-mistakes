package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Result is what Extract returns when it successfully attaches an intent
// to a run. AgentName/SessionID/Score are surfaced for telemetry and DB
// persistence; callers store the Summary onto db.Run.Intent.
type Result struct {
	Summary   string
	AgentName string
	SessionID string
	Score     float64
}

// ExtractParams configures a single Extract call.
type ExtractParams struct {
	// HomeDir overrides the user's home directory. Empty means use os.UserHomeDir.
	HomeDir string
	// OriginCWD is the user's actual repo directory. The caller is responsible
	// for passing the original working path, NOT the no-mistakes worktree.
	OriginCWD string
	// DiffFiles is the set of files changed between base and head, repo-relative.
	DiffFiles []string
	// BaseTime is the committer time of the base SHA.
	BaseTime time.Time
	// HeadTime is the committer time of the head SHA.
	HeadTime time.Time
	// SlackDays extends WindowStart backwards. The plan called for 3 days.
	SlackDays int
	// Threshold is the minimum raw file-overlap score required before applying
	// stricter multi-file and stale-partial acceptance rules.
	Threshold float64
	// Readers are the per-agent transcript readers to consult. Order is
	// insignificant; matching picks the best score across all of them.
	Readers []Reader
	// Cache is consulted before summarization. Pass NewMemCache() if no DB.
	Cache Cache
	// Summarizer turns the chosen session's text into a short summary.
	Summarizer Summarizer
	// Logf receives best-effort candidate diagnostics. Nil disables logging.
	Logf func(format string, args ...any)
}

// ErrNoMatch indicates no agent transcript matched the change. Callers
// should treat this as a normal "no intent attached" outcome, not an error.
var ErrNoMatch = errors.New("intent: no matching transcript")

// Extract runs the discover -> match -> cache -> summarize pipeline and
// returns the final intent. It returns ErrNoMatch when no session satisfies
// the matcher's threshold, overlap, and freshness acceptance rules.
func Extract(ctx context.Context, p ExtractParams) (*Result, error) {
	if p.OriginCWD == "" {
		return nil, fmt.Errorf("intent: OriginCWD is required")
	}
	if len(p.DiffFiles) == 0 {
		return nil, ErrNoMatch
	}
	if p.Cache == nil {
		p.Cache = NewMemCache()
	}
	if p.Summarizer == nil {
		return nil, fmt.Errorf("intent: Summarizer is required")
	}

	slack := time.Duration(maxInt(p.SlackDays, 0)) * 24 * time.Hour
	opts := DiscoverOpts{
		HomeDir:     p.HomeDir,
		OriginCWD:   canonicalPath(p.OriginCWD),
		WindowStart: p.BaseTime.Add(-slack),
		WindowEnd:   p.HeadTime,
	}

	var sessions []*Session
	for _, r := range p.Readers {
		if r == nil {
			continue
		}
		discovered, err := r.Discover(ctx, opts)
		if err != nil {
			slog.Debug("intent reader discover failed", "agent", r.Name(), "error", err)
			continue
		}
		for _, s := range discovered {
			s.AgentName = r.Name()
		}
		sessions = append(sessions, discovered...)
	}

	if len(sessions) == 0 {
		return nil, ErrNoMatch
	}

	// Load message bodies only for sessions that look promising on metadata.
	// At this stage we cannot score yet (need messages), so we load them all.
	// Discover is supposed to keep the candidate set small via the time/cwd
	// filter; if that's true, this is cheap.
	var loaded []*Session
	for _, s := range sessions {
		var reader Reader
		for _, r := range p.Readers {
			if r != nil && r.Name() == s.AgentName {
				reader = r
				break
			}
		}
		if reader == nil {
			continue
		}
		if err := reader.Load(ctx, s); err != nil {
			slog.Debug("intent reader load failed", "agent", s.AgentName, "session", s.SessionID, "error", err)
			continue
		}
		loaded = append(loaded, s)
	}

	match := pickMatchWithOptions(loaded, p.DiffFiles, matchOptions{
		Threshold: p.Threshold,
		HeadTime:  p.HeadTime,
		Logf:      p.Logf,
	})
	if match == nil {
		return nil, ErrNoMatch
	}

	key := cacheKeyFor(match.Session)
	if cached, ok := p.Cache.Get(key); ok && cached != "" {
		return &Result{
			Summary:   cached,
			AgentName: match.Session.AgentName,
			SessionID: match.Session.SessionID,
			Score:     match.Score,
		}, nil
	}

	summary, err := p.Summarizer.Summarize(ctx, match.Session)
	if err != nil {
		return nil, fmt.Errorf("intent: summarize: %w", err)
	}
	p.Cache.Put(key, summary, match.Session.AgentName, match.Session.SessionID)

	return &Result{
		Summary:   summary,
		AgentName: match.Session.AgentName,
		SessionID: match.Session.SessionID,
		Score:     match.Score,
	}, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

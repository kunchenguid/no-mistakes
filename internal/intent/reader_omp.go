package intent

import "context"

// ompReader reads Oh My Pi (omp) coding-agent transcripts from
// ~/.omp/agent/sessions/. OMP is a Pi fork that writes the same session-file
// format, so discovery and parsing reuse the shared pi-format helpers.
type ompReader struct{}

// NewOMPReader returns a Reader for Oh My Pi coding-agent transcripts.
func NewOMPReader() Reader { return &ompReader{} }

func (r *ompReader) Name() string { return OMPReaderName }

func (r *ompReader) Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error) {
	return discoverPiStyleSessions(ctx, opts, OMPReaderName, ".omp", "agent", "sessions")
}

func (r *ompReader) Load(_ context.Context, s *Session) error {
	return loadPiStyleSession(s, OMPReaderName)
}

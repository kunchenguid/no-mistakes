package intent

import (
	"context"
	"time"
)

// Role identifies who produced a transcript message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single user or assistant turn extracted from an agent transcript.
// Tool calls and tool results are deliberately excluded from Text. FilePaths is a
// best-effort list of file paths the agent referenced via tool inputs or quoted
// in assistant text - used purely for matching, not for the summary.
type Message struct {
	Role      Role
	Text      string
	FilePaths []string
	Timestamp time.Time
	// Synthetic marks a message that was inserted by no-mistakes itself
	// (e.g. a "middle messages omitted" notice from clampMessages). The
	// transcript serializer renders these without a role prefix so the
	// downstream LLM does not mistake them for user or assistant turns.
	Synthetic bool
}

// Session is one transcript candidate from one agent.
type Session struct {
	AgentName    string
	SessionID    string
	CWD          string
	StartedAt    time.Time
	LastActivity time.Time
	// LastMsgKey is a per-reader stable identifier for the last message
	// (uuid where available, otherwise a timestamp). Used as the cache key
	// input so repeat extractions on an unchanged session hit the cache.
	LastMsgKey string
	// Messages holds the user/assistant turns. Populated by Reader.Load,
	// not by Discover, since most candidates will be filtered out.
	Messages []Message

	// startedAtPath is a reader-private handle that points to the underlying
	// transcript (a .jsonl file path, a SQLite row id, etc.). The Load
	// implementation is the only consumer.
	startedAtPath string
}

// DiscoverOpts narrows the search down to relevant sessions before any
// expensive body parsing happens.
type DiscoverOpts struct {
	// HomeDir overrides the user's home directory. Empty means use os.UserHomeDir.
	HomeDir string
	// OriginCWD is the user's actual repo directory (NOT the worktree the
	// pipeline runs in). Symlinks should already be resolved by the caller.
	OriginCWD string
	// WindowStart is the earliest LastActivity allowed (inclusive).
	WindowStart time.Time
	// WindowEnd is the latest StartedAt allowed (inclusive).
	WindowEnd time.Time
}

// Reader is implemented by per-agent transcript readers.
type Reader interface {
	Name() string
	// Discover returns candidate sessions matching opts. Implementations must
	// only read enough of each transcript to populate metadata; full message
	// bodies must wait until Load is called.
	Discover(ctx context.Context, opts DiscoverOpts) ([]*Session, error)
	// Load populates s.Messages with user/assistant text only. Tool calls and
	// tool results must be omitted from Message.Text but file paths they
	// reference may be added to Message.FilePaths.
	Load(ctx context.Context, s *Session) error
}

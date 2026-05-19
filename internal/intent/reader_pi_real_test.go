package intent

import (
	"context"
	"os"
	"testing"
)

func TestPiReader_RealLocalSessions(t *testing.T) {
	if os.Getenv("NM_TEST_REAL_PI") != "1" {
		t.Skip("set NM_TEST_REAL_PI=1 to validate against local ~/.pi/agent/sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	sessions, err := NewPiReader().Discover(context.Background(), DiscoverOpts{HomeDir: home})
	if err != nil {
		t.Fatalf("discover real Pi sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("no real Pi sessions discovered")
	}
	loaded := 0
	messages := 0
	for _, s := range sessions {
		if err := NewPiReader().Load(context.Background(), s); err != nil {
			t.Fatalf("load real Pi session %q: %v", s.SessionID, err)
		}
		if len(s.Messages) > 0 {
			loaded++
			messages += len(s.Messages)
		}
	}
	if loaded == 0 {
		t.Fatal("real Pi sessions loaded but produced no user/assistant messages")
	}
	t.Logf("loaded %d/%d real Pi sessions with %d extracted user/assistant messages", loaded, len(sessions), messages)
}

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestReviewerSourceTag(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"", ""},
		{types.FindingSourceAgent, ""},
		{types.FindingSourceUser, ""},
		{"codex", "[codex]"},
		{"claude", "[claude]"},
	}
	for _, tc := range cases {
		if got := reviewerSourceTag(tc.source); got != tc.want {
			t.Errorf("reviewerSourceTag(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

func TestIsUserSource(t *testing.T) {
	if !isUserSource(types.FindingSourceUser) {
		t.Error("expected the user sentinel to be a user source")
	}
	for _, s := range []string{"", types.FindingSourceAgent, "codex", "claude"} {
		if isUserSource(s) {
			t.Errorf("expected %q not to be treated as a user source", s)
		}
	}
}

func TestRenderFindings_ReviewerSourceTags(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"review-codex-1","severity":"warning","file":"a.go","line":1,"description":"codex issue","source":"codex"},
		{"id":"review-claude-1","severity":"error","file":"b.go","line":2,"description":"claude issue","source":"claude"},
		{"id":"user-1","severity":"info","description":"user idea","source":"user"},
		{"id":"agent-1","severity":"info","description":"plain agent finding","source":"agent"}
	],"summary":"4 issues"}`

	plain := stripANSI(renderFindings(raw, 80))

	if !strings.Contains(plain, "[codex]") {
		t.Error("expected a [codex] reviewer tag in the rendered findings")
	}
	if !strings.Contains(plain, "[claude]") {
		t.Error("expected a [claude] reviewer tag in the rendered findings")
	}
	if !strings.Contains(plain, "[user]") {
		t.Error("expected the [user] tag for user-authored findings")
	}
	// The agent sentinel and empty sources must not get an attribution tag.
	if strings.Contains(plain, "[agent]") {
		t.Error("did not expect an [agent] attribution tag")
	}
}

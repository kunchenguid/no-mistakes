package scm

import (
	"strings"
	"testing"
)

func TestMaxPRBodyChars(t *testing.T) {
	t.Parallel()

	if got := MaxPRBodyChars(ProviderAzureDevOps); got != 4000 {
		t.Fatalf("MaxPRBodyChars(azuredevops) = %d, want 4000", got)
	}
	for _, p := range []Provider{ProviderGitHub, ProviderGitLab, ProviderBitbucket, ProviderUnknown} {
		if got := MaxPRBodyChars(p); got != 0 {
			t.Fatalf("MaxPRBodyChars(%s) = %d, want 0 (unlimited)", p, got)
		}
	}
}

func TestPRBodyLenCountsUTF16Units(t *testing.T) {
	t.Parallel()

	if got := PRBodyLen("abc"); got != 3 {
		t.Fatalf("PRBodyLen(ascii) = %d, want 3", got)
	}
	// A non-BMP rune is two UTF-16 code units, matching Azure's .NET measure.
	if got := PRBodyLen("😀"); got != 2 {
		t.Fatalf("PRBodyLen(emoji) = %d, want 2", got)
	}
}

func TestClampPRBody(t *testing.T) {
	t.Parallel()

	if got := ClampPRBody("short", 0); got != "short" {
		t.Fatalf("ClampPRBody with unlimited max changed body to %q", got)
	}
	if got := ClampPRBody("short", 4000); got != "short" {
		t.Fatalf("ClampPRBody under budget changed body to %q", got)
	}
	if got := ClampPRBody(strings.Repeat("a", 4000), 4000); PRBodyLen(got) != 4000 {
		t.Fatalf("ClampPRBody at exact budget changed length to %d", PRBodyLen(got))
	}

	clamped := ClampPRBody(strings.Repeat("x", 5000), 4000)
	if PRBodyLen(clamped) > 4000 {
		t.Fatalf("ClampPRBody left %d units, want <= 4000", PRBodyLen(clamped))
	}
	if !strings.HasSuffix(clamped, prBodyTruncationMarker) {
		t.Fatalf("ClampPRBody should mark truncation, got tail %q", clamped[len(clamped)-40:])
	}

	// Emoji bodies must respect the UTF-16 budget, never overshoot by counting runes.
	emoji := ClampPRBody(strings.Repeat("😀", 3000), 4000) // 6000 units in
	if PRBodyLen(emoji) > 4000 {
		t.Fatalf("ClampPRBody(emoji) = %d units, want <= 4000", PRBodyLen(emoji))
	}
}

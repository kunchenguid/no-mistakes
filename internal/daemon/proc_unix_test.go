//go:build !windows

package daemon

import (
	"testing"
	"time"
)

func TestParseProcessStartTimeUsesProvidedLocation(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)

	startedAt, err := parseProcessStartTime("Mon Jan 2 15:04:05 2006", loc)
	if err != nil {
		t.Fatalf("parseProcessStartTime returned error: %v", err)
	}

	if startedAt.Location() != loc {
		t.Fatalf("location = %v, want %v", startedAt.Location(), loc)
	}
	if startedAt.UTC() != time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC) {
		t.Fatalf("utc time = %v, want %v", startedAt.UTC(), time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC))
	}
}

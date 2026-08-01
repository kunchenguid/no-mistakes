package cli

import "testing"

func TestGhVersionPredatesChecksJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantVersion  string
		wantPredates bool
	}{
		{"host reproduction", "gh version 2.45.0 (2024-01-16)\n", "2.45.0", true},
		{"older major", "gh version 1.14.0 (2021-01-01)\n", "1.14.0", true},
		{"boundary", "gh version 2.50.0 (2024-05-29)\n", "2.50.0", false},
		{"newer", "gh version 3.1.2 (2026-01-01)\n", "3.1.2", false},
		{"unknown vendor output", "gh vendor build\n", "", false},
		{"empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			version, predates := ghVersionPredatesChecksJSON(tt.raw)
			if version != tt.wantVersion || predates != tt.wantPredates {
				t.Fatalf("ghVersionPredatesChecksJSON(%q) = (%q, %v), want (%q, %v)",
					tt.raw, version, predates, tt.wantVersion, tt.wantPredates)
			}
		})
	}
}

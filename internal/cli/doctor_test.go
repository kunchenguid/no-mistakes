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
		{
			name:         "older minor predates",
			raw:          "gh version 2.49.2 (2024-05-13)\nhttps://github.com/cli/cli/releases/tag/v2.49.2\n",
			wantVersion:  "2.49.2",
			wantPredates: true,
		},
		{
			name:         "much older release predates",
			raw:          "gh version 2.4.0 (2021-12-21)\n",
			wantVersion:  "2.4.0",
			wantPredates: true,
		},
		{
			name:         "exact boundary 2.50.0 does not predate",
			raw:          "gh version 2.50.0 (2024-05-29)\n",
			wantVersion:  "2.50.0",
			wantPredates: false,
		},
		{
			name:         "newer minor does not predate",
			raw:          "gh version 2.63.1 (2025-01-10)\n",
			wantVersion:  "2.63.1",
			wantPredates: false,
		},
		{
			name:         "newer major does not predate",
			raw:          "gh version 3.0.0 (2026-02-01)\n",
			wantVersion:  "3.0.0",
			wantPredates: false,
		},
		{
			name:         "unparseable output fails safe to no warning",
			raw:          "gh (homebrew build, version unknown)\n",
			wantVersion:  "",
			wantPredates: false,
		},
		{
			name:         "empty output fails safe to no warning",
			raw:          "",
			wantVersion:  "",
			wantPredates: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, predates := ghVersionPredatesChecksJSON(tt.raw)
			if version != tt.wantVersion {
				t.Fatalf("ghVersionPredatesChecksJSON(%q) version = %q, want %q", tt.raw, version, tt.wantVersion)
			}
			if predates != tt.wantPredates {
				t.Fatalf("ghVersionPredatesChecksJSON(%q) predates = %v, want %v", tt.raw, predates, tt.wantPredates)
			}
		})
	}
}

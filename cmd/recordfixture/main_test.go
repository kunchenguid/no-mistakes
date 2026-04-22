package main

import (
	"reflect"
	"testing"
)

func TestSplitBinArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		def     string
		wantBin string
		want    []string
	}{
		{
			name:    "default bin keeps forwarded args",
			args:    []string{"--model", "sonnet", "--profile", "ci"},
			def:     "claude",
			wantBin: "claude",
			want:    []string{"--model", "sonnet", "--profile", "ci"},
		},
		{
			name:    "custom bin removed from forwarded args",
			args:    []string{"--model", "sonnet", "--bin", "/tmp/agent", "--profile", "ci"},
			def:     "claude",
			wantBin: "/tmp/agent",
			want:    []string{"--model", "sonnet", "--profile", "ci"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, got := splitBinArgs(tt.args, tt.def)
			if gotBin != tt.wantBin {
				t.Fatalf("bin = %q, want %q", gotBin, tt.wantBin)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("forwarded args = %#v, want %#v", got, tt.want)
			}
		})
	}
}

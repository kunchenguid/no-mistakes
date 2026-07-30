package conventional

import (
	"strings"
	"testing"
)

func TestTaskIDApply_ReleasePleaseKeepsConventionalTypeAtPositionZero(t *testing.T) {
	t.Parallel()
	task := TaskID{ID: "WA-3093", Format: TaskIDFormatReleasePlease}
	got := task.Apply("fix(carousel): tighten slide spacing")
	want := "fix(carousel): tighten slide spacing (WA-3093)"
	if got != want {
		t.Fatalf("Apply() = %q, want %q", got, want)
	}
	// The whole point of the default format: release automation must still be
	// able to parse the type, so the decorated title stays conventional.
	if !IsTitle(got) {
		t.Fatalf("release-please format must keep the title conventional, got %q", got)
	}
	if TightenTitle(got) != got {
		t.Fatalf("TightenTitle rewrote a release-please-formatted title: %q", TightenTitle(got))
	}
}

func TestTaskIDApply_PrefixAndSuffixPlacement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		format TaskIDFormat
		want   string
	}{
		{"prefix", TaskIDFormatPrefix, "[WA-3093] fix(carousel): tighten slide spacing"},
		{"suffix", TaskIDFormatSuffix, "fix(carousel): tighten slide spacing [WA-3093]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := TaskID{ID: "WA-3093", Format: tt.format}
			if got := task.Apply("fix(carousel): tighten slide spacing"); got != tt.want {
				t.Fatalf("Apply() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskIDApply_EmptyIDIsANoOp(t *testing.T) {
	t.Parallel()
	title := "fix(carousel): tighten slide spacing"
	for _, id := range []string{"", "   "} {
		for _, format := range TaskIDFormats() {
			task := TaskID{ID: id, Format: format}
			if got := task.Apply(title); got != title {
				t.Fatalf("Apply(%q, %q) = %q, want the untouched title", id, format, got)
			}
		}
	}
}

func TestTaskIDApply_IsIdempotentAcrossRuns(t *testing.T) {
	t.Parallel()
	for _, format := range TaskIDFormats() {
		task := TaskID{ID: "WA-3093", Format: format}
		once := task.Apply("fix(carousel): tighten slide spacing")
		twice := task.Apply(once)
		if twice != once {
			t.Fatalf("format %q accreted the id on re-apply: %q -> %q", format, once, twice)
		}
	}
}

func TestTaskIDApply_DoesNotReapplyAnIDTheTitleAlreadyCarries(t *testing.T) {
	t.Parallel()
	// A human (or a previous run in another format) already put the id in the
	// title. Re-running must never stack a second copy on.
	existing := []string{
		"[WA-3093] fix(carousel): tighten slide spacing",
		"fix(carousel): tighten slide spacing [WA-3093]",
		"fix(carousel): tighten slide spacing (WA-3093)",
		"fix(carousel): WA-3093 tighten slide spacing",
	}
	for _, title := range existing {
		for _, format := range TaskIDFormats() {
			task := TaskID{ID: "WA-3093", Format: format}
			if got := task.Apply(title); got != title {
				t.Fatalf("Apply(%q) with format %q = %q, want unchanged", title, format, got)
			}
		}
	}
}

func TestTaskIDApply_DistinguishesIDsThatShareAPrefix(t *testing.T) {
	t.Parallel()
	task := TaskID{ID: "WA-309", Format: TaskIDFormatReleasePlease}
	got := task.Apply("fix(carousel): follow up to WA-3093")
	want := "fix(carousel): follow up to WA-3093 (WA-309)"
	if got != want {
		t.Fatalf("Apply() = %q, want %q", got, want)
	}
}

func TestTaskIDApply_HandlesNonJiraIDShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want string
	}{
		{"#412", "fix(carousel): tighten slide spacing (#412)"},
		{"1207937251234567", "fix(carousel): tighten slide spacing (1207937251234567)"},
		{"CU-8695mn2k", "fix(carousel): tighten slide spacing (CU-8695mn2k)"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			task := TaskID{ID: tt.id, Format: TaskIDFormatReleasePlease}
			if got := task.Apply("fix(carousel): tighten slide spacing"); got != tt.want {
				t.Fatalf("Apply() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskIDApply_UnsetFormatFallsBackToReleasePlease(t *testing.T) {
	t.Parallel()
	// Legacy runs persist no format at all; the release-safe default must win
	// rather than silently dropping the id.
	task := TaskID{ID: "WA-3093"}
	want := "fix(carousel): tighten slide spacing (WA-3093)"
	if got := task.Apply("fix(carousel): tighten slide spacing"); got != want {
		t.Fatalf("Apply() = %q, want %q", got, want)
	}
}

func TestTaskIDApply_EmptyTitleStaysEmpty(t *testing.T) {
	t.Parallel()
	task := TaskID{ID: "WA-3093", Format: TaskIDFormatPrefix}
	if got := task.Apply("  "); got != "" {
		t.Fatalf("Apply() = %q, want empty", got)
	}
}

func TestTaskIDApply_RejectsAnUnusableIDInsteadOfCorruptingTheTitle(t *testing.T) {
	t.Parallel()
	title := "fix(carousel): tighten slide spacing"
	for _, id := range []string{"WA\n3093", "WA\t3093", strings.Repeat("x", maxTaskIDLength+1)} {
		task := TaskID{ID: id, Format: TaskIDFormatPrefix}
		if got := task.Apply(title); got != title {
			t.Fatalf("Apply(%q) = %q, want the untouched title", id, got)
		}
	}
}

func TestValidateTaskID(t *testing.T) {
	t.Parallel()
	valid := []string{"WA-3093", "#412", "CU-8695mn2k", "1207937251234567", "PROJ_12"}
	for _, id := range valid {
		if err := ValidateTaskID(id); err != nil {
			t.Fatalf("ValidateTaskID(%q) = %v, want nil", id, err)
		}
	}
	invalid := []string{"", "   ", "WA\n3093", "WA\r3093", strings.Repeat("x", maxTaskIDLength+1)}
	for _, id := range invalid {
		if err := ValidateTaskID(id); err == nil {
			t.Fatalf("ValidateTaskID(%q) = nil, want an error", id)
		}
	}
}

func TestParseTaskIDFormat(t *testing.T) {
	t.Parallel()
	for _, format := range TaskIDFormats() {
		got, err := ParseTaskIDFormat(string(format))
		if err != nil {
			t.Fatalf("ParseTaskIDFormat(%q) = %v", format, err)
		}
		if got != format {
			t.Fatalf("ParseTaskIDFormat(%q) = %q", format, got)
		}
	}
	if got, err := ParseTaskIDFormat(""); err != nil || got != DefaultTaskIDFormat {
		t.Fatalf("ParseTaskIDFormat(\"\") = %q, %v; want %q, nil", got, err, DefaultTaskIDFormat)
	}
	if _, err := ParseTaskIDFormat("jira"); err == nil {
		t.Fatal("ParseTaskIDFormat(\"jira\") = nil error, want an error naming the valid formats")
	}
}

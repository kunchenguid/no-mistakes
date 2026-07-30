package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/conventional"
)

func TestTaskIDPushOptionRoundTrip(t *testing.T) {
	task := conventional.TaskID{ID: "WA-3093", Format: conventional.TaskIDFormatPrefix}
	options := formatTaskIDPushOptions(task)
	if len(options) == 0 {
		t.Fatal("formatTaskIDPushOptions returned nothing for a set task id")
	}

	got, err := parseTaskIDPushOptions(options)
	if err != nil {
		t.Fatalf("parseTaskIDPushOptions: %v", err)
	}
	if got != task {
		t.Fatalf("round trip = %+v, want %+v", got, task)
	}
}

func TestTaskIDPushOptionSurvivesSpecialCharacters(t *testing.T) {
	// Push options are line-oriented, so ids with spaces or "=" must survive
	// the transport the same way intent does.
	task := conventional.TaskID{ID: "#412 (urgent)", Format: conventional.TaskIDFormatSuffix}
	got, err := parseTaskIDPushOptions(formatTaskIDPushOptions(task))
	if err != nil {
		t.Fatalf("parseTaskIDPushOptions: %v", err)
	}
	if got != task {
		t.Fatalf("round trip = %+v, want %+v", got, task)
	}
}

func TestTaskIDPushOptionEmptyIsANoOp(t *testing.T) {
	if options := formatTaskIDPushOptions(conventional.TaskID{}); len(options) != 0 {
		t.Fatalf("formatTaskIDPushOptions(empty) = %v, want none", options)
	}
	got, err := parseTaskIDPushOptions([]string{"no-mistakes.skip=lint"})
	if err != nil {
		t.Fatalf("parseTaskIDPushOptions: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("parsed a task id from unrelated push options: %+v", got)
	}
}

func TestTaskIDPushOptionDefaultsFormatWhenOnlyIDIsCarried(t *testing.T) {
	got, err := parseTaskIDPushOptions(formatTaskIDPushOptions(conventional.TaskID{ID: "WA-3093"}))
	if err != nil {
		t.Fatalf("parseTaskIDPushOptions: %v", err)
	}
	if got.ID != "WA-3093" || got.Format != conventional.DefaultTaskIDFormat {
		t.Fatalf("parsed = %+v, want ID WA-3093 with the default format", got)
	}
}

func TestResolveTaskIDRejectsAnUnknownFormat(t *testing.T) {
	_, err := resolveTaskID("WA-3093", "jira")
	if err == nil {
		t.Fatal("resolveTaskID accepted an unknown format")
	}
	for _, format := range conventional.TaskIDFormats() {
		if !strings.Contains(err.Error(), string(format)) {
			t.Fatalf("error %q does not name the valid format %q", err, format)
		}
	}
}

func TestResolveTaskIDRejectsAnUnusableID(t *testing.T) {
	if _, err := resolveTaskID("WA\n3093", ""); err == nil {
		t.Fatal("resolveTaskID accepted an id containing a newline")
	}
}

func TestResolveTaskIDWithoutAnIDIsEmptyRegardlessOfFormat(t *testing.T) {
	// --task-id-format alone must not turn into a decorated title, and it must
	// not error either: the format is inert without an id.
	task, err := resolveTaskID("  ", "prefix")
	if err != nil {
		t.Fatalf("resolveTaskID: %v", err)
	}
	if !task.Empty() {
		t.Fatalf("resolveTaskID = %+v, want empty", task)
	}
}

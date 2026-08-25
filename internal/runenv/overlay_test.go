package runenv

import (
	"reflect"
	"testing"
)

func TestOverlayApplyRemovesAmbientSelectorsAndSetsProfile(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GH_TOKEN=ambient-token",
		"GH_HOST=wrong.example.com",
		"GH_CONFIG_DIR=/ambient/gh",
		"KEEP=value",
	}
	overlay := Overlay{
		Set: map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
		Unset: []string{
			"GH_TOKEN",
			"GITHUB_TOKEN",
			"GH_HOST",
			"GH_REPO",
		},
	}

	got := overlay.Apply(base)
	want := []string{
		"PATH=/usr/bin",
		"KEEP=value",
		"GH_CONFIG_DIR=/profiles/personal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply() = %#v, want %#v", got, want)
	}
}

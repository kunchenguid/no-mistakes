package main

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
)

func TestIsFakeProviderPlugin(t *testing.T) {
	for _, tc := range []struct {
		argv0 string
		want  bool
	}{
		{argv0: fakeplugin.ExecutableName, want: true},
		{argv0: fakeplugin.ExecutableName + ".exe", want: true},
		{argv0: fakeplugin.ExecutableName + ".EXE", want: true},
		{argv0: "NM-FAKE-PROVIDER-PLUGIN.exe", want: true},
		{argv0: filepath.Join("usr", "local", "bin", fakeplugin.ExecutableName), want: true},
		{argv0: filepath.Join("C:", "bin", fakeplugin.ExecutableName+".EXE"), want: true},
		{argv0: "gh", want: false},
		{argv0: "gh.exe", want: false},
		{argv0: "git", want: false},
		{argv0: "git.EXE", want: false},
	} {
		if got := isFakeProviderPlugin(tc.argv0); got != tc.want {
			t.Errorf("isFakeProviderPlugin(%q) = %v, want %v", tc.argv0, got, tc.want)
		}
	}
}

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

func TestFakePipelineStatusDerivesGitlabsAggregate(t *testing.T) {
	t.Setenv("FAKE_CLI_PIPELINE_STATUS", "")
	for _, tc := range []struct {
		jobs string
		want string
	}{
		{`[{"status":"success"},{"status":"failed","allow_failure":true}]`, "success"},
		{`[{"status":"failed","allow_failure":true},{"status":"failed"}]`, "failed"},
		{`[{"status":"failed"}]`, "failed"},
		// A blocking manual job stops the pipeline, and GitLab reads it before
		// a failure; an optional manual job does not hold the pipeline at all.
		{`[{"status":"success"},{"status":"manual","allow_failure":false}]`, "manual"},
		{`[{"status":"manual","allow_failure":true}]`, "success"},
		{`[{"status":"failed"},{"status":"manual","allow_failure":false}]`, "manual"},
		{`[{"status":"running"},{"status":"manual","allow_failure":false}]`, "running"},
	} {
		if got := fakePipelineStatus(tc.jobs); got != tc.want {
			t.Errorf("fakePipelineStatus(%s) = %q, want %q", tc.jobs, got, tc.want)
		}
	}
}

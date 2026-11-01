package cloudflare

import (
	"slices"
	"strings"
	"testing"
)

const sessionLine = `{"type":"wrangler-session","version":1,"wrangler_version":"4.156.0","command_line_args":["pages","deploy","site"],"log_file_path":"/home/someone/.wrangler/logs/wrangler.log","timestamp":"2026-11-01T20:00:00.000Z"}`

func detailedLine(version, url string) string {
	return `{"type":"pages-deploy-detailed","version":` + version + `,"pages_project":"nm-evidence","deployment_id":"5d4c3b2a-0000-4000-8000-000000000000","url":"` + url + `","alias":"https://no-mistakes-evidence.nm-evidence.pages.dev","environment":"preview","production_branch":"main","deployment_trigger":{"metadata":{"commit_hash":"abc123"}},"timestamp":"2026-11-01T20:00:05.000Z"}`
}

func TestParseDeployOutputReturnsTheDeploymentURLNotTheBranchAlias(t *testing.T) {
	output := strings.Join([]string{
		sessionLine,
		`{"type":"pages-deploy","version":1,"pages_project":"nm-evidence","deployment_id":"5d4c3b2a-0000-4000-8000-000000000000","url":"https://5d4c3b2a.nm-evidence.pages.dev","timestamp":"2026-11-01T20:00:05.000Z"}`,
		detailedLine("1", "https://5d4c3b2a.nm-evidence.pages.dev"),
		"",
	}, "\n")
	got, err := ParseDeployOutput([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://5d4c3b2a.nm-evidence.pages.dev" {
		t.Fatalf("deployment URL = %q", got)
	}
}

func TestParseDeployOutputRefusesARunThatReportedNoDeployment(t *testing.T) {
	if got, err := ParseDeployOutput([]byte(sessionLine + "\n")); err == nil {
		t.Fatalf("got %q, want an error", got)
	}
}

func TestParseDeployOutputRefusesAnUnknownOutputVersion(t *testing.T) {
	if got, err := ParseDeployOutput([]byte(detailedLine("2", "https://5d4c3b2a.nm-evidence.pages.dev"))); err == nil {
		t.Fatalf("got %q, want an error", got)
	}
}

func TestParseDeployOutputRefusesAURLOutsidePagesDev(t *testing.T) {
	for _, url := range []string{
		"",
		"http://5d4c3b2a.nm-evidence.pages.dev",
		"https://evidence.example.com",
		"https://pages.dev.example.com",
		"https://5d4c3b2a.nm-evidence.pages.dev:8443",
		"https://5d4c3b2a.nm-evidence.pages.dev/other",
		"https://user@5d4c3b2a.nm-evidence.pages.dev",
	} {
		if got, err := ParseDeployOutput([]byte(detailedLine("1", url))); err == nil {
			t.Errorf("url %q: got %q, want an error", url, got)
		}
	}
}

func TestValidateProjectNameRefusesAFlagShapedName(t *testing.T) {
	for _, name := range []string{"", "--help", "-evidence", "evidence-", "Evidence", "nm_evidence", "nm.evidence", strings.Repeat("a", 64)} {
		if ValidateProjectName(name) == nil {
			t.Errorf("%q accepted", name)
		}
	}
	for _, name := range []string{"a", "nm-evidence", "azalialaw-evidence-2", strings.Repeat("a", 63)} {
		if err := ValidateProjectName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
}

func TestDeployArgsNeverLeaveWranglerToReadGitMetadata(t *testing.T) {
	args := DeployRequest{Project: "nm-evidence", Branch: "no-mistakes-evidence", CommitHash: "abc123", CommitMessage: "evidence", Dir: "/tmp/site"}.Args()
	want := []string{
		"pages", "deploy", "/tmp/site",
		"--project-name=nm-evidence",
		"--branch=no-mistakes-evidence",
		"--commit-hash=abc123",
		"--commit-message=evidence",
		"--commit-dirty=true",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

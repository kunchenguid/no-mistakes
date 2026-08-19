package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// probeHead is the commit under test in the probe tests.
const probeHead = "abc123"

const (
	probeWorkflowsPath = "gh api repos/test/repo/actions/workflows"
	probeContentsPath  = "gh api repos/test/repo/contents/.github/workflows?ref=" + probeHead
)

func TestProbeCIConfigurationReportsPresentFromRegisteredWorkflows(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		probeWorkflowsPath: {
			stdout: `{"total_count":2,"workflows":[{"id":1,"state":"active"},{"id":2,"state":"disabled_inactivity"}]}`,
		},
	}), nil, "", "test/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationPresent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationPresent)
	}
}

// A pull request that adds the repository's first workflow is the case the
// registered list cannot answer: GitHub does not list a workflow until it
// creates a run for it, so the list is empty in exactly the window this probe
// runs in. The commit's own tree is the second, independent source.
func TestProbeCIConfigurationReportsPresentFromWorkflowFilesInTheCommit(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		probeWorkflowsPath: {
			stdout: `{"total_count":0,"workflows":[]}`,
		},
		probeContentsPath: {
			stdout: `[{"name":"ci.yml","type":"file"}]`,
		},
	}), nil, "", "test/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationPresent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationPresent)
	}
}

func TestProbeCIConfigurationReportsAbsentWithNoWorkflowsAnywhere(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		probeWorkflowsPath: {
			stdout: `{"total_count":0,"workflows":[]}`,
		},
		probeContentsPath: {
			stderr: "gh: Not Found (HTTP 404)",
			code:   1,
		},
	}), nil, "", "test/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationAbsent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationAbsent)
	}
}

// A workflow directory that holds no workflow definition is still no CI.
func TestProbeCIConfigurationReportsAbsentForADirectoryWithoutDefinitions(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		probeWorkflowsPath: {
			stdout: `{"total_count":0,"workflows":[]}`,
		},
		probeContentsPath: {
			stdout: `[{"name":"README.md","type":"file"}]`,
		},
	}), nil, "", "test/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationAbsent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationAbsent)
	}
}

// Every read this probe cannot complete must fail to Unknown, so a caller keeps
// the waiting behavior it had rather than acting on a guess.
func TestProbeCIConfigurationFailsToUnknown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		responses map[string]githubTestResponse
	}{
		{
			name: "workflow list unreadable",
			responses: map[string]githubTestResponse{
				probeWorkflowsPath: {
					stderr: "gh: HTTP 502",
					code:   1,
				},
			},
		},
		{
			name: "workflow list unparseable",
			responses: map[string]githubTestResponse{
				probeWorkflowsPath: {
					stdout: "not json",
				},
			},
		},
		{
			name: "commit tree unreadable for a reason other than absence",
			responses: map[string]githubTestResponse{
				probeWorkflowsPath: {
					stdout: `{"total_count":0,"workflows":[]}`,
				},
				probeContentsPath: {
					stderr: "gh: Bad credentials (HTTP 401)",
					code:   1,
				},
			},
		},
		{
			name: "commit tree unparseable",
			responses: map[string]githubTestResponse{
				probeWorkflowsPath: {
					stdout: `{"total_count":0,"workflows":[]}`,
				},
				probeContentsPath: {
					stdout: "not json",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := New(githubTestCmdFactory(tc.responses), nil, "", "test/repo")
			got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
			if err == nil {
				t.Fatal("ProbeCIConfiguration() error = nil, want an error naming the unreadable source")
			}
			if got != scm.CIConfigurationUnknown {
				t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationUnknown)
			}
		})
	}
}

func TestProbeCIConfigurationFailsClosedWithoutARepository(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(nil), nil, "", "")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err == nil {
		t.Fatal("ProbeCIConfiguration() error = nil, want a refusal when no repository is known")
	}
	if got != scm.CIConfigurationUnknown {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationUnknown)
	}
}

// The --repo flag needs the "host/owner/name" form on GitHub Enterprise Server,
// but a REST path carries only "owner/name" with the instance named by
// --hostname, so a multi-host gh configuration cannot answer for the wrong one.
func TestProbeCIConfigurationScopesEnterpriseRequestsByHostname(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --hostname ghe.example.com repos/org/repo/actions/workflows": {
			stdout: `{"total_count":1,"workflows":[{"id":7}]}`,
		},
	}), nil, "ghe.example.com", "ghe.example.com/org/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, probeHead)
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationPresent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationPresent)
	}
}

// Without a head SHA the probe still answers, reading the repository's default
// branch instead of refusing.
func TestProbeCIConfigurationOmitsTheRefWhenNoHeadIsKnown(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		probeWorkflowsPath: {
			stdout: `{"total_count":0,"workflows":[]}`,
		},
		"gh api repos/test/repo/contents/.github/workflows": {
			stdout: `[{"name":"ci.yaml","type":"file"}]`,
		},
	}), nil, "", "test/repo")

	got, err := host.ProbeCIConfiguration(context.Background(), &scm.PR{Number: "123"}, "  ")
	if err != nil {
		t.Fatalf("ProbeCIConfiguration() error = %v", err)
	}
	if got != scm.CIConfigurationPresent {
		t.Fatalf("ProbeCIConfiguration() = %q, want %q", got, scm.CIConfigurationPresent)
	}
}

func TestGitHubHostSatisfiesTheCIConfigurationProbe(t *testing.T) {
	t.Parallel()

	var probe scm.CIConfigurationProbe = New(githubTestCmdFactory(nil), nil, "", "test/repo")
	if probe == nil {
		t.Fatal("the GitHub host must satisfy scm.CIConfigurationProbe")
	}
}

package github

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// realCheckoutDeath is the head of a real Buildkite job log for a build whose
// agent could not clone the repository: the clone trace, the clone's own error,
// and the checkout step's failure warning. Nothing after it came from a step of
// the repository, because no step ran.
const realCheckoutDeath = "\x1b_bk;t=1778528419561\x1b\\~~~ Running commands\n" +
	"\x1b_bk;t=1778528419562\x1b\\$ cd /var/lib/buildkite-agent/builds/agent-1/keenvc/frontdeskclinic\n" +
	"\x1b_bk;t=1778528419700\x1b\\Cloning into '.'...\n" +
	"\x1b_bk;t=1778528419800\x1b\\kex_exchange_identification: Connection closed by remote host\n" +
	"\x1b_bk;t=1778528419850\x1b\\fatal: Could not read from remote repository.\n" +
	"\x1b_bk;t=1778528419861\x1b\\Please make sure you have the correct access rights\n" +
	"\x1b_bk;t=1778528419900\x1b\\git@github.com: Permission denied (publickey).\n" +
	"\x1b_bk;t=1778528420042\x1b\\🚨 Error: cloning git repository: exit status 128\n" +
	"\x1b_bk;t=1778528420099\x1b\\^^^ +++\n" +
	"⚠️ Warning: Checkout failed!\n" +
	"Perhaps the remote repository is private and requires the correct\n" +
	"credentials to be set up, or the repository does not exist?\n"

// The build link has to be the service's build page, not one job's page.
const buildLinkFixture = "https://buildkite.com/keenvc/frontdeskclinic/builds/3716"

// The build-level shape - a commit status a CI service posted about itself -
// is exactly what a build that died at checkout reports. A status with a target
// URL, a status whose description names a job-level failure class, and a passing
// status must all stay unmarked: each is a verdict about the code, or a failure
// that reached a job.
func TestGetChecks_ProviderAttributedOnlyForTheBuildLevelShape(t *testing.T) {
	t.Parallel()

	const head = "abc123"
	nodes := `[` +
		`{"__typename":"StatusContext","id":"SC_build","context":"buildkite/keenvc/frontdeskclinic","state":"FAILURE","targetUrl":"","description":"Buildkite keenvc/frontdeskclinic exited 128"},` +
		`{"__typename":"StatusContext","id":"SC_job","context":"buildkite/keenvc/frontdeskclinic-nightly","state":"FAILURE","targetUrl":"` + buildLinkFixture + `#0190aa-job","description":"Buildkite keenvc/frontdeskclinic-nightly exited 1"},` +
		`{"__typename":"StatusContext","id":"SC_timed","context":"buildkite/keenvc/slow","state":"FAILURE","targetUrl":"","description":"Buildkite keenvc/slow timed_out"},` +
		`{"__typename":"StatusContext","id":"SC_ok","context":"buildkite/keenvc/green","state":"SUCCESS","targetUrl":"","description":""}` +
		`]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 42 --repo keenvc/frontdeskclinic --json headRefOid --jq .headRefOid": {stdout: head + "\n"},
		"gh pr checks 42 --repo keenvc/frontdeskclinic --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"test","state":"SUCCESS","bucket":"pass"}]` + "\n",
		},
		"gh api --method GET repos/keenvc/frontdeskclinic/actions/runs -f head_sha=" + head + " -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":0,"workflow_runs":[]}]` + "\n",
		},
		githubCommitChecksCommand("", "keenvc/frontdeskclinic", head): {stdout: githubCommitChecksResponse(nodes)},
	}), nil, "", "keenvc/frontdeskclinic")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: head})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	got := map[string]bool{}
	for _, check := range checks {
		got[check.Name] = check.ProviderAttributedFailure
	}
	want := map[string]bool{
		"buildkite/keenvc/frontdeskclinic":         true,
		"buildkite/keenvc/frontdeskclinic-nightly": false,
		"buildkite/keenvc/slow":                    false,
		"buildkite/keenvc/green":                   false,
	}
	for name, wantFlag := range want {
		flag, ok := got[name]
		if !ok {
			t.Fatalf("GetChecks() did not report %q; got %v", name, got)
		}
		if flag != wantFlag {
			t.Errorf("check %q ProviderAttributedFailure = %v, want %v", name, flag, wantFlag)
		}
	}
}

// A log the CI service produced is read three ways, and only positive evidence
// of a missing repository is infrastructure: an unreadable, empty, or
// head-truncated log is ambiguous, and a complete log with no acquisition marker
// is a genuine failure.
func TestClassifyCIProviderLog_ThreeWaySplit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   scm.RepositoryAcquisition
	}{
		{"checkout death", realCheckoutDeath, scm.RepositoryAcquisitionInfrastructure},
		{"checkout death without framing", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", scm.RepositoryAcquisitionInfrastructure},
		{"genuine test failure", "~~~ Running go test\n--- FAIL: TestThing\n\tthing_test.go:12: got 1, want 2\nFAIL\n", scm.RepositoryAcquisitionNone},
		{"genuine lint failure", "~~~ Running make lint\nmain.go:12:2: undefined: thing\n", scm.RepositoryAcquisitionNone},
		{"head truncated", "…(log truncated)\n" + "some later step output\n", scm.RepositoryAcquisitionAmbiguous},
		{"head truncated with a marker", "…(log truncated)\n🚨 Error: cloning git repository: exit status 128\n", scm.RepositoryAcquisitionInfrastructure},
		{"empty", "", scm.RepositoryAcquisitionAmbiguous},
		{"blank", "   \n\t\n", scm.RepositoryAcquisitionAmbiguous},
	}
	for _, tc := range cases {
		if got := classifyCIProviderLog(tc.output); got != tc.want {
			t.Errorf("%s: classifyCIProviderLog() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The marker is matched against the message, not the CI service's per-line
// framing or its colour codes.
func TestAcquisitionFailureMarker(t *testing.T) {
	t.Parallel()

	for _, output := range []string{
		realCheckoutDeath,
		"git@github.com: Permission denied (publickey).\n",
		"\x1b[31mfatal: Could not read from remote repository.\x1b[0m\n",
		"\x1b_bk;t=1\x1b\\🚨 Error: cloning git repository: exit status 128\n",
		"error: fetching commit abc123\n",
		"⚠️ Warning: Checkout failed!\n",
	} {
		if !acquisitionFailureMarker(output) {
			t.Errorf("acquisitionFailureMarker(%q) = false, want true", output)
		}
	}
	if acquisitionFailureMarker("--- FAIL: TestThing\n\tthing_test.go:12: got 1, want 2\n") {
		t.Error("acquisitionFailureMarker flagged an ordinary test failure")
	}
}

// A status is only a question for the build-level shape; a target URL means a
// job verdict, and a description naming a job-class failure means the failure
// reached a job.
func TestStatusContextAttributedToProvider(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state, link string
		want        bool
	}{
		{"FAILURE", "", true},
		{"failure", "", true},
		{"FAILURE", buildLinkFixture, false},
		{"SUCCESS", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := statusContextAttributedToProvider(tc.state, tc.link); got != tc.want {
			t.Errorf("statusContextAttributedToProvider(%q, %q) = %v, want %v", tc.state, tc.link, got, tc.want)
		}
	}

	for _, desc := range []string{
		"Buildkite keenvc/frontdeskclinic timed_out",
		"Buildkite keenvc/frontdeskclinic canceled",
		"Buildkite keenvc/frontdeskclinic cancelled",
		"Buildkite keenvc/frontdeskclinic expired",
		"Buildkite keenvc/frontdeskclinic skipped",
		"Buildkite keenvc/frontdeskclinic broken",
		"Buildkite keenvc/frontdeskclinic not run",
	} {
		if !providerAttributedFailureDescription(desc) {
			t.Errorf("providerAttributedFailureDescription(%q) = false, want true (a job-level class)", desc)
		}
	}
	for _, desc := range []string{
		"Buildkite keenvc/frontdeskclinic exited 128",
		"",
	} {
		if providerAttributedFailureDescription(desc) {
			t.Errorf("providerAttributedFailureDescription(%q) = true, want false (not a job-level class)", desc)
		}
	}
}

func TestCIServiceBuildURL(t *testing.T) {
	t.Parallel()

	for _, link := range []string{
		"https://buildkite.com/keenvc/frontdeskclinic/builds/3716",
		"https://buildkite.com/keenvc/frontdeskclinic/builds/3716/",
	} {
		if !ciServiceBuildURL.MatchString(link) {
			t.Errorf("ciServiceBuildURL(%q) = false, want true", link)
		}
	}
	for _, link := range []string{
		buildLinkFixture + "#0190aa-job",
		"https://buildkite.com/keenvc/frontdeskclinic/pipelines",
		"https://github.com/keenvc/frontdeskclinic/actions/runs/123",
		"",
	} {
		if ciServiceBuildURL.MatchString(link) {
			t.Errorf("ciServiceBuildURL(%q) = true, want false", link)
		}
	}
}

// A head-truncated log is ambiguous even when its tail is clean: a checkout
// failure is the first thing a build emits, so it is exactly what a tail cut
// removes.
func TestLogHeadTruncated(t *testing.T) {
	t.Parallel()

	if !logHeadTruncated("…(log truncated)\nline one\nline two") {
		t.Error("logHeadTruncated did not flag an explicit truncation marker")
	}
	if logHeadTruncated("line one\n…(log truncated)\nline two") {
		t.Error("logHeadTruncated flagged a marker that is not at the head")
	}
	if logHeadTruncated("line one\nline two") {
		t.Error("logHeadTruncated flagged a complete log")
	}
}

// An unmarked check is never consulted, so no log read happens for it at all -
// that is what keeps a test that merely PRINTS an acquisition error from masking
// its own failure.
func TestRepositoryAcquisitions_UnmarkedChecksReadNoLog(t *testing.T) {
	t.Parallel()

	requests := make([]string, 0, 2)
	base := githubTestCmdFactory(map[string]githubTestResponse{})
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		requests = append(requests, " "+strings.Join(args, " "))
		return base(ctx, name, args...)
	}, nil, "", "keenvc/frontdeskclinic")

	got, err := host.RepositoryAcquisitions(context.Background(), &scm.PR{}, "main", "abc123", []scm.Check{
		{Name: "genuine", Bucket: scm.CheckBucketFail},
	})
	if err != nil {
		t.Fatalf("RepositoryAcquisitions() error = %v", err)
	}
	if len(got) != 1 || got[0] != scm.RepositoryAcquisitionNone {
		t.Fatalf("RepositoryAcquisitions() = %v, want [none]", got)
	}
	if len(requests) != 0 {
		t.Fatalf("RepositoryAcquisitions() ran %v for an unmarked check, want no provider call", requests)
	}
}

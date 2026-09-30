package forgejo

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func rawPullFactsJSON(sourceRepo, state, sha string) string {
	return rawPullFactsJSONFor(42, sourceRepo, state, sha)
}

func rawPullFactsJSONFor(number int, sourceRepo, state, sha string) string {
	url := testBaseURL + "/" + testRepo + "/pulls/" + strconv.Itoa(number)
	return fmt.Sprintf(`{"status":200,"data":{"number":%d,"html_url":%q,"state":%q,"merged":false,"head":{"ref":"feature/forgejo","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}}`, number, url, state, sha, sourceRepo)
}

func pullListJSON(numbers ...int) string {
	items := make([]string, 0, len(numbers))
	for _, number := range numbers {
		items = append(items, fmt.Sprintf(`{"number":%d}`, number))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestReadPRFactsKeepsForgeHeadAndBaseTogether(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{{stdout: rawPullFactsJSON(testRepo, "open", testHeadSHA)}}}
	host := newTestHost(recorder)
	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: testPRURL})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != testRepo || facts.SourceBranch != "feature/forgejo" || facts.HeadSHA != testHeadSHA || facts.BaseBranch != "main" || facts.State != scm.PRStateOpen {
		t.Fatalf("incomplete Forgejo PR facts: %+v", facts)
	}
}

func TestReadPRFactsPreservesForkSourceIdentity(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{{stdout: rawPullFactsJSON("other/widgets", "open", testHeadSHA)}}}
	facts, err := newTestHost(recorder).ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: testPRURL})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "other/widgets" {
		t.Fatalf("source repository = %q, want fork identity", facts.SourceRepository)
	}
}

func TestFindOpenPRFactsRejectsForeignSource(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: pullListJSON(42)},
		{stdout: rawPullFactsJSON("other/widgets", "open", testHeadSHA)},
		{stdout: `[]`},
	}}
	facts, err := newTestHost(recorder).FindOpenPRFacts(context.Background(), testRepo, "feature/forgejo")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("foreign fork PR bound to local source: %+v", facts)
	}
}

func TestFindOpenPRFactsDiscoversForkSource(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: pullListJSON(42)},
		{stdout: rawPullFactsJSON("other/widgets", "open", testHeadSHA)},
		{stdout: `[]`},
	}}
	facts, err := newTestHost(recorder).FindOpenPRFacts(context.Background(), "other/widgets", "feature/forgejo")
	if err != nil || len(facts) != 1 || facts[0].SourceRepository != "other/widgets" {
		t.Fatalf("fork facts = %+v, err=%v", facts, err)
	}
	want := []string{"api", "GET", "repos/" + testRepo + "/pulls?state=open&sort=oldest&limit=50&page=1", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if len(recorder.calls) == 0 || !reflect.DeepEqual(recorder.calls[0].args, want) {
		t.Fatalf("fork discovery command = %+v, want %v", recorder.calls, want)
	}
}

func TestFindOpenPRFactsFiltersSameBranchForkCandidates(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: pullListJSON(42)},
		{stdout: rawPullFactsJSONFor(42, "bob/widgets", "open", testHeadSHA)},
		{stdout: pullListJSON(43)},
		{stdout: rawPullFactsJSONFor(43, "alice/widgets", "open", testHeadSHA)},
		{stdout: `[]`},
	}}
	facts, err := newTestHost(recorder).FindOpenPRFacts(context.Background(), "alice/widgets", "feature/forgejo")
	if err != nil || len(facts) != 1 || facts[0].PR.Number != "43" || facts[0].SourceRepository != "alice/widgets" {
		t.Fatalf("exact fork facts = %+v, err=%v", facts, err)
	}
}

func TestReadPRFactsRejectsMissingSourceAndHead(t *testing.T) {
	for _, raw := range []string{
		`{"status":200,"data":{"number":42,"html_url":"` + testPRURL + `","state":"open","head":{"ref":"feature/forgejo","sha":"` + testHeadSHA + `"},"base":{"ref":"main"}}}`,
		rawPullFactsJSON(testRepo, "open", "short"),
	} {
		if _, err := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: raw}}}).ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: testPRURL}); err == nil {
			t.Fatalf("accepted incomplete PR facts: %s", raw)
		}
	}
}

func TestFindOpenPRFactsRejectsAmbiguousSearch(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: pullListJSON(42, 43)},
		{stdout: rawPullFactsJSONFor(42, testRepo, "open", testHeadSHA)},
		{stdout: rawPullFactsJSONFor(43, testRepo, "open", testHeadSHA)},
		{stdout: `[]`},
	}}
	host := newTestHost(recorder)
	_, err := host.FindOpenPRFacts(context.Background(), testRepo, "feature/forgejo")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous Forgejo search was accepted: %v", err)
	}
}

func TestFindOpenPRFactsRejectsIncompletePage(t *testing.T) {
	if _, err := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `null`}}}).FindOpenPRFacts(context.Background(), testRepo, "feature/forgejo"); err == nil {
		t.Fatal("incomplete Forgejo PR page was accepted")
	}
}

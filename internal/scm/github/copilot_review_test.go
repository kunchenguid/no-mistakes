package github

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const copilotReviewsCommand = "gh api --hostname github.com graphql -f query=" + copilotReviewsQuery + " -F owner=test -F name=repo -F number=42"
const copilotThreadsCommand = "gh api --hostname github.com graphql -f query=" + reviewThreadsQuery + " -F owner=test -F name=repo -F number=42"

const copilotCleanBody = "## Pull request overview\n\nAdds a widget.\n\n### Reviewed changes\n\nCopilot reviewed 1 out of 1 changed files in this pull request and generated no comments.\n"

func copilotReview(id, state, commit, submitted, body string) map[string]any {
	return map[string]any{
		"id": "PRR_" + id, "fullDatabaseId": id, "state": state, "body": body,
		"url":         "https://github.com/test/repo/pull/42#pullrequestreview-" + id,
		"author":      map[string]any{"__typename": "Bot", "login": "copilot-pull-request-reviewer", "databaseId": scm.CopilotAuthorID},
		"commit":      map[string]any{"oid": commit},
		"submittedAt": submitted, "updatedAt": submitted,
	}
}

func copilotReviewsPayload(head string, requested bool, reviews ...map[string]any) string {
	return copilotReviewsPage(head, requested, "", reviews...)
}

func copilotReviewsPage(head string, requested bool, next string, reviews ...map[string]any) string {
	if reviews == nil {
		reviews = []map[string]any{}
	}
	requests := []any{map[string]any{"requestedReviewer": map[string]any{"__typename": "User"}}}
	if requested {
		requests = append(requests, map[string]any{"requestedReviewer": map[string]any{"__typename": "Bot", "databaseId": scm.CopilotAuthorID, "login": "copilot-pull-request-reviewer"}})
	}
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"number": 42, "headRefOid": head,
		"reviewRequests": map[string]any{"nodes": requests, "pageInfo": map[string]any{"hasNextPage": false}},
		"reviews":        map[string]any{"nodes": reviews, "pageInfo": map[string]any{"hasNextPage": next != "", "endCursor": next}},
	}}}})
	return string(data)
}

type copilotThread struct {
	id       int64
	login    string
	authorID int64
	resolved bool
	path     string
	line     any
	body     string
}

func copilotThreadsPayload(threads ...copilotThread) string {
	nodes := []any{}
	for i, thread := range threads {
		nodes = append(nodes, map[string]any{"id": "T" + string(rune('a'+i)), "isResolved": thread.resolved, "comments": map[string]any{
			"nodes": []any{map[string]any{
				"databaseId": thread.id, "body": thread.body, "path": thread.path, "line": thread.line,
				"url":    "https://github.com/test/repo/pull/42#discussion_r" + strings.TrimSpace(jsonNumber(thread.id)),
				"author": map[string]any{"login": thread.login, "databaseId": thread.authorID},
			}},
			"pageInfo": map[string]any{"hasNextPage": false},
		}})
	}
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false},
	}}}}})
	return string(data)
}

func jsonNumber(v int64) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func copilotHost(reviews, threads string) *Host {
	if threads == "" {
		threads = copilotThreadsPayload()
	}
	return New(githubTestCmdFactory(map[string]githubTestResponse{
		reviewHeadCommand: {stdout: reviewHead}, copilotReviewsCommand: {stdout: reviews}, copilotThreadsCommand: {stdout: threads},
	}), nil, "github.com", "test/repo").WithRequiredReviews([]string{"copilot"}, 0)
}

func copilotCheck(t *testing.T, host *Host) scm.Check {
	t.Helper()
	checks, err := host.requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", []scm.Check{{Name: "External review: copilot", Bucket: scm.CheckBucketPass, App: "copilot"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0].ExternalReview != "" {
		t.Fatalf("ordinary look-alike check was not preserved unchanged: %+v", checks)
	}
	return checks[1]
}

func TestCopilotReviewCompletion(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	const later = "2026-11-19T03:00:00Z"
	const oldHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	twoMissed := "## Needs a closer look\n\n<details><summary>2 previously missed issues</summary>\n<details><summary>First problem</summary>Details one.</details>\n<details><summary>Second problem</summary>Details two.</details>\n</details>\n"
	impostor := func(change func(map[string]any)) map[string]any {
		review := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
		change(review)
		return review
	}
	for _, tc := range []struct {
		name     string
		reviews  string
		bucket   scm.CheckBucket
		state    string
		findings int
	}{
		{name: "commented clean", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)), bucket: scm.CheckBucketPass, state: "COMPLETE"},
		{name: "approved clean", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "APPROVED", reviewHead, at, copilotCleanBody)), bucket: scm.CheckBucketPass, state: "COMPLETE"},
		{name: "summary-only issues with zero inline comments", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", reviewHead, at, twoMissed)), bucket: scm.CheckBucketFail, state: "UNRESOLVED_FINDINGS", findings: 2},
		{name: "empty body does not prove completion", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", reviewHead, at, "")), bucket: scm.CheckBucketFail, state: "UNRESOLVED_FINDINGS", findings: 1},
		{name: "missing", reviews: copilotReviewsPayload(reviewHead, false), bucket: scm.CheckBucketPending, state: "MISSING"},
		{name: "old head review is never a fallback", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", oldHead, at, copilotCleanBody)), bucket: scm.CheckBucketPending, state: "MISSING"},
		{name: "requested", reviews: copilotReviewsPayload(reviewHead, true), bucket: scm.CheckBucketPending, state: "PENDING"},
		{name: "same head re-request after a clean review", reviews: copilotReviewsPayload(reviewHead, true, copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)), bucket: scm.CheckBucketPending, state: "PENDING"},
		{name: "pending review", reviews: copilotReviewsPayload(reviewHead, false, impostor(func(r map[string]any) { r["state"] = "PENDING"; r["submittedAt"] = nil })), bucket: scm.CheckBucketPending, state: "PENDING"},
		{name: "newer dismissal does not resurrect an older success", reviews: copilotReviewsPayload(reviewHead, false,
			copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody), copilotReview("102", "DISMISSED", reviewHead, later, copilotCleanBody)), bucket: scm.CheckBucketFail, state: "DISMISSED"},
		{name: "changes requested always blocks", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "CHANGES_REQUESTED", reviewHead, at, copilotCleanBody)), bucket: scm.CheckBucketFail, state: "UNRESOLVED_FINDINGS", findings: 1},
		{name: "service failure is incomplete", reviews: copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", reviewHead, at, "Copilot encountered an error and was unable to review this pull request.")), bucket: scm.CheckBucketFail, state: "INCOMPLETE", findings: 1},
		{name: "newer clean review supersedes an older summary", reviews: copilotReviewsPayload(reviewHead, false,
			copilotReview("101", "COMMENTED", reviewHead, at, twoMissed), copilotReview("102", "COMMENTED", reviewHead, later, copilotCleanBody)), bucket: scm.CheckBucketPass, state: "COMPLETE"},
		{name: "review ID above 32 bits orders a same-time tie", reviews: copilotReviewsPayload(reviewHead, false,
			copilotReview("4294967301", "COMMENTED", reviewHead, at, copilotCleanBody), copilotReview("4294967302", "COMMENTED", reviewHead, at, twoMissed)), bucket: scm.CheckBucketFail, state: "UNRESOLVED_FINDINGS", findings: 2},
		{name: "right login with wrong numeric ID", reviews: copilotReviewsPayload(reviewHead, false, impostor(func(r map[string]any) {
			r["author"] = map[string]any{"__typename": "Bot", "login": "copilot-pull-request-reviewer", "databaseId": 1}
		})), bucket: scm.CheckBucketPending, state: "MISSING"},
		{name: "right ID on a user account", reviews: copilotReviewsPayload(reviewHead, false, impostor(func(r map[string]any) {
			r["author"] = map[string]any{"__typename": "User", "login": "copilot-pull-request-reviewer", "databaseId": scm.CopilotAuthorID}
		})), bucket: scm.CheckBucketPending, state: "MISSING"},
		{name: "coding agent account", reviews: copilotReviewsPayload(reviewHead, false, impostor(func(r map[string]any) {
			r["author"] = map[string]any{"__typename": "Bot", "login": "Copilot", "databaseId": 198982749}
		})), bucket: scm.CheckBucketPending, state: "MISSING"},
		{name: "human approval", reviews: copilotReviewsPayload(reviewHead, false, impostor(func(r map[string]any) {
			r["state"] = "APPROVED"
			r["author"] = map[string]any{"__typename": "User", "login": "maintainer"}
		})), bucket: scm.CheckBucketPending, state: "MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := copilotCheck(t, copilotHost(tc.reviews, ""))
			if check.Bucket != tc.bucket || check.State != tc.state || len(check.ReviewFindings) != tc.findings {
				t.Fatalf("check=%+v findings=%+v, want %s/%s with %d findings", check, check.ReviewFindings, tc.bucket, tc.state, tc.findings)
			}
			if check.HeadSHA != reviewHead || check.AppHost != "github.com" || check.ProviderID != "external-review:copilot" || check.ExecutionID == "" {
				t.Fatalf("check identity=%+v", check)
			}
			bot, ok := scm.ReviewBotForCheck(scm.ReviewBotsFor([]string{"copilot"}), check)
			if !ok || bot.RequiredReview != "copilot" {
				t.Fatalf("synthesized check was not authenticated as Copilot: %+v", check)
			}
			for _, finding := range check.ReviewFindings {
				if finding.Path != "" || finding.Line != 0 || !strings.HasPrefix(finding.URL, "https://github.com/test/repo/pull/42#pullrequestreview-") {
					t.Fatalf("summary finding invented an anchor or lost its link: %+v", finding)
				}
			}
		})
	}
}

func TestCopilotReviewThreadsAndSummaryLinks(t *testing.T) {
	t.Parallel()
	body := "## Needs a closer look\n\n<details><summary>3 open issues</summary>\n\n" +
		"- Unchecked error, see https://github.com/test/repo/pull/42#discussion_r1001\n" +
		"- Already fixed, see https://github.com/test/repo/pull/42#discussion_r1002\n" +
		"- The cache key ignores the tenant\n" +
		"</details>\n"
	threads := copilotThreadsPayload(
		copilotThread{id: 1001, login: "copilot-pull-request-reviewer", authorID: scm.CopilotAuthorID, path: "internal/widget/a.go", line: 12, body: "Unchecked error."},
		copilotThread{id: 1002, login: "copilot-pull-request-reviewer", authorID: scm.CopilotAuthorID, resolved: true, path: "internal/widget/b.go", line: 3, body: "Fixed."},
		copilotThread{id: 1003, login: "copilot-pull-request-reviewer", authorID: scm.CopilotAuthorID, path: "internal/widget/old.go", line: nil, body: "Outdated anchor from an earlier head."},
		copilotThread{id: 1004, login: "copilot-pull-request-reviewer", authorID: 1, path: "internal/widget/a.go", line: 1, body: "Spoofed login."},
		copilotThread{id: 1005, login: "maintainer", authorID: 77, path: "internal/widget/a.go", line: 1, body: "Human note."},
	)
	check := copilotCheck(t, copilotHost(copilotReviewsPayload(reviewHead, false, copilotReview("101", "COMMENTED", reviewHead, "2026-11-19T02:00:00Z", body)), threads))
	if check.Bucket != scm.CheckBucketFail || check.State != "UNRESOLVED_FINDINGS" {
		t.Fatalf("check=%+v", check)
	}
	var got []string
	for _, finding := range check.ReviewFindings {
		got = append(got, finding.ID)
	}
	if strings.Join(got, ",") != "1001,1003,copilot-summary:PRR_101:2" {
		t.Fatalf("findings=%v (%+v)", got, check.ReviewFindings)
	}
	if f := check.ReviewFindings[0]; f.Path != "internal/widget/a.go" || f.Line != 12 || f.URL != "https://github.com/test/repo/pull/42#discussion_r1001" {
		t.Fatalf("thread finding=%+v", f)
	}
	if f := check.ReviewFindings[1]; f.Path != "internal/widget/old.go" || f.Line != 0 {
		t.Fatalf("outdated thread finding=%+v", f)
	}
	if f := check.ReviewFindings[2]; f.Path != "" || !strings.Contains(f.Body, "cache key ignores the tenant") {
		t.Fatalf("summary finding=%+v", f)
	}
}

func TestCopilotReviewDiscoveryFailsClosed(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	clean := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	moreRequests := strings.Replace(copilotReviewsPayload(reviewHead, false, clean), `"reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"User"}}],"pageInfo":{"hasNextPage":false}}`, `"reviewRequests":{"nodes":[],"pageInfo":{"hasNextPage":true}}`, 1)
	noSubmission := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	noSubmission["submittedAt"] = nil
	badID := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	badID["fullDatabaseId"] = "12x"
	foreignURL := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	foreignURL["url"] = "https://github.com/other/repo/pull/42#pullrequestreview-101"
	tie := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	tieTwin := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	tieTwin["id"] = "PRR_other"
	for name, payload := range map[string]string{
		"not json":                 "{",
		"graphql error":            `{"errors":[{"message":"rate limited"}],"data":null}`,
		"no pull request":          `{"data":{"repository":{"pullRequest":null}}}`,
		"another pull request":     strings.Replace(copilotReviewsPayload(reviewHead, false, clean), `"number":42`, `"number":43`, 1),
		"moved head":               copilotReviewsPayload(strings.Repeat("b", 40), false, clean),
		"missing review list":      `{"data":{"repository":{"pullRequest":{"number":42,"headRefOid":"` + reviewHead + `","reviewRequests":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`,
		"missing request list":     `{"data":{"repository":{"pullRequest":{"number":42,"headRefOid":"` + reviewHead + `","reviews":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`,
		"incomplete requests":      moreRequests,
		"missing submission time":  copilotReviewsPayload(reviewHead, false, noSubmission),
		"unverifiable review ID":   copilotReviewsPayload(reviewHead, false, badID),
		"review URL elsewhere":     copilotReviewsPayload(reviewHead, false, foreignURL),
		"repeated review identity": copilotReviewsPayload(reviewHead, false, clean, clean),
		"unorderable tie":          copilotReviewsPayload(reviewHead, false, tie, tieTwin),
		"invalid next cursor":      strings.Replace(copilotReviewsPage(reviewHead, false, "c1", clean), `"endCursor":"c1"`, `"endCursor":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := copilotHost(payload, "").requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil)
			if err == nil {
				t.Fatal("malformed or incomplete Copilot discovery produced a check")
			}
			if name == "moved head" && !errors.Is(err, scm.ErrHeadChanged) {
				t.Fatalf("moved head error=%v", err)
			}
		})
	}
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		copilotReviewsCommand: {stdout: copilotReviewsPayload(reviewHead, false, clean)}, copilotThreadsCommand: {stderr: "HTTP 502", code: 1},
	}), nil, "github.com", "test/repo")
	if _, err := host.requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil); err == nil {
		t.Fatal("an unreadable thread list certified the review")
	}
}

// statefulCopilotHost answers the review query from a sequence of payloads,
// one per call, repeating the last.
func statefulCopilotHost(t *testing.T, pages []string, threads string) (*Host, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	base := githubTestCmdFactory(map[string]githubTestResponse{reviewHeadCommand: {stdout: reviewHead}, copilotThreadsCommand: {stdout: threads}})
	return New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.Join(args, " ")
		mu.Lock()
		calls = append(calls, key)
		reviewCalls := 0
		for _, call := range calls {
			if strings.Contains(call, "reviewRequests") {
				reviewCalls++
			}
		}
		mu.Unlock()
		if strings.Contains(key, "reviewRequests") {
			page := pages[min(reviewCalls, len(pages))-1]
			return githubTestCmdFactory(map[string]githubTestResponse{name + " " + key: {stdout: page}})(ctx, name, args...)
		}
		return base(ctx, name, args...)
	}, nil, "github.com", "test/repo"), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func TestCopilotReviewChangeDuringDiscoveryIsNotCertified(t *testing.T) {
	t.Parallel()
	const at = "2026-11-19T02:00:00Z"
	clean := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	edited := copilotReview("101", "COMMENTED", reviewHead, at, copilotCleanBody)
	edited["updatedAt"] = "2026-11-19T02:30:00Z"
	newer := copilotReview("102", "COMMENTED", reviewHead, "2026-11-19T03:00:00Z", copilotCleanBody)
	for name, second := range map[string]string{
		"newer submission": copilotReviewsPayload(reviewHead, false, clean, newer),
		"edited body":      copilotReviewsPayload(reviewHead, false, edited),
		"re-request":       copilotReviewsPayload(reviewHead, true, clean),
		"dismissal":        copilotReviewsPayload(reviewHead, false, copilotReview("101", "DISMISSED", reviewHead, at, copilotCleanBody)),
		"moved head":       copilotReviewsPayload(strings.Repeat("b", 40), false, clean),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, _ := statefulCopilotHost(t, []string{copilotReviewsPayload(reviewHead, false, clean), second}, copilotThreadsPayload())
			if checks, err := host.requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil); err == nil {
				t.Fatalf("a review that changed during discovery was certified: %+v", checks)
			}
		})
	}
	host, calls := statefulCopilotHost(t, []string{copilotReviewsPayload(reviewHead, false, clean)}, copilotThreadsPayload())
	checks, err := host.requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil)
	if err != nil || len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("stable review checks=%+v err=%v", checks, err)
	}
	for _, call := range calls() {
		if !strings.HasPrefix(call, "api --hostname github.com graphql ") || strings.Contains(call, "mutation") {
			t.Fatalf("Copilot discovery issued a non-read call: %s", call)
		}
	}
}

func TestCopilotReviewPaginatesEveryReviewPage(t *testing.T) {
	t.Parallel()
	first := copilotReviewsPage(reviewHead, false, "c1", copilotReview("101", "COMMENTED", reviewHead, "2026-11-19T02:00:00Z", copilotCleanBody))
	second := copilotReviewsPage(reviewHead, false, "", copilotReview("102", "COMMENTED", reviewHead, "2026-11-19T03:00:00Z", "## Needs a closer look\n\nPlease double-check the migration order.\n"))
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		copilotReviewsCommand:                   {stdout: first},
		copilotReviewsCommand + " -F cursor=c1": {stdout: second},
		copilotThreadsCommand:                   {stdout: copilotThreadsPayload()},
	}), nil, "github.com", "test/repo")
	checks, err := host.requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil)
	if err != nil || len(checks) != 1 || checks[0].State != "UNRESOLVED_FINDINGS" || len(checks[0].ReviewFindings) != 1 {
		t.Fatalf("the latest review on a later page was not selected: %+v err=%v", checks, err)
	}
}

func TestCopilotReviewUnsupportedHosts(t *testing.T) {
	t.Parallel()
	noCalls := func(context.Context, string, ...string) *exec.Cmd {
		t.Error("unsupported Copilot host made a request")
		return exec.Command("false")
	}
	checks, err := New(noCalls, nil, "ghe.example.com", "test/repo").requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil)
	if err != nil || len(checks) != 1 || checks[0].Bucket != scm.CheckBucketFail || !strings.Contains(checks[0].State, "GITHUB ENTERPRISE SERVER") {
		t.Fatalf("GHES checks=%+v err=%v", checks, err)
	}
	if _, ok := scm.ReviewBotForCheck(scm.ReviewBotsFor([]string{"copilot"}), checks[0]); ok {
		t.Fatal("an unsupported marker authenticated as a verified Copilot review")
	}
	checks, err = New(noCalls, nil, "", "test/repo").requiredReviewChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead}, "copilot", nil)
	if err != nil || len(checks) != 0 {
		t.Fatalf("unknown host checks=%+v err=%v", checks, err)
	}
}

// Without the opt-in no Copilot read happens, and a Copilot thread comment the
// shared reader still returns never authenticates a check by itself.
func TestCopilotOptOutMakesNoCopilotRead(t *testing.T) {
	t.Parallel()
	base := githubTestCmdFactory(map[string]githubTestResponse{
		reviewHeadCommand: {stdout: reviewHead},
		"gh pr checks 42 --repo test/repo --json name,state,bucket,completedAt,link":                                                               {stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`},
		"gh api --hostname github.com --method GET repos/test/repo/actions/runs -f head_sha=" + reviewHead + " -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
	})
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if strings.Contains(strings.Join(args, " "), "reviewRequests") {
			t.Error("opted-out run read Copilot reviews")
		}
		return base(ctx, name, args...)
	}, nil, "github.com", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: reviewHead})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.ExternalReview != "" {
			t.Fatalf("opted-out run synthesized %+v", check)
		}
	}
}

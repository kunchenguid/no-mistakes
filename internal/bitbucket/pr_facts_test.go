package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const bitbucketFactsPull = `{"id":42,"state":"OPEN","source":{"branch":{"name":"feature"},"repository":{"full_name":"test/repo"},"commit":{"hash":"0123456789abcdef0123456789abcdef01234567"}},"destination":{"branch":{"name":"develop"}},"links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}}}`

const bitbucketForkFactsPull = `{"id":42,"state":"OPEN","source":{"branch":{"name":"feature"},"repository":{"full_name":"fork/repo"},"commit":{"hash":"0123456789abcdef0123456789abcdef01234567"}},"destination":{"branch":{"name":"develop"}},"links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}}}`

func TestReadPRFactsBindsBitbucketSourceHeadAndTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, bitbucketFactsPull)
	}))
	defer server.Close()
	host := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://bitbucket.org/test/repo/pull-requests/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "test/repo" || facts.SourceBranch != "feature" || facts.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || facts.BaseBranch != "develop" || facts.State != scm.PRStateOpen {
		t.Fatalf("incomplete Bitbucket PR facts: %+v", facts)
	}
}

func TestBitbucketPRFactsPreserveForkSourceIdentity(t *testing.T) {
	query := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2.0/repositories/test/repo/pullrequests" {
			query <- r.URL.Query().Get("q")
			fmt.Fprintf(w, `{"values":[%s]}`, bitbucketForkFactsPull)
			return
		}
		fmt.Fprint(w, bitbucketForkFactsPull)
	}))
	defer server.Close()
	host := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "test", RepoSlug: "repo"}, false)

	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://bitbucket.org/test/repo/pull-requests/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "fork/repo" {
		t.Fatalf("recorded fork source = %q, want fork/repo", facts.SourceRepository)
	}
	found, err := host.FindOpenPRFacts(context.Background(), "fork/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].SourceRepository != "fork/repo" {
		t.Fatalf("fork candidates = %+v", found)
	}
	if got := <-query; !strings.Contains(got, `source.repository.full_name="fork/repo"`) {
		t.Fatalf("query = %q, want fork source repository", got)
	}
}

func TestFindOpenPRFactsReadsEveryBitbucketPage(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			second := strings.ReplaceAll(bitbucketFactsPull, `"id":42`, `"id":43`)
			second = strings.ReplaceAll(second, "/pull-requests/42", "/pull-requests/43")
			fmt.Fprintf(w, `{"values":[%s]}`, second)
			return
		}
		fmt.Fprintf(w, `{"values":[%s],"next":%q}`, bitbucketFactsPull, server.URL+"/2.0/repositories/test/repo/pullrequests?page=2")
	}))
	defer server.Close()
	host := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	facts, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("FindOpenPRFacts returned %d candidates, want both for ambiguity detection", len(facts))
	}
}

func TestReadPRFactsRejectsWrongIdentityAndMissingHead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.ReplaceAll(bitbucketFactsPull, "0123456789abcdef0123456789abcdef01234567", ""))
	}))
	defer server.Close()
	host := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://bitbucket.org/test/repo/pull-requests/99"}); err == nil {
		t.Fatal("accepted a different recorded PR URL")
	}
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("accepted PR facts without an exact head")
	}
}

func TestFindOpenPRFactsRejectsMalformedLaterPage(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"values":null}`)
			return
		}
		fmt.Fprintf(w, `{"values":[%s],"next":%q}`, bitbucketFactsPull, server.URL+"/2.0/repositories/test/repo/pullrequests?page=2")
	}))
	defer server.Close()
	host := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	if _, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature"); err == nil {
		t.Fatal("accepted an incomplete later PR page")
	}
}

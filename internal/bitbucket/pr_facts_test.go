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

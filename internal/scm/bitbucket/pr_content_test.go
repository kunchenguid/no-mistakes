package bitbucket

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetPRContentReadsTitleAndDescription(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {
			stdout: `{"id":7,"title":"Human title","description":"# Human\n\n😀 café\n"}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)

	got, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetPRContent() error = %v", err)
	}
	if got.Title != "Human title" || got.Body != "# Human\n\n😀 café\n" {
		t.Fatalf("GetPRContent() = %+v", got)
	}
}

func TestGetPRContentAllowsExplicitlyEmptyBody(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {
			stdout: `{"id":7,"title":"T","description":""}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)

	got, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"})
	if err != nil {
		t.Fatalf("GetPRContent() error = %v", err)
	}
	if got.Body != "" {
		t.Fatalf("GetPRContent().Body = %q, want empty", got.Body)
	}
}

func TestGetPRContentRejectsUnprovenResponses(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		code   int
	}{
		{"empty output", "", "", 0},
		{"null", "null", "", 0},
		{"empty object", "{}", "", 0},
		{"missing description", `{"id":7,"title":"T"}`, "", 0},
		{"null description", `{"id":7,"title":"T","description":null}`, "", 0},
		{"empty title", `{"id":7,"title":"","description":"body"}`, "", 0},
		{"mismatched id", `{"id":8,"title":"T","description":"body"}`, "", 0},
		{"transport error", "", "boom", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(bbTestCmdFactory(map[string]bbTestResponse{
				"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {stdout: tt.stdout, stderr: tt.stderr, code: tt.code},
			}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
			if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
				t.Errorf("accepted %q", tt.stdout)
			}
		})
	}
}

func TestGetPRContentRejectsMissingIdentity(t *testing.T) {
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
	if _, err := h.GetPRContent(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil PR")
	}
	if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "not-a-number"}); err == nil {
		t.Fatal("expected error for non-numeric PR number")
	}
}

func TestGetPRRepositoryContentReportsResponseIdentity(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {
			stdout: `{"id":7,"title":"T","description":"body","links":{"html":{"href":"https://bitbucket.org/Other/Repo/pull-requests/7"}}}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)

	got, err := h.GetPRRepositoryContent(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetPRRepositoryContent() error = %v", err)
	}
	if got.Content.Title != "T" || got.Content.Body != "body" {
		t.Fatalf("content = %+v", got.Content)
	}
	// Identity is the response's own, not the configured repo's.
	if got.Repository.Host != "bitbucket.org" || got.Repository.Path != "Other/Repo" {
		t.Fatalf("identity = %+v", got.Repository)
	}
}

func TestGetPRRepositoryContentLeavesIdentityEmptyWithoutALink(t *testing.T) {
	h := New(bbTestCmdFactory(map[string]bbTestResponse{
		"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {
			stdout: `{"id":7,"title":"T","description":"body"}`,
		},
	}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)

	got, err := h.GetPRRepositoryContent(context.Background(), "7")
	if err != nil {
		t.Fatalf("GetPRRepositoryContent() error = %v", err)
	}
	if got.Repository != (scm.RepositoryIdentity{}) {
		t.Fatalf("identity = %+v, want empty", got.Repository)
	}
}

func TestGetPRRepositoryContentRejectsUnprovenResponses(t *testing.T) {
	for _, stdout := range []string{`{}`, `null`, `{"id":7,"title":"T"}`, `{"id":8,"title":"T","description":"b"}`, `{"id":7,"title":"","description":"b"}`} {
		h := New(bbTestCmdFactory(map[string]bbTestResponse{
			"twg bb pull-requests get 7 --workspace owner --repo repo -o json --output-summary none": {stdout: stdout},
		}), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
		if _, err := h.GetPRRepositoryContent(context.Background(), "7"); err == nil {
			t.Errorf("accepted %q", stdout)
		}
	}
	h := New(bbTestCmdFactory(nil), func() bool { return true }, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
	if _, err := h.GetPRRepositoryContent(context.Background(), "x"); err == nil {
		t.Error("accepted a non-numeric number")
	}
}

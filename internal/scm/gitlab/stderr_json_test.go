package gitlab

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// glab prints notices on stderr; they must not corrupt the JSON on stdout.
func TestFindPRIgnoresStderrNotice(t *testing.T) {
	t.Parallel()

	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr list --source-branch feature/refactor --target-branch release/1.0 --output json": {
			stdout: `[{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42"}]` + "\n",
			stderr: "A new version of glab is available\n",
		},
	}), nil, "gitlab.example.com", "group/project")
	pr, err := host.FindPR(context.Background(), "feature/refactor", "release/1.0")
	if err != nil || pr == nil || pr.Number != "42" {
		t.Fatalf("FindPR() = %+v, %v; want MR 42", pr, err)
	}
}

func TestGetReviewCommentsAndCreatePRIgnoreStderrNotice(t *testing.T) {
	t.Parallel()

	notice := "A new version of glab is available\nhttps://gitlab.example.com/group/project/-/merge_requests/999\n"
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --paginate projects/group%2Fproject/merge_requests/7/discussions?per_page=100 --hostname gitlab.example.com": {
			stdout: "[]\n",
			stderr: notice,
		},
		"glab mr create --source-branch feature --target-branch main --title t --description b --yes": {
			stdout: "https://gitlab.example.com/group/project/-/merge_requests/8\n",
			stderr: notice,
		},
	}), nil, "gitlab.example.com", "group/project")
	if _, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "7"}); err != nil {
		t.Fatalf("GetReviewComments() error = %v", err)
	}
	pr, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"})
	if err != nil || pr.Number != "8" {
		t.Fatalf("CreatePR() = %+v, %v; want MR 8", pr, err)
	}
}

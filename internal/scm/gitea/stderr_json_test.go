package gitea

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A warning on stderr must not corrupt the JSON tea prints on stdout.
func TestFindPRIgnoresStderrWarning(t *testing.T) {
	t.Parallel()

	host := New(giteaTestCmdFactory(map[string]giteaTestResponse{
		"tea pulls list --repo owner/repo --login work --fields index,title,state,url,head,base --output json": {
			stdout: `[{"index":"7","title":"mine","state":"open","url":"https://gitea.example.com/owner/repo/pulls/7","head":"feature/x","base":"main"}]`,
			stderr: "tool wrapper warning\n",
		},
	}), nil, "gitea.example.com", scm.TeaAccount{Login: "work"}, "owner/repo")
	pr, err := host.FindPR(context.Background(), "feature/x", "main")
	if err != nil || pr == nil || pr.Number != "7" {
		t.Fatalf("FindPR() = %+v, %v; want PR 7", pr, err)
	}
}

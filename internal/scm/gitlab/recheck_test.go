package gitlab

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksForHead(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const other = "2222222222222222222222222222222222222222"
	const view = "glab mr view 123 --output json"
	pipelines := "glab api --paginate projects/group%2Fproject/pipelines?sha=" + head
	jobs := "glab api --paginate projects/group%2Fproject/pipelines/77/jobs"
	bridges := "glab api --paginate projects/group%2Fproject/pipelines/77/bridges"
	mr := fmt.Sprintf(`{"sha":%q,"state":"opened","head_pipeline":{"id":77,"sha":%q}}`, head, head)
	pipeline := func(id int, status string) string {
		return fmt.Sprintf(`[{"id":%d,"sha":%q,"status":%q}]`, id, head, status)
	}
	for _, tc := range []string{"green", "pending", "failed sibling", "pending child", "empty pipelines", "empty children output", "null children", "corrupt later page", "no jobs", "unreadable jobs", "missing bridges", "failed downstream", "stale pipeline", "head moved during read", "pipeline moved during read", "provider error"} {
		t.Run(tc, func(t *testing.T) {
			responses := map[string]gitlabTestResponse{
				view:                                  {stdout: mr},
				pipelines:                             {stdout: pipeline(77, "success")},
				pipelines + "&source=parent_pipeline": {stdout: pipeline(78, "success")},
				jobs:                                  {stdout: `[{"id":1,"name":"test","status":"success"}]`},
				bridges:                               {stdout: fmt.Sprintf(`[{"id":2,"name":"child","status":"success","downstream_pipeline":{"sha":%q,"status":"success"}}]`, head)},
			}
			switch tc {
			case "pending":
				responses[pipelines] = gitlabTestResponse{stdout: pipeline(77, "running")}
			case "failed sibling":
				responses[pipelines] = gitlabTestResponse{stdout: pipeline(77, "success") + "\n" + pipeline(79, "failed")}
			case "pending child":
				responses[pipelines+"&source=parent_pipeline"] = gitlabTestResponse{stdout: pipeline(78, "pending")}
			case "empty pipelines":
				responses[pipelines] = gitlabTestResponse{stdout: `[]`}
			case "empty children output":
				responses[pipelines+"&source=parent_pipeline"] = gitlabTestResponse{}
			case "null children":
				responses[pipelines+"&source=parent_pipeline"] = gitlabTestResponse{stdout: `null`}
			case "corrupt later page":
				responses[pipelines] = gitlabTestResponse{stdout: pipeline(77, "success") + "\nnot-json"}
			case "no jobs":
				responses[jobs] = gitlabTestResponse{stdout: `[]`}
				responses[bridges] = gitlabTestResponse{stdout: `[]`}
			case "unreadable jobs":
				responses[jobs] = gitlabTestResponse{stdout: `{}`}
			case "missing bridges":
				responses[bridges] = gitlabTestResponse{stdout: `null`}
			case "failed downstream":
				responses[bridges] = gitlabTestResponse{stdout: fmt.Sprintf(`[{"id":2,"name":"child","status":"success","downstream_pipeline":{"sha":%q,"status":"failed"}}]`, head)}
			case "stale pipeline":
				responses[view] = gitlabTestResponse{stdout: fmt.Sprintf(`{"sha":%q,"state":"opened","head_pipeline":{"id":77,"sha":%q}}`, head, other)}
			case "provider error":
				responses[pipelines] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			}
			reads := 0
			factory := gitlabTestCmdFactory(responses)
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name+" "+strings.Join(args, " ") == view {
					reads++
					if reads == 2 {
						if tc == "head moved during read" {
							responses[view] = gitlabTestResponse{stdout: strings.ReplaceAll(mr, head, other)}
						}
						if tc == "pipeline moved during read" {
							responses[view] = gitlabTestResponse{stdout: strings.ReplaceAll(mr, `"id":77`, `"id":79`)}
						}
					}
				}
				return factory(ctx, name, args...)
			}, nil, "gitlab.example.com", "group/project")
			checks, err := host.GetChecksForHead(context.Background(), &scm.PR{Number: "123"}, head)
			green := err == nil && len(checks) > 0
			for _, check := range checks {
				if check.Bucket != scm.CheckBucketPass {
					green = false
				}
			}
			if green != (tc == "green") {
				t.Fatalf("verified=%v checks=%+v error=%v", green, checks, err)
			}
		})
	}
}

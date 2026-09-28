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
	pipelineRead := "glab api projects/group%2Fproject/pipelines/77"
	pipelines := "glab api --paginate projects/group%2Fproject/pipelines?sha=" + head
	jobs := "glab api --paginate projects/group%2Fproject/pipelines/77/jobs"
	bridges := "glab api --paginate projects/group%2Fproject/pipelines/77/bridges"
	mr := fmt.Sprintf(`{"sha":%q,"state":"opened","head_pipeline":{"id":77,"sha":%q}}`, head, head)
	pipeline := func(id int, sha, status string) string {
		return fmt.Sprintf(`{"id":%d,"sha":%q,"status":%q}`, id, sha, status)
	}
	bridge := func(id int, sha, status string) string {
		return fmt.Sprintf(`[{"id":2,"name":"child","status":"success","downstream_pipeline":%s}]`, pipeline(id, sha, status))
	}
	for _, tc := range []struct {
		name      string
		wantGreen bool
	}{
		{"green", true},
		{"superseded failed", true},
		{"superseded canceled", true},
		{"unrelated pending child", true},
		{"distinct downstream SHA", true},
		{"trigger only", true},
		{"jobs only", true},
		{"later bridge page", true},
		{"pending pipeline", false},
		{"failed pipeline with unrelated green", false},
		{"canceled pipeline", false},
		{"skipped pipeline", false},
		{"manual pipeline", false},
		{"unknown pipeline", false},
		{"missing pipeline status", false},
		{"missing pipeline", false},
		{"null pipeline", false},
		{"unreadable pipeline", false},
		{"unrelated pipeline", false},
		{"pipeline SHA mismatch", false},
		{"no jobs", false},
		{"pending job", false},
		{"failed job", false},
		{"unreadable jobs", false},
		{"empty jobs output", false},
		{"corrupt later job page", false},
		{"failed later job page", false},
		{"missing bridges", false},
		{"unreadable bridges", false},
		{"empty bridges output", false},
		{"corrupt later bridge page", false},
		{"failed later bridge page", false},
		{"missing downstream", false},
		{"unreadable downstream", false},
		{"missing downstream ID", false},
		{"missing downstream SHA", false},
		{"missing downstream status", false},
		{"pending downstream", false},
		{"failed downstream", false},
		{"canceled downstream", false},
		{"skipped downstream", false},
		{"pending bridge", false},
		{"failed bridge", false},
		{"skipped bridge with failing downstream", false},
		{"stale MR pipeline", false},
		{"missing MR pipeline", false},
		{"head moved during read", false},
		{"pipeline moved during read", false},
		{"MR closed during read", false},
		{"MR unreadable during read", false},
		{"provider error", false},
		{"jobs read error", false},
		{"bridges read error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := map[string]gitlabTestResponse{
				view:                                  {stdout: mr},
				pipelineRead:                          {stdout: pipeline(77, head, "success")},
				pipelines:                             {stdout: "[" + pipeline(77, head, "success") + "]"},
				pipelines + "&source=parent_pipeline": {stdout: "[" + pipeline(78, head, "success") + "]"},
				jobs:                                  {stdout: `[{"id":1,"name":"test","status":"success"}]`},
				bridges:                               {stdout: bridge(78, head, "success")},
			}
			wantCount := 3
			switch tc.name {
			case "superseded failed", "superseded canceled":
				status := strings.TrimPrefix(tc.name, "superseded ")
				responses[pipelines] = gitlabTestResponse{stdout: "[" + pipeline(76, head, status) + "," + pipeline(77, head, "success") + "]"}
			case "unrelated pending child":
				responses[pipelines+"&source=parent_pipeline"] = gitlabTestResponse{stdout: "[" + pipeline(79, head, "pending") + "]"}
			case "distinct downstream SHA":
				responses[bridges] = gitlabTestResponse{stdout: bridge(78, other, "success")}
			case "trigger only":
				responses[jobs] = gitlabTestResponse{stdout: `[]`}
				wantCount = 2
			case "jobs only":
				responses[bridges] = gitlabTestResponse{stdout: `[]`}
				wantCount = 2
			case "later bridge page":
				responses[bridges] = gitlabTestResponse{stdout: "[]\n" + bridge(78, other, "success")}
			case "pending pipeline", "canceled pipeline", "skipped pipeline", "manual pipeline", "unknown pipeline":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(77, head, strings.TrimSuffix(tc.name, " pipeline"))}
			case "failed pipeline with unrelated green":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(77, head, "failed")}
				responses[pipelines] = gitlabTestResponse{stdout: "[" + pipeline(76, head, "success") + "]"}
			case "missing pipeline status":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(77, head, "")}
			case "missing pipeline":
				responses[pipelineRead] = gitlabTestResponse{}
			case "null pipeline":
				responses[pipelineRead] = gitlabTestResponse{stdout: `null`}
			case "unreadable pipeline":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(77, head, "success") + "\nnot-json"}
			case "unrelated pipeline":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(76, head, "success")}
			case "pipeline SHA mismatch":
				responses[pipelineRead] = gitlabTestResponse{stdout: pipeline(77, other, "success")}
			case "no jobs":
				responses[jobs] = gitlabTestResponse{stdout: `[]`}
				responses[bridges] = gitlabTestResponse{stdout: `[]`}
			case "pending job", "failed job":
				responses[jobs] = gitlabTestResponse{stdout: fmt.Sprintf(`[{"id":1,"name":"test","status":%q}]`, strings.TrimSuffix(tc.name, " job"))}
			case "unreadable jobs":
				responses[jobs] = gitlabTestResponse{stdout: `{}`}
			case "empty jobs output":
				responses[jobs] = gitlabTestResponse{}
			case "corrupt later job page":
				responses[jobs] = gitlabTestResponse{stdout: responses[jobs].stdout + "\nnot-json"}
			case "failed later job page":
				responses[jobs] = gitlabTestResponse{stdout: responses[jobs].stdout + "\n" + `[{"id":3,"name":"lint","status":"failed"}]`}
			case "missing bridges":
				responses[bridges] = gitlabTestResponse{stdout: `null`}
			case "unreadable bridges":
				responses[bridges] = gitlabTestResponse{stdout: `{}`}
			case "empty bridges output":
				responses[bridges] = gitlabTestResponse{}
			case "corrupt later bridge page":
				responses[bridges] = gitlabTestResponse{stdout: responses[bridges].stdout + "\nnot-json"}
			case "failed later bridge page":
				responses[bridges] = gitlabTestResponse{stdout: responses[bridges].stdout + "\n" + strings.Replace(bridge(79, other, "failed"), `"id":2`, `"id":3`, 1)}
			case "missing downstream":
				responses[bridges] = gitlabTestResponse{stdout: `[{"id":2,"name":"child","status":"success"}]`}
			case "unreadable downstream":
				responses[bridges] = gitlabTestResponse{stdout: `[{"id":2,"name":"child","status":"success","downstream_pipeline":[]}]`}
			case "missing downstream ID":
				responses[bridges] = gitlabTestResponse{stdout: bridge(0, other, "success")}
			case "missing downstream SHA":
				responses[bridges] = gitlabTestResponse{stdout: bridge(78, "", "success")}
			case "missing downstream status":
				responses[bridges] = gitlabTestResponse{stdout: bridge(78, other, "")}
			case "pending downstream", "failed downstream", "canceled downstream", "skipped downstream":
				responses[bridges] = gitlabTestResponse{stdout: bridge(78, other, strings.TrimSuffix(tc.name, " downstream"))}
			case "pending bridge", "failed bridge":
				responses[bridges] = gitlabTestResponse{stdout: strings.Replace(bridge(78, other, "success"), `"status":"success"`, fmt.Sprintf(`"status":%q`, strings.TrimSuffix(tc.name, " bridge")), 1)}
			case "skipped bridge with failing downstream":
				responses[bridges] = gitlabTestResponse{stdout: strings.Replace(bridge(78, other, "failed"), `"status":"success"`, `"status":"skipped"`, 1)}
			case "stale MR pipeline":
				responses[view] = gitlabTestResponse{stdout: fmt.Sprintf(`{"sha":%q,"state":"opened","head_pipeline":{"id":77,"sha":%q}}`, head, other)}
			case "missing MR pipeline":
				responses[view] = gitlabTestResponse{stdout: fmt.Sprintf(`{"sha":%q,"state":"opened"}`, head)}
			case "provider error":
				responses[pipelineRead] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			case "jobs read error":
				responses[jobs] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			case "bridges read error":
				responses[bridges] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			}
			reads := 0
			factory := gitlabTestCmdFactory(responses)
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				command := name + " " + strings.Join(args, " ")
				if _, ok := responses[command]; !ok {
					t.Errorf("unexpected provider operation: %s", command)
				}
				if command == view {
					reads++
					if reads == 2 {
						switch tc.name {
						case "head moved during read":
							responses[view] = gitlabTestResponse{stdout: strings.ReplaceAll(mr, head, other)}
						case "pipeline moved during read":
							responses[view] = gitlabTestResponse{stdout: strings.ReplaceAll(mr, `"id":77`, `"id":79`)}
						case "MR closed during read":
							responses[view] = gitlabTestResponse{stdout: strings.ReplaceAll(mr, `"opened"`, `"closed"`)}
						case "MR unreadable during read":
							responses[view] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
						}
					}
				}
				return factory(ctx, name, args...)
			}, nil, "gitlab.example.com", "group/project")
			checks, err := host.GetChecksForHead(context.Background(), &scm.PR{Number: "123"}, head)
			green := err == nil && len(checks) > 0
			for _, check := range checks {
				if check.Bucket != scm.CheckBucketPass && check.Bucket != scm.CheckBucketSkip {
					green = false
				}
			}
			if green != tc.wantGreen {
				t.Fatalf("verified=%v checks=%+v error=%v", green, checks, err)
			}
			if green && (len(checks) != wantCount || reads != 2) {
				t.Fatalf("checks=%+v MR reads=%d, want %d current checks and two MR reads", checks, reads, wantCount)
			}
		})
	}
}

package gitlab

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksForHeadDescendants(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const childHead = "2222222222222222222222222222222222222222"
	const grandchildHead = "3333333333333333333333333333333333333333"
	pipeline := func(id int, project, sha, status string) string {
		return fmt.Sprintf(`{"id":%d,"sha":%q,"status":%q,"web_url":"https://gitlab.example.com/%s/-/pipelines/%d"}`, id, sha, status, project, id)
	}
	bridge := func(downstream string) string {
		return fmt.Sprintf(`[{"id":10,"name":"trigger","status":"success","downstream_pipeline":%s}]`, downstream)
	}
	for _, tc := range []string{
		"green", "same project", "legacy pipeline URL", "later descendant page",
		"pending", "failed", "unreadable", "absent", "null pipeline", "empty pipeline",
		"stale downstream status", "wrong pipeline ID", "changed downstream SHA",
		"empty descendant", "unreadable descendant jobs", "unreadable descendant bridges",
		"failed descendant job", "pending descendant job", "corrupt descendant page", "failed later descendant page",
		"missing downstream URL", "foreign downstream host", "mismatched URL ID", "ambiguous project", "URL query",
		"cycle", "canceled traversal", "MR head moved",
	} {
		t.Run(tc, func(t *testing.T) {
			child := pipeline(78, "group/child", childHead, "success")
			grandchild := pipeline(79, "group/grandchild", grandchildHead, "success")
			responses := map[string]gitlabTestResponse{
				"glab mr view 123 --output json":                                       {stdout: fmt.Sprintf(`{"sha":%q,"state":"opened","head_pipeline":{"id":77,"sha":%q}}`, head, head)},
				"glab api projects/group%2Fproject/pipelines/77":                       {stdout: pipeline(77, "group/project", head, "success")},
				"glab api --paginate projects/group%2Fproject/pipelines/77/jobs":       {stdout: `[]`},
				"glab api --paginate projects/group%2Fproject/pipelines/77/bridges":    {stdout: bridge(child)},
				"glab api projects/group%2Fchild/pipelines/78":                         {stdout: child},
				"glab api --paginate projects/group%2Fchild/pipelines/78/jobs":         {stdout: `[]`},
				"glab api --paginate projects/group%2Fchild/pipelines/78/bridges":      {stdout: bridge(grandchild)},
				"glab api projects/group%2Fgrandchild/pipelines/79":                    {stdout: grandchild},
				"glab api --paginate projects/group%2Fgrandchild/pipelines/79/jobs":    {stdout: `[{"id":11,"name":"test","status":"success"}]`},
				"glab api --paginate projects/group%2Fgrandchild/pipelines/79/bridges": {stdout: `[]`},
			}
			switch tc {
			case "pending", "failed":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge(pipeline(79, "group/grandchild", grandchildHead, tc))}
			case "unreadable":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			case "absent":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge("null")}
			case "null pipeline":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{stdout: `null`}
			case "empty pipeline":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{}
			case "stale downstream status":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{stdout: pipeline(79, "group/grandchild", grandchildHead, "running")}
			case "wrong pipeline ID":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{stdout: pipeline(80, "group/grandchild", grandchildHead, "success")}
			case "changed downstream SHA":
				responses["glab api projects/group%2Fgrandchild/pipelines/79"] = gitlabTestResponse{stdout: pipeline(79, "group/grandchild", childHead, "success")}
			case "empty descendant":
				responses["glab api --paginate projects/group%2Fgrandchild/pipelines/79/jobs"] = gitlabTestResponse{stdout: `[]`}
			case "unreadable descendant jobs":
				responses["glab api --paginate projects/group%2Fgrandchild/pipelines/79/jobs"] = gitlabTestResponse{stdout: `null`}
			case "unreadable descendant bridges":
				responses["glab api --paginate projects/group%2Fgrandchild/pipelines/79/bridges"] = gitlabTestResponse{code: 1, stderr: "provider unavailable"}
			case "failed descendant job", "pending descendant job":
				responses["glab api --paginate projects/group%2Fgrandchild/pipelines/79/jobs"] = gitlabTestResponse{stdout: fmt.Sprintf(`[{"id":11,"name":"test","status":%q}]`, strings.TrimSuffix(tc, " descendant job"))}
			case "corrupt descendant page":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge(grandchild) + "\nnot-json"}
			case "failed later descendant page":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge(grandchild) + "\n" + bridge(pipeline(80, "group/grandchild", grandchildHead, "failed"))}
			case "later descendant page":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: "[]\n" + bridge(grandchild)}
			case "same project":
				replacer := strings.NewReplacer("group%2Fchild", "group%2Fproject", "group%2Fgrandchild", "group%2Fproject", "group/child", "group/project", "group/grandchild", "group/project", childHead, head, grandchildHead, head)
				sameProject := make(map[string]gitlabTestResponse)
				for command, response := range responses {
					response.stdout = replacer.Replace(response.stdout)
					sameProject[replacer.Replace(command)] = response
				}
				responses = sameProject
			case "legacy pipeline URL":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: strings.ReplaceAll(bridge(grandchild), "/-/pipelines/", "/pipelines/")}
			case "missing downstream URL":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge(fmt.Sprintf(`{"id":79,"sha":%q,"status":"success"}`, grandchildHead))}
			case "foreign downstream host":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: strings.ReplaceAll(bridge(grandchild), "gitlab.example.com", "other.example.com")}
			case "mismatched URL ID":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: strings.ReplaceAll(bridge(grandchild), "/pipelines/79", "/pipelines/80")}
			case "ambiguous project":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: strings.ReplaceAll(bridge(grandchild), "group/grandchild", "group/../grandchild")}
			case "URL query":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: strings.ReplaceAll(bridge(grandchild), "/pipelines/79", "/pipelines/79?other=1")}
			case "cycle":
				responses["glab api --paginate projects/group%2Fchild/pipelines/78/bridges"] = gitlabTestResponse{stdout: bridge(pipeline(77, "group/project", head, "success"))}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			factory := gitlabTestCmdFactory(responses)
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				command := name + " " + strings.Join(args, " ")
				if _, ok := responses[command]; !ok {
					t.Errorf("unexpected provider operation: %s", command)
				}
				if tc == "canceled traversal" && command == "glab api projects/group%2Fgrandchild/pipelines/79" {
					cancel()
				}
				if command == "glab mr view 123 --output json" {
					reads++
					if tc == "MR head moved" && reads == 2 {
						responses[command] = gitlabTestResponse{stdout: strings.ReplaceAll(responses[command].stdout, head, childHead)}
					}
				}
				return factory(ctx, name, args...)
			}, nil, "gitlab.example.com", "group/project")
			checks, err := host.GetChecksForHead(ctx, &scm.PR{Number: "123"}, head)
			green := err == nil && len(checks) > 0
			for _, check := range checks {
				if check.Bucket != scm.CheckBucketPass && check.Bucket != scm.CheckBucketSkip {
					green = false
				}
			}
			wantGreen := tc == "green" || tc == "same project" || tc == "legacy pipeline URL" || tc == "later descendant page"
			if green != wantGreen {
				t.Fatalf("verified=%v checks=%+v error=%v", green, checks, err)
			}
			if green && (len(checks) != 6 || reads != 2) {
				t.Fatalf("checks=%+v MR reads=%d, want the complete chain and two MR reads", checks, reads)
			}
		})
	}
}

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
		return fmt.Sprintf(`{"id":%d,"sha":%q,"status":%q,"web_url":"https://gitlab.example.com/group/downstream/-/pipelines/%d"}`, id, sha, status, id)
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
				"glab api projects/group%2Fdownstream/pipelines/78":                    {stdout: pipeline(78, head, "success")},
				"glab api --paginate projects/group%2Fdownstream/pipelines/78/jobs":    {stdout: `[{"id":4,"name":"downstream test","status":"success"}]`},
				"glab api --paginate projects/group%2Fdownstream/pipelines/78/bridges": {stdout: `[]`},
			}
			wantCount := 5
			switch tc.name {
			case "superseded failed", "superseded canceled":
				status := strings.TrimPrefix(tc.name, "superseded ")
				responses[pipelines] = gitlabTestResponse{stdout: "[" + pipeline(76, head, status) + "," + pipeline(77, head, "success") + "]"}
			case "unrelated pending child":
				responses[pipelines+"&source=parent_pipeline"] = gitlabTestResponse{stdout: "[" + pipeline(79, head, "pending") + "]"}
			case "distinct downstream SHA":
				responses[bridges] = gitlabTestResponse{stdout: bridge(78, other, "success")}
				responses["glab api projects/group%2Fdownstream/pipelines/78"] = gitlabTestResponse{stdout: pipeline(78, other, "success")}
			case "trigger only":
				responses[jobs] = gitlabTestResponse{stdout: `[]`}
				wantCount = 4
			case "jobs only":
				responses[bridges] = gitlabTestResponse{stdout: `[]`}
				wantCount = 2
			case "later bridge page":
				responses[bridges] = gitlabTestResponse{stdout: "[]\n" + bridge(78, other, "success")}
				responses["glab api projects/group%2Fdownstream/pipelines/78"] = gitlabTestResponse{stdout: pipeline(78, other, "success")}
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
				responses[bridges] = gitlabTestResponse{stdout: strings.Replace(bridge(78, head, "success"), `"status":"success"`, fmt.Sprintf(`"status":%q`, strings.TrimSuffix(tc.name, " bridge")), 1)}
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

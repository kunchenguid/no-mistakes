package steps

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func latestBitbucketStatuses(statuses []bitbucket.CommitStatus) []bitbucket.CommitStatus {
	latest := make([]bitbucket.CommitStatus, 0, len(statuses))
	seen := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		id := strings.TrimSpace(status.Key)
		if id == "" {
			id = bitbucketStatusName(status)
		}
		if id == "" {
			latest = append(latest, status)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		latest = append(latest, status)
	}
	return latest
}

func bitbucketStatusName(status bitbucket.CommitStatus) string {
	name := strings.TrimSpace(status.Name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(status.Key)
}

func bitbucketStatusBucket(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESSFUL", "SUCCESS":
		return "pass"
	case "FAILED", "FAILURE", "ERROR":
		return "fail"
	case "STOPPED":
		return "cancel"
	case "INPROGRESS", "IN_PROGRESS", "PENDING":
		return "pending"
	default:
		return ""
	}
}

func resolveBitbucketRepoRef(upstreamURL string, prURL *string) (bitbucket.RepoRef, error) {
	if repo, err := bitbucket.ParseRepoRef(upstreamURL); err == nil {
		return repo, nil
	}
	if prURL != nil && strings.TrimSpace(*prURL) != "" {
		return bitbucket.ParseRepoRef(*prURL)
	}
	return bitbucket.RepoRef{}, fmt.Errorf("resolve Bitbucket repository from upstream %q", upstreamURL)
}

func trimLogOutput(logOutput string, maxBytes int) string {
	if len(logOutput) <= maxBytes {
		return logOutput
	}
	logOutput = logOutput[len(logOutput)-maxBytes:]
	for i := 0; i < len(logOutput) && i < 4; i++ {
		if logOutput[i]&0xC0 != 0x80 {
			return logOutput[i:]
		}
	}
	return logOutput
}

func normalizeBitbucketPipelineUUID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.Trim(trimmed, "{}")
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed)
}

func bitbucketPipelineUUIDFromStatusURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	fragments := []string{parsed.Fragment, parsed.Path}
	for _, fragment := range fragments {
		idx := strings.LastIndex(fragment, "/results/")
		if idx < 0 {
			continue
		}
		uuid := fragment[idx+len("/results/"):]
		uuid = strings.TrimSpace(strings.SplitN(uuid, "?", 2)[0])
		uuid = strings.TrimSpace(strings.SplitN(uuid, "/", 2)[0])
		return normalizeBitbucketPipelineUUID(uuid)
	}
	return ""
}

func bitbucketFailedPipelineUUIDs(statuses []bitbucket.CommitStatus, failingNames []string) map[string]struct{} {
	if len(failingNames) == 0 {
		return nil
	}
	failing := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			failing[trimmed] = struct{}{}
		}
	}
	if len(failing) == 0 {
		return nil
	}
	targets := map[string]struct{}{}
	for _, status := range latestBitbucketStatuses(statuses) {
		if _, ok := failing[bitbucketStatusName(status)]; !ok {
			continue
		}
		uuid := bitbucketPipelineUUIDFromStatusURL(status.URL)
		if uuid != "" {
			targets[uuid] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return targets
}

func (s *CIStep) fetchBitbucketFailedStepLogs(sctx *pipeline.StepContext, client *bitbucket.Client, repo bitbucket.RepoRef, commitSHA string, targetPipelines map[string]struct{}) string {
	if client == nil || strings.TrimSpace(commitSHA) == "" {
		return ""
	}
	pipelines, err := client.ListPipelinesByCommit(sctx.Ctx, repo, commitSHA)
	if err != nil {
		return ""
	}
	for _, pipelineRun := range pipelines {
		if len(targetPipelines) > 0 {
			if _, ok := targetPipelines[normalizeBitbucketPipelineUUID(pipelineRun.UUID)]; !ok {
				continue
			}
		}
		steps, err := client.ListPipelineSteps(sctx.Ctx, repo, pipelineRun.UUID)
		if err != nil {
			continue
		}
		for _, step := range steps {
			if strings.EqualFold(step.State.Result.Name, "FAILED") {
				logOutput, err := client.GetStepLog(sctx.Ctx, repo, pipelineRun.UUID, step.UUID)
				if err != nil || strings.TrimSpace(logOutput) == "" {
					continue
				}
				return trimLogOutput(strings.TrimSpace(logOutput), 32*1024)
			}
		}
	}
	return ""
}

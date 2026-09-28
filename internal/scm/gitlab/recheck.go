package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetChecksForHead deliberately does not use `ci status --mr`: that command
// does not prove which commit produced its jobs. Pin the MR pipeline and all
// same-head pipeline results, including children (excluded by GitLab's default
// list endpoint), then re-read the MR. No provider rerun or write is performed.
func (h *Host) GetChecksForHead(ctx context.Context, pr *scm.PR, expectedHead string) ([]scm.Check, error) {
	if strings.TrimSpace(expectedHead) == "" || h.projectPath == "" {
		return nil, fmt.Errorf("missing exact head or GitLab project")
	}
	pipelineID, err := h.recheckMRHead(ctx, pr, expectedHead)
	if err != nil {
		return nil, err
	}
	endpoint := "projects/" + url.PathEscape(h.projectPath) + "/pipelines"
	var checks []scm.Check
	found := false
	for _, suffix := range []string{"", "&source=parent_pipeline"} {
		out, err := h.cmd(ctx, "glab", "api", "--paginate", endpoint+"?sha="+url.QueryEscape(expectedHead)+suffix).Output()
		if err != nil {
			return nil, fmt.Errorf("read exact-head GitLab pipelines: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(out))
		pages := 0
		for {
			var page []struct {
				ID     int    `json:"id"`
				SHA    string `json:"sha"`
				Status string `json:"status"`
			}
			err := decoder.Decode(&page)
			if err == io.EOF {
				break
			}
			if err != nil || page == nil {
				return nil, fmt.Errorf("unreadable GitLab pipeline page: %v", err)
			}
			pages++
			for _, p := range page {
				if p.ID <= 0 || p.SHA != expectedHead || p.Status == "" {
					return nil, fmt.Errorf("GitLab pipeline is not bound to expected head %s", expectedHead)
				}
				if p.ID == pipelineID {
					found = true
				}
				// A skipped/manual pipeline is not proof of green required checks.
				bucket := scm.CheckBucketPending
				if p.Status == "success" {
					bucket = scm.CheckBucketPass
				} else if p.Status == "failed" {
					bucket = scm.CheckBucketFail
				}
				checks = append(checks, scm.Check{Name: fmt.Sprintf("pipeline %d", p.ID), Bucket: bucket, State: p.Status})
			}
		}
		if pages == 0 {
			return nil, fmt.Errorf("GitLab returned no pipeline pages")
		}
	}
	if !found {
		return nil, fmt.Errorf("current MR pipeline missing from exact-head results")
	}
	// Retain the ordinary job verdicts as well as pipeline-level verdicts. A
	// trigger-only root can have bridges instead of jobs; both are required reads.
	var jobs []scm.Check
	for _, kind := range []string{"jobs", "bridges"} {
		out, err := h.cmd(ctx, "glab", "api", "--paginate", fmt.Sprintf("%s/%d/%s", endpoint, pipelineID, kind)).Output()
		if err != nil {
			return nil, fmt.Errorf("read GitLab pipeline %s: %w", kind, err)
		}
		parsed, err := recheckJobs(out, kind == "bridges", expectedHead)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, parsed...)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("current MR pipeline has no jobs or bridges")
	}
	checks = append(checks, jobs...)
	current, err := h.recheckMRHead(ctx, pr, expectedHead)
	if err != nil {
		return nil, err
	}
	if current != pipelineID {
		return nil, fmt.Errorf("MR pipeline changed during CI recheck")
	}
	return checks, nil
}

// Unlike the older CLI job parser, REST recheck reads require an actual array
// on every page. A missing, null, or corrupt later page cannot hide a check.
func recheckJobs(out []byte, bridges bool, expectedHead string) ([]scm.Check, error) {
	decoder := json.NewDecoder(bytes.NewReader(out))
	pages := 0
	var checks []scm.Check
	for {
		var page []struct {
			gitlabJob
			Downstream *struct {
				SHA    string `json:"sha"`
				Status string `json:"status"`
			} `json:"downstream_pipeline"`
		}
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return nil, fmt.Errorf("unreadable GitLab job page: %v", err)
		}
		pages++
		for _, job := range page {
			if job.ID <= 0 || job.Name == "" || job.Status == "" {
				return nil, fmt.Errorf("incomplete GitLab job result")
			}
			if bridges && job.Status == "success" && (job.Downstream == nil || job.Downstream.SHA != expectedHead || job.Downstream.Status != "success") {
				return nil, fmt.Errorf("bridge %s has no successful downstream at the expected head", job.Name)
			}
			checks = append(checks, jobsToChecks([]gitlabJob{job.gitlabJob})...)
		}
	}
	if pages == 0 {
		return nil, fmt.Errorf("GitLab returned no job pages")
	}
	return checks, nil
}

func (h *Host) recheckMRHead(ctx context.Context, pr *scm.PR, expectedHead string) (int, error) {
	out, err := h.cmd(ctx, "glab", "mr", "view", pr.Number, "--output", "json").Output()
	if err != nil {
		return 0, fmt.Errorf("read GitLab MR head: %w", err)
	}
	var mr struct {
		SHA          string `json:"sha"`
		State        string `json:"state"`
		HeadPipeline struct {
			ID  int    `json:"id"`
			SHA string `json:"sha"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal(bytesTrimToJSON(out), &mr); err != nil {
		return 0, err
	}
	if mr.State != "opened" || mr.SHA != expectedHead || mr.HeadPipeline.SHA != expectedHead || mr.HeadPipeline.ID <= 0 {
		return 0, fmt.Errorf("MR or pipeline is not open at expected head %s", expectedHead)
	}
	return mr.HeadPipeline.ID, nil
}

package firewall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// CheckOptions configure github-check: local private JSON, optional portal ingest.
type CheckOptions struct {
	PortalURL   string
	PortalToken string
	PrivateJSON string
	StorePath   string
	HTTPClient  *http.Client
}

// Exit codes. Both non-zero codes fail closed; they differ only in what the
// caller may claim about the pull request. ExitViolation is a Hard Rules
// verdict the scanner actually reached. ExitError is the firewall failing to
// judge or to record a verdict - an unreadable diff, a refused or unreachable
// portal - which must not be reported as a violation of the Hard Rules.
const (
	ExitViolation = 1
	ExitError     = 2
)

// GitHubCheck scans, writes only generic stdout, optionally stores and ingests.
// Exit 0 is a clean scan, ExitViolation is a finding, and ExitError is any
// failure to judge or record one. Stdout never includes match snippets.
func GitHubCheck(in Input, opts CheckOptions) (stdout string, exit int, err error) {
	res := Scan(in)
	failed := res.Failed()

	var stored *Verdict
	if opts.StorePath != "" {
		store, openErr := OpenStore(opts.StorePath)
		if openErr != nil {
			return PublicErrorText(opts.PortalURL), ExitError, fmt.Errorf("open store: %w", openErr)
		}
		defer store.Close()
		stored, openErr = store.Insert(in, res, opts.PortalURL)
		if openErr != nil {
			return PublicErrorText(opts.PortalURL), ExitError, fmt.Errorf("store verdict: %w", openErr)
		}
	}

	portalShown := opts.PortalURL
	if stored != nil && stored.PortalURL != "" {
		portalShown = stored.PortalURL
	}

	if opts.PrivateJSON != "" {
		payload := map[string]any{
			"repo":            in.Repo,
			"branch":          in.Branch,
			"head_sha":        in.HeadSHA,
			"base_sha":        in.BaseSHA,
			"pr_url":          in.PRURL,
			"pr_number":       in.PRNumber,
			"conclusion":      res.Conclusion(),
			"findings":        res.Findings,
			"error":           res.Error,
			"title":           in.Title,
			"body":            in.Body,
			"commit_messages": in.CommitMessages,
			"diff":            in.Diff,
		}
		if stored != nil {
			payload["id"] = stored.ID
			payload["portal_url"] = stored.PortalURL
		}
		b, mErr := json.MarshalIndent(payload, "", "  ")
		if mErr != nil {
			return PublicErrorText(opts.PortalURL), ExitError, mErr
		}
		if wErr := os.WriteFile(opts.PrivateJSON, b, 0o600); wErr != nil {
			return PublicErrorText(opts.PortalURL), ExitError, wErr
		}
	}

	if strings.TrimSpace(opts.PortalURL) != "" {
		ingested, iErr := ingest(opts, in, res)
		if iErr != nil {
			return PublicErrorText(opts.PortalURL), ExitError, fmt.Errorf("portal ingest: %w", iErr)
		}
		if ingested != "" {
			portalShown = ingested
		}
	}

	if res.Error != "" {
		return PublicErrorText(portalShown), ExitError, fmt.Errorf("scan diff: %s", res.Error)
	}

	stdout = PublicText(portalShown, failed)

	if failed {
		return stdout, ExitViolation, nil
	}
	return stdout, 0, nil
}

func ingest(opts CheckOptions, in Input, res Result) (string, error) {
	body, err := json.Marshal(ingestRequest{
		Repo:           in.Repo,
		Branch:         in.Branch,
		HeadSHA:        in.HeadSHA,
		BaseSHA:        in.BaseSHA,
		PRURL:          in.PRURL,
		PRNumber:       in.PRNumber,
		Title:          in.Title,
		Body:           in.Body,
		CommitMessages: in.CommitMessages,
		Diff:           in.Diff,
		Findings:       res.Findings,
		Error:          res.Error,
	})
	if err != nil {
		return "", err
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	url := strings.TrimRight(opts.PortalURL, "/") + "/v1/firewall/verdicts"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.PortalToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.PortalToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("portal status %d", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &created)
	if created.ID != "" {
		return JoinPortalURL(opts.PortalURL, created.ID), nil
	}
	return opts.PortalURL, nil
}

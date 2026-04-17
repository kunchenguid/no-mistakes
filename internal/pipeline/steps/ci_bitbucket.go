package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
)

// resolveBitbucketRepoRef parses a Bitbucket repo reference from the upstream
// URL, falling back to the PR URL when the upstream is not a Bitbucket URL.
func resolveBitbucketRepoRef(upstreamURL string, prURL *string) (bitbucket.RepoRef, error) {
	if repo, err := bitbucket.ParseRepoRef(upstreamURL); err == nil {
		return repo, nil
	}
	if prURL != nil && strings.TrimSpace(*prURL) != "" {
		return bitbucket.ParseRepoRef(*prURL)
	}
	return bitbucket.RepoRef{}, fmt.Errorf("resolve Bitbucket repository from upstream %q", upstreamURL)
}

// trimLogOutput truncates log output to the last maxBytes bytes, respecting
// UTF-8 boundaries at the truncation point.
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

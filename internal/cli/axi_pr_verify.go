package cli

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiPRVerifyCmd() *cobra.Command {
	return newAxiPRVerifyCmdWithReader(func(ctx context.Context, repo string) (steps.PRAppendixReader, error) {
		p, err := paths.New()
		if err != nil {
			return nil, err
		}
		global, err := config.LoadGlobal(p.ConfigFile())
		if err != nil {
			return nil, err
		}
		return steps.NewPRAppendixReader(ctx, config.Merge(global, &config.RepoConfig{}), repo)
	})
}

func newAxiPRVerifyCmdWithReader(resolve func(context.Context, string) (steps.PRAppendixReader, error)) *cobra.Command {
	var repo, pr string
	cmd := &cobra.Command{
		Use: "pr-verify", Short: "Read a live PR and verify its owned appendix without writing",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, pr = strings.TrimSpace(repo), strings.TrimSpace(pr)
			if !validPRVerifyRepo(repo) {
				return emitError(cmd, 2, "--repo requires the repository's full remote URL")
			}
			if n, err := strconv.ParseUint(pr, 10, 64); err != nil || n == 0 {
				return emitError(cmd, 2, "--pr requires a positive PR number")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			reader, err := resolve(ctx, repo)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			content, err := reader(ctx, pr)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			result := steps.VerifyPRAppendix(content.Body)
			emitDoc(cmd, toon.Field{Key: "pr", Value: pr}, toon.Field{Key: "verdict", Value: result.Verdict}, toon.Field{Key: "reason", Value: result.Reason})
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "full repository remote URL (required)")
	cmd.Flags().StringVar(&pr, "pr", "", "PR number to read (required)")
	return cmd
}

func validPRVerifyRepo(repo string) bool {
	parsed := repo
	if !strings.Contains(repo, "://") {
		authority, path, ok := strings.Cut(repo, ":")
		if !ok || strings.ContainsAny(authority, "/\\") || strings.HasPrefix(path, "/") {
			return false
		}
		parsed = "ssh://" + authority + "/" + path
	}
	u, err := url.Parse(parsed)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
	default:
		return false
	}
	parts := strings.Split(scm.RepoPath(repo), "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "\\") {
			return false
		}
	}
	return true
}

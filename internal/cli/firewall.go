package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/firewall"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newFirewallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firewall",
		Short: "Publish-policy scanner and LAN portal for public product repos",
	}
	cmd.AddCommand(newFirewallScanCmd())
	cmd.AddCommand(newFirewallGitHubCheckCmd())
	cmd.AddCommand(newFirewallServeCmd())
	return cmd
}

func newFirewallScanCmd() *cobra.Command {
	var diffFile, title, body string
	cmd := &cobra.Command{
		Use:           "scan",
		Short:         "Scan a unified diff on the LAN (prints findings; not for GitHub logs)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			diff, err := readDiffFlag(diffFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			res := firewall.Scan(firewall.Input{Diff: diff, Title: title, Body: body})
			if !res.Failed() {
				fmt.Fprint(cmd.OutOrStdout(), "publish-policy ok\n")
				return nil
			}
			rows := make([]firewall.AxiFinding, 0, len(res.Findings))
			for _, f := range res.Findings {
				rows = append(rows, firewall.AxiFinding{
					ID: f.ID, Severity: f.Severity, File: f.File, Line: f.Line,
					Action: f.Action, Class: string(f.Class), Description: f.Description,
				})
			}
			emitDoc(cmd, toon.Field{Key: "conclusion", Value: res.Conclusion()}, toon.Field{Key: "findings", Value: rows})
			return &exitError{code: 1}
		},
	}
	cmd.Flags().StringVar(&diffFile, "diff", "-", "unified diff file, or - for stdin")
	cmd.Flags().StringVar(&title, "title", "", "pull request title")
	cmd.Flags().StringVar(&body, "body", "", "pull request body")
	return cmd
}

func newFirewallGitHubCheckCmd() *cobra.Command {
	var (
		diffFile, titleFile, bodyFile, commitsFile string
		repo, prURL, branch, head, base            string
		prNumber                                   int
		portalURL, portalToken, privateJSON        string
	)
	cmd := &cobra.Command{
		Use:           "github-check",
		Short:         "Fail-closed GitHub check: generic stdout, optional LAN ingest",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			diff, err := readDiffFlag(diffFile, cmd.InOrStdin())
			if err != nil {
				fmt.Fprint(cmd.OutOrStdout(), firewall.PublicText(portalURL, true))
				return &exitError{code: 1, err: err}
			}
			title, _ := readOptional(titleFile)
			body, _ := readOptional(bodyFile)
			commits, _ := readLines(commitsFile)
			portal := firstNonEmpty(portalURL, os.Getenv("NO_MISTAKES_FIREWALL_PORTAL_URL"))
			opts := firewall.CheckOptions{
				PortalURL:   portal,
				PortalToken: firstNonEmpty(portalToken, os.Getenv("NO_MISTAKES_FIREWALL_INGEST_TOKEN")),
				PrivateJSON: privateJSON,
			}
			if portal == "" {
				p, err := paths.New()
				if err != nil {
					fmt.Fprint(cmd.OutOrStdout(), firewall.PublicText("", true))
					return &exitError{code: 1, err: err}
				}
				opts.StorePath = p.FirewallDB()
			}
			stdout, exit, err := firewall.GitHubCheck(firewall.Input{
				Diff:           diff,
				Title:          title,
				Body:           body,
				CommitMessages: commits,
				Repo:           repo,
				PRURL:          prURL,
				PRNumber:       prNumber,
				HeadSHA:        head,
				BaseSHA:        base,
				Branch:         branch,
			}, opts)
			fmt.Fprint(cmd.OutOrStdout(), stdout)
			if err != nil {
				return &exitError{code: 1}
			}
			if exit != 0 {
				return &exitError{code: exit}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&diffFile, "diff", "-", "unified diff file, or - for stdin")
	cmd.Flags().StringVar(&titleFile, "title-file", "", "file containing the PR title")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "file containing the PR body")
	cmd.Flags().StringVar(&commitsFile, "commits-file", "", "file containing commit messages, one per line")
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name")
	cmd.Flags().StringVar(&prURL, "pr-url", "", "pull request URL")
	cmd.Flags().IntVar(&prNumber, "pr-number", 0, "pull request number")
	cmd.Flags().StringVar(&branch, "branch", "", "head branch")
	cmd.Flags().StringVar(&head, "head", "", "head SHA")
	cmd.Flags().StringVar(&base, "base", "", "base SHA")
	cmd.Flags().StringVar(&portalURL, "portal-url", "", "LAN portal base URL")
	cmd.Flags().StringVar(&portalToken, "portal-token", "", "ingest bearer token")
	cmd.Flags().StringVar(&privateJSON, "private-json", "", "LAN-only JSON path (never print this file)")
	return cmd
}

func newFirewallServeCmd() *cobra.Command {
	var listen, portalURL, token string
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Serve the LAN portal API (does not start the pipeline daemon)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			store, err := firewall.OpenStore(p.FirewallDB())
			if err != nil {
				return err
			}
			defer store.Close()
			addr := firstNonEmpty(listen, os.Getenv("NO_MISTAKES_FIREWALL_LISTEN"), firewall.DefaultListen)
			srv := &firewall.Server{
				Store:      store,
				PortalBase: firstNonEmpty(portalURL, os.Getenv("NO_MISTAKES_FIREWALL_PORTAL_URL")),
				IngestTok:  firstNonEmpty(token, os.Getenv("NO_MISTAKES_FIREWALL_INGEST_TOKEN")),
				Listen:     addr,
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "firewall portal listening on %s (pipeline daemon untouched)\n", addr)
			return srv.ListenAndServe()
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "listen address (default 127.0.0.1:8787)")
	cmd.Flags().StringVar(&portalURL, "portal-url", "", "absolute LAN URL prefix advertised in public summaries")
	cmd.Flags().StringVar(&token, "ingest-token", "", "optional bearer token required for POST /v1/firewall/verdicts")
	return cmd
}

func newAxiFirewallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firewall",
		Short: "Axi-shaped publish-firewall records on the LAN portal store",
	}
	cmd.AddCommand(newAxiFirewallStatusCmd())
	cmd.AddCommand(newAxiFirewallRespondCmd())
	cmd.AddCommand(newAxiFirewallLogsCmd())
	return cmd
}

func newAxiFirewallStatusCmd() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Show a firewall verdict as an axi run",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openFirewallStore()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer store.Close()
			if id == "" {
				list, err := store.List(10)
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				rows := make([]runRow, 0, len(list))
				for _, v := range list {
					rows = append(rows, runRow{ID: v.ID, Branch: v.Branch, Status: v.Status, Head: v.HeadSHA, PR: v.PRURL})
				}
				emitDoc(cmd, toon.Field{Key: "count", Value: len(rows)}, toon.Field{Key: "runs", Value: rows})
				return nil
			}
			v, err := store.Get(id)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			run := v.AxiRun()
			emitDoc(cmd, toon.Field{Key: "run", Value: run})
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "run", "", "verdict / run id")
	return cmd
}

func newAxiFirewallRespondCmd() *cobra.Command {
	var id, action string
	cmd := &cobra.Command{
		Use:           "respond",
		Short:         "Record an acknowledgement; does not pass the GitHub check",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return emitError(cmd, 2, "--run is required")
			}
			store, err := openFirewallStore()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer store.Close()
			v, err := store.Respond(id, action)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			emitDoc(cmd,
				toon.Field{Key: "run", Value: v.AxiRun()},
				toon.Field{Key: "responded", Value: true},
				toon.Field{Key: "conclusion", Value: v.Conclusion},
				toon.Field{Key: "help", Value: []string{"Acknowledgement recorded; the GitHub check stays failed until a new head SHA is scanned"}},
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "run", "", "verdict / run id")
	cmd.Flags().StringVar(&action, "action", "acknowledge", "respond action (acknowledge only)")
	return cmd
}

func newAxiFirewallLogsCmd() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:           "logs",
		Short:         "Show generic firewall logs for a verdict",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return emitError(cmd, 2, "--run is required")
			}
			store, err := openFirewallStore()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer store.Close()
			lines, err := store.Logs(id)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			rows := make([]logRow, 0, len(lines))
			for _, line := range lines {
				rows = append(rows, logRow{Line: line})
			}
			emitDoc(cmd, toon.Field{Key: "logs", Value: rows})
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "run", "", "verdict / run id")
	return cmd
}

func openFirewallStore() (*firewall.Store, error) {
	p, err := paths.New()
	if err != nil {
		return nil, err
	}
	return firewall.OpenStore(p.FirewallDB())
}

func readDiffFlag(path string, stdin io.Reader) (string, error) {
	if path == "" || path == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		if len(bytesTrim(b)) == 0 {
			return "", fmt.Errorf("empty diff")
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func readOptional(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readLines(path string) ([]string, error) {
	s, err := readOptional(path)
	if err != nil || s == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n"), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

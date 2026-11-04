package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/spf13/cobra"
)

// The supplied PR title and description each travel as one base64 push
// option, like intent.
const (
	prTitlePushOptionPrefix = "no-mistakes.pr-title="
	prBodyPushOptionPrefix  = "no-mistakes.pr-body="
)

// maxAxiRunPRBodyBytes is the pkt-line budget for the description's push
// option (see maxAxiRunIntentBytes); steps.MaxRunPRBodyBytes is the daemon's
// copy of the same limit.
const maxAxiRunPRBodyBytes = (65516 - len(prBodyPushOptionPrefix)) / 4 * 3

// bindPRTextFlags adds --pr-title and --pr-body-file. There is deliberately no
// inline body flag: Markdown is exactly the text a caller's shell would expand.
func bindPRTextFlags(cmd *cobra.Command, title, bodyFile *string) {
	cmd.Flags().StringVar(title, "pr-title", "", "PR title to use verbatim when this run creates the PR (pr.title_format still applies)")
	cmd.Flags().StringVar(bodyFile, "pr-body-file", "", "read the PR description to use verbatim when this run creates the PR from this file; '-' reads stdin to EOF")
}

// resolvePRTextFlags reads and validates --pr-title and --pr-body-file before
// any run resource opens, so a bad value never pushes or starts a run.
// intentReadsStdin reports that --intent already claims stdin.
func resolvePRTextFlags(cmd *cobra.Command, title, bodyFile string, intentReadsStdin bool) (db.RunPRText, error) {
	var text db.RunPRText
	if cmd.Flags().Changed("pr-title") {
		normalized, err := steps.NormalizeRunPRTitle(title)
		if err == nil && normalized == "" {
			err = errors.New("must not be empty or whitespace-only")
		}
		if err != nil {
			return db.RunPRText{}, fmt.Errorf("--pr-title %w", err)
		}
		text.Title = normalized
	}
	if cmd.Flags().Changed("pr-body-file") {
		var data []byte
		var err error
		switch bodyFile {
		case "":
			return db.RunPRText{}, errors.New("--pr-body-file requires a file path, or - for stdin")
		case "-":
			if intentReadsStdin {
				return db.RunPRText{}, errors.New("--pr-body-file - and --intent - cannot both read stdin; pass one of them as a file")
			}
			data, err = readAxiPRBody(cmd.InOrStdin())
		default:
			data, err = readAxiRegularFile(bodyFile, "--pr-body-file", readAxiPRBody)
		}
		if err != nil {
			return db.RunPRText{}, fmt.Errorf("read --pr-body-file: %w", err)
		}
		body := string(data)
		err = steps.ValidateRunPRBody(body)
		if err == nil && body == "" {
			err = errors.New("must not be empty or whitespace-only")
		}
		if err != nil {
			return db.RunPRText{}, fmt.Errorf("--pr-body-file %w", err)
		}
		text.Body = body
	}
	return text, nil
}

func readAxiPRBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxAxiRunPRBodyBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAxiRunPRBodyBytes {
		return nil, fmt.Errorf("input exceeds the %d-byte PR description transport limit", maxAxiRunPRBodyBytes)
	}
	return data, nil
}

func hasRunPRText(text db.RunPRText) bool {
	return text.Title != "" || text.Body != ""
}

func formatPRTextPushOptions(text db.RunPRText) []string {
	var options []string
	if opt := formatOpaquePushOption(prTitlePushOptionPrefix, text.Title); opt != "" {
		options = append(options, opt)
	}
	if opt := formatOpaquePushOption(prBodyPushOptionPrefix, text.Body); opt != "" {
		options = append(options, opt)
	}
	return options
}

// parsePRTextPushOptions decodes the supplied PR text push options. A repeated
// option must repeat the same value; the daemon validates the text itself.
func parsePRTextPushOptions(options []string) (db.RunPRText, error) {
	title, err := parseOpaquePushOptions(options, prTitlePushOptionPrefix, "pr title")
	if err != nil {
		return db.RunPRText{}, err
	}
	body, err := parseOpaquePushOptions(options, prBodyPushOptionPrefix, "pr body")
	if err != nil {
		return db.RunPRText{}, err
	}
	return db.RunPRText{Title: title, Body: body}, nil
}

// requireDaemonHonorsRunPRText refuses supplied PR text before any push when
// the running daemon predates it: it would drop the unknown fields and open a
// PR with drafted text. The probe is a method such a daemon refuses.
func requireDaemonHonorsRunPRText(client closingIssueRefsUpdateClient, text db.RunPRText) error {
	if !hasRunPRText(text) {
		return nil
	}
	var result ipc.ProbeRunPRTextResult
	err := client.Call(ipc.MethodProbeRunPRText, &ipc.ProbeRunPRTextParams{}, &result)
	if err == nil && !result.OK {
		err = errors.New("daemon declined the supplied PR text capability")
	}
	if err != nil {
		return fmt.Errorf("the running daemon is too old to honor --pr-title or --pr-body-file (%v); restart it with `no-mistakes daemon restart` so the current binary serves it", err)
	}
	return nil
}

// conflictingActiveRunPRText reports when supplied PR text would be discarded
// by reattaching to an in-flight run that carries different (or no) text.
func conflictingActiveRunPRText(run *ipc.RunInfo, text db.RunPRText) error {
	if run == nil {
		return nil
	}
	if text.Title != "" && run.PRTitleDigest != db.PRTextDigest(text.Title, "") {
		return fmt.Errorf("active run %s is already in progress without this --pr-title", run.ID)
	}
	if text.Body != "" && run.PRBodyDigest != db.PRTextDigest("", text.Body) {
		return fmt.Errorf("active run %s is already in progress without this --pr-body-file description", run.ID)
	}
	return nil
}

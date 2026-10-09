package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ProtocolVersion is the provider-plugin contract this build speaks. A plugin
// must report it from "status"; any other value fails closed so a plugin
// written against a different contract is never half-understood.
// docs/src/content/docs/reference/provider-plugin-protocol.md owns the public
// contract.
const ProtocolVersion = 1

// Subcommands. A plugin is an AXI-shaped CLI, driven the way no-mistakes
// drives forgejo-axi: every invocation is
//
//	<command> <configured args...> <subcommand words> [operation args] [context flags] --json
//
// and prints exactly one JSON document. The words are split on spaces when
// the argv is built.
const (
	CmdStatus         = "status"
	CmdPRFind         = "pr find"
	CmdPRCreate       = "pr create"
	CmdPRUpdate       = "pr update"
	CmdPRView         = "pr view"
	CmdPRChecks       = "pr checks"
	CmdPRMergeability = "pr mergeability"
	CmdPRCheckLogs    = "pr check-logs"
	CmdPRMerged       = "pr merged"
	CmdPRRetarget     = "pr retarget"
)

// Error codes a plugin may report in its failure document. Unknown codes are
// ordinary failures.
const (
	// ErrorCodeUnsupported is never a valid answer: no-mistakes only invokes
	// required subcommands and the ones a plugin declared in "status", so a
	// plugin that refuses one contradicts its own handshake. It surfaces as
	// ErrProtocol (a capability mismatch), never as scm.ErrUnsupported,
	// which would let a caller quietly fall back to a weaker path.
	ErrorCodeUnsupported = "unsupported"
	// ErrorCodeHeadChanged maps to scm.ErrHeadChanged ("pr merged").
	ErrorCodeHeadChanged = "head_changed"
)

// ErrProtocol marks a plugin that broke this contract: output no-mistakes
// cannot validate (empty, non-JSON, a second document, a missing required
// key, a mismatched PR identity, an unknown enum value), a protocol version
// it does not speak, a capability it declared but then refused, a success
// exit that carries an error object, oversized output, or an invocation that
// outlived its timeout. Steps fail on it instead of skipping: a skip would
// let a run read as passed-with-skips off an answer nothing validated. A
// plugin that exits non-zero to say it cannot serve the repository (for
// example it is not authenticated) is not a protocol violation; that skips
// with the plugin's own message, like a built-in provider whose CLI is not
// logged in.
var ErrProtocol = errors.New("provider plugin protocol violation")

// ErrTimeout additionally marks the ErrProtocol of an invocation that
// outlived its timeout. It is still a violation wherever one fails the step;
// only the CI monitor's repeated polls and its failed-log retrieval tolerate
// it, because a slow forge during a days-long watch is a transient read
// failure that is never read as a pass, while malformed output would fail
// identically on every poll.
var ErrTimeout = errors.New("provider plugin timed out")

// Repository identifies the repository a run targets. It is sent as context
// flags on every invocation. RemoteURL is the credential-redacted upstream
// remote; Host is the remote host after SSH HostName resolution and RawHost
// the literal host token in the remote.
type Repository struct {
	RemoteURL string
	Host      string
	RawHost   string
	Path      string
}

// contextArgs returns the flags every invocation carries, mirroring
// forgejo-axi's --repo. Values always use the single-argument --flag=value
// form so a value beginning with "-" can never be parsed as an option.
func contextArgs(plugin string, repo Repository) []string {
	return []string{
		flag("plugin", plugin),
		flag("repo", repo.Path),
		flag("host", repo.Host),
		flag("raw-host", repo.RawHost),
		flag("remote-url", repo.RemoteURL),
	}
}

func flag(name, value string) string { return "--" + name + "=" + value }

// commandWords splits a Cmd* constant into argv words.
func commandWords(command string) []string { return strings.Fields(command) }

// failureDocument is what a plugin may print on stdout with a non-zero exit.
type failureDocument struct {
	Error *ResponseError `json:"error"`
}

// ResponseError is a plugin-reported failure.
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// statusResult is the handshake result.
type statusResult struct {
	ProtocolVersion int `json:"protocol_version"`
	Capabilities    struct {
		MergeableState  bool `json:"mergeable_state"`
		FailedCheckLogs bool `json:"failed_check_logs"`
		MergedProof     bool `json:"merged_proof"`
		SetPRBaseBranch bool `json:"set_pr_base_branch"`
	} `json:"capabilities"`
	// MaxPRBodyChars is required: a missing key must not decode to the zero
	// that means "unlimited" and let publication skip the plugin's limit.
	MaxPRBodyChars *int `json:"max_pr_body_chars"`
}

// wirePR is a PR as a plugin reports it.
type wirePR struct {
	Number     prNumber `json:"number"`
	URL        string   `json:"url"`
	HeadBranch string   `json:"head_branch"`
	BaseBranch string   `json:"base_branch"`
	HeadSHA    string   `json:"head_sha"`
}

type prResult struct {
	PR *wirePR `json:"pr"`
}

// findResult keeps the raw "pr" value so a missing key is told apart from an
// explicit null: only null means "no open PR", and reading {} as that would
// let the PR step create a duplicate.
type findResult struct {
	PR json.RawMessage `json:"pr"`
}

type viewResult struct {
	PR    *wirePR `json:"pr"`
	State string  `json:"state"`
	Title *string `json:"title"`
	Body  *string `json:"body"`
}

type wireCheck struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	Bucket      string `json:"bucket"`
	State       string `json:"state"`
	Link        string `json:"link"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	ExecutionID string `json:"execution_id"`
}

type checksResult struct {
	Checks *[]wireCheck `json:"checks"`
}

type mergeabilityResult struct {
	State string `json:"state"`
}

type checkLogsResult struct {
	Logs *string `json:"logs"`
}

type mergedResult struct {
	Merged         bool     `json:"merged"`
	Number         prNumber `json:"number"`
	URL            string   `json:"url"`
	HeadSHA        string   `json:"head_sha"`
	MergeCommitSHA string   `json:"merge_commit_sha"`
	MergedAt       string   `json:"merged_at"`
	MergedBy       string   `json:"merged_by"`
}

// prNumber accepts a PR number written either as a JSON string or a JSON
// integer, because both spellings are natural for plugin authors. It always
// normalizes to a positive base-10 string.
type prNumber string

func (n *prNumber) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*n = ""
		return nil
	}
	var text string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		text = string(data)
	}
	value, err := strconv.ParseUint(text, 10, 63)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != text {
		return fmt.Errorf("PR number %s must be a positive base-10 integer", data)
	}
	*n = prNumber(text)
	return nil
}

// decodeResult validates a success document and decodes it into dst. The
// document must be exactly one JSON object; unknown fields are tolerated so
// plugins can add their own diagnostics, but trailing data is not (a second
// document is output nothing can attribute), and a top-level "error" key is
// reserved for failures, so a plugin that exits 0 while reporting an error is
// refused rather than read as a result.
func decodeResult(stdout []byte, dst any) error {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return errors.New("exited 0 with empty stdout (want one JSON object)")
	}
	var raw json.RawMessage
	if err := decodeSingle(trimmed, &raw); err != nil {
		return fmt.Errorf("exited 0 with invalid JSON output: %w", err)
	}
	if raw[0] != '{' {
		return errors.New("exited 0 but the output is not a JSON object")
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("exited 0 with invalid JSON output: %w", err)
	}
	if len(probe.Error) > 0 && !bytes.Equal(probe.Error, []byte("null")) {
		var reported failureDocument
		message := ""
		if json.Unmarshal(raw, &reported) == nil && reported.Error != nil {
			message = ": " + boundedText(reported.Error.Message)
		}
		return fmt.Errorf("exited 0 but reported an error%s (failures must exit non-zero)", message)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("returned an invalid result: %w", err)
	}
	return nil
}

// decodeFailure reads an optional failure document from the stdout of a
// non-zero exit. It returns nil when stdout is not exactly one failure
// document, in which case the caller reports stderr instead.
func decodeFailure(stdout []byte) *ResponseError {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil
	}
	var doc failureDocument
	if err := decodeSingle(trimmed, &doc); err != nil {
		return nil
	}
	return doc.Error
}

// decodeSingle decodes exactly one JSON value into dst and refuses trailing
// data.
func decodeSingle(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == nil:
		return errors.New("unexpected data after the JSON document")
	case errors.Is(err, io.EOF):
		return nil
	default:
		return err
	}
}

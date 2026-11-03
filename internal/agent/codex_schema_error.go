package agent

import (
	"encoding/json"
	"strings"
)

// A provider rejecting the request's schema cannot be repaired by retrying the
// analyzer or by parsing an unconstrained response. Keep the original error for
// diagnostics, but make the route incompatibility and next action explicit.
type codexSchemaIncompatibility struct{ err error }

func (e *codexSchemaIncompatibility) Error() string {
	return "codex structured-output incompatibility: the configured provider does not accept the structured-output schema sent via --output-schema; select a compatible Codex profile/model or configure a different agent route, then rerun the pipeline so review can complete: " + e.err.Error()
}

func (e *codexSchemaIncompatibility) Unwrap() error             { return e.err }
func (*codexSchemaIncompatibility) TerminalProviderError() bool { return true }

func codexRejectsOutputSchema(detail string) bool {
	detail = strings.TrimSpace(detail)
	// Some Codex versions forward only the provider's message. This prefix is
	// deliberately narrow: a model, auth, tool-input, or local schema-file error
	// must not be relabeled as a provider structured-output incompatibility.
	invalidSchemaMessage := func(message string) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), "invalid json schema:")
	}
	if invalidSchemaMessage(detail) {
		return true
	}
	// Codex may prefix the provider body with an HTTP status and append a URL
	// or other diagnostics. Decode the envelope without requiring those suffixes
	// to be JSON. Classification uses provider fields rather than model names or
	// the particular array/type diagnostic reported in issue #1207.
	start := strings.IndexByte(detail, '{')
	if start < 0 {
		return false
	}
	var response struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(detail[start:])).Decode(&response); err != nil || response.Error == nil {
		return false
	}
	failure := response.Error
	if failure.Code == "invalid_json_schema" {
		return true
	}
	if failure.Type != "invalid_request_error" && failure.Code != "invalid_request_error" {
		return false
	}
	return invalidSchemaMessage(failure.Message) ||
		failure.Param == "response_format" || strings.HasPrefix(failure.Param, "response_format.") ||
		failure.Param == "text.format" || strings.HasPrefix(failure.Param, "text.format.")
}

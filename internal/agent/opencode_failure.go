package agent

import (
	"errors"
	"fmt"
	"strings"
)

// opencodeMessageFailure is a turn that opencode completed with an error on
// the assistant message. It carries opencode's own retryability verdict so
// the retry loop can repeat a provider blip without ever repeating a request
// the provider rejected as invalid.
type opencodeMessageFailure struct {
	name       string
	message    string
	statusCode int
	retries    int
	retryable  bool
	structured bool
}

func newOpencodeMessageFailure(e *opencodeMessageError) error {
	if e == nil {
		return nil
	}
	return &opencodeMessageFailure{
		name:       e.Name,
		message:    e.message(),
		statusCode: e.statusCode(),
		retries:    e.retries(),
		retryable:  e.retryable(),
		structured: e.IsStructuredOutput(),
	}
}

func (e *opencodeMessageFailure) Error() string {
	// StructuredOutputError keeps its own wording: the actionable fact is
	// that opencode already spent its internal retries trying to make the
	// model call the StructuredOutput tool.
	if e.structured {
		return fmt.Sprintf("opencode structured output failed after %d internal retries: %s",
			e.retries, e.detail())
	}
	name := e.name
	if name == "" {
		name = "error"
	}
	if e.statusCode != 0 {
		return fmt.Sprintf("opencode %s (status %d): %s", name, e.statusCode, e.detail())
	}
	return fmt.Sprintf("opencode %s: %s", name, e.detail())
}

func (e *opencodeMessageFailure) detail() string {
	if msg := strings.TrimSpace(e.message); msg != "" {
		return msg
	}
	return "no detail reported"
}

// label is the short telemetry tag for a retried failure.
func (e *opencodeMessageFailure) label() string {
	name := e.name
	if name == "" {
		name = "message error"
	}
	if e.statusCode != 0 {
		return fmt.Sprintf("opencode %s %d", name, e.statusCode)
	}
	return "opencode " + name
}

// classifyOpencodeTransient extends the shared transient classifier with
// opencode's assistant-message errors. When opencode reported the failure it
// is the authority on whether a retry is worthwhile, so a non-retryable one
// stops here rather than falling through to the substring matching - a 400
// body quoting a provider's own rate-limit prose must not look transient.
func classifyOpencodeTransient(err error) (string, bool) {
	var failure *opencodeMessageFailure
	if errors.As(err, &failure) {
		if failure.retryable {
			return failure.label(), true
		}
		return "", false
	}
	return classifyTransient(err)
}

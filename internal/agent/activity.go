package agent

// ActivityKind identifies one source of liveness evidence for a running agent
// invocation. The pipeline's invocation watchdog folds every reported kind
// into a single monotonic last-activity clock: an invocation is killed only
// after its full configured silence budget elapses with no activity from any
// source. Kinds are deliberately coarse - they carry no content, no paths,
// and no session payloads - so timeout diagnostics can name the evidence
// class without leaking prompt or session data.
type ActivityKind string

const (
	// ActivityStdout reports bytes visible on the agent's stdout pipe. This
	// is the wrapper's traditional evidence source; some harnesses (pi in
	// JSON mode) buffer stdout when piped, so a quiet pipe does not prove a
	// quiet agent.
	ActivityStdout ActivityKind = "stdout"
	// ActivityLifecycle reports native process lifecycle transitions
	// (process start, process exit). A start also resets the silence clock,
	// which is what gives every freshly launched process its own full
	// budget. Process existence alone is never activity.
	ActivityLifecycle ActivityKind = "lifecycle"
	// ActivitySession reports advancement of the exact adapter-native
	// session file bound to this invocation (pi's session JSONL). It is
	// credited only when the binding is unambiguous; otherwise the
	// invocation falls back to stdout/lifecycle evidence only.
	ActivitySession ActivityKind = "session"
)

// ActivityKindLabel renders a kind for operator-facing diagnostics. The
// labels name evidence classes, never content.
func ActivityKindLabel(kind ActivityKind) string {
	switch kind {
	case ActivityStdout:
		return "stdout bytes"
	case ActivityLifecycle:
		return "process lifecycle"
	case ActivitySession:
		return "pi session events"
	default:
		return string(kind)
	}
}

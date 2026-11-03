package types

// ResponseOrigin identifies the caller that produced a gate response. It is
// audit metadata only and must not influence gate resolution.
type ResponseOrigin string

const (
	ResponseOriginUnknown     ResponseOrigin = "unknown"
	ResponseOriginInteractive ResponseOrigin = "interactive"
	ResponseOriginAutoYes     ResponseOrigin = "auto_yes"
)

// OrUnknown leaves omitted origins explicitly unknown, never inferred from the
// action or selection source. Unrecognized client values are also unknown.
func (o ResponseOrigin) OrUnknown() ResponseOrigin {
	switch o {
	case ResponseOriginInteractive, ResponseOriginAutoYes:
		return o
	default:
		return ResponseOriginUnknown
	}
}

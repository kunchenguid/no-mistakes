package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// RunStatus represents the lifecycle state of a pipeline run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
	// RunCIMonitorInterrupted means the daemon restarted while babysitting an
	// already-created PR. The PR remains intact, so this is not a pipeline
	// failure.
	RunCIMonitorInterrupted RunStatus = "ci_monitor_interrupted"
)

const (
	RunCancelReasonAbortedByUser  = "cancelled: aborted by user"
	RunCancelReasonSuperseded     = "cancelled: superseded by new push"
	RunCIMonitorInterruptedReason = "ci monitor interrupted by daemon restart; PR remains open"
)

// Terminal reports whether the run has reached a final state the daemon will
// never advance further. This is the single source of truth for "is this run
// terminal": every enumeration of terminal statuses (branchsync custody
// recovery, the axi drive outcome check, the e2e harness wait loop) routes
// through it so a newly added terminal status can never drift out of sync.
// RunCIMonitorInterrupted is terminal - the daemon restarted mid-CI-monitor
// and the run is never resumed (issue #361) - so it must classify exactly like
// completed/failed/cancelled.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled, RunCIMonitorInterrupted:
		return true
	default:
		return false
	}
}

// StepName identifies a pipeline step.
type StepName string

const (
	StepIntent   StepName = "intent"
	StepRebase   StepName = "rebase"
	StepReview   StepName = "review"
	StepTest     StepName = "test"
	StepDocument StepName = "document"
	StepLint     StepName = "lint"
	StepPush     StepName = "push"
	StepPR       StepName = "pr"
	StepCI       StepName = "ci"
)

func normalizeStepName(s StepName) StepName {
	if s == "babysit" {
		return StepCI
	}
	return s
}

func (s *StepName) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = normalizeStepName(StepName(raw))
	return nil
}

func (s *StepName) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*s = normalizeStepName(StepName(v))
		return nil
	case []byte:
		*s = normalizeStepName(StepName(v))
		return nil
	case nil:
		*s = ""
		return nil
	default:
		return fmt.Errorf("scan StepName from %T", src)
	}
}

func (s StepName) Value() (driver.Value, error) {
	return string(s), nil
}

// StepOrder returns the fixed execution order for a step (1-indexed).
// A custom gate shares its anchor's order: it runs immediately after that core
// step, and a restart that resets from the anchor must reset the gate with it.
func (s StepName) Order() int {
	if anchor, ok := s.CustomGateAnchor(); ok {
		return anchor.Order()
	}
	switch s {
	case StepIntent:
		return 1
	case StepRebase:
		return 2
	case StepReview:
		return 3
	case StepTest:
		return 4
	case StepDocument:
		return 5
	case StepLint:
		return 6
	case StepPush:
		return 7
	case StepPR:
		return 8
	case StepCI:
		return 9
	default:
		return 0
	}
}

// AllSteps returns all pipeline steps in execution order.
func AllSteps() []StepName {
	return []StepName{StepIntent, StepRebase, StepReview, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
}

// CustomGateStepPrefix marks a step name as a repository-declared extra gate
// rather than one of the fixed core steps, and CustomGateStepSeparator joins
// the encoded anchor to the gate's label.
//
// The separator is '.' rather than ':' because a step name is used verbatim as
// a filename: the executor derives each step's log path from it, and Win32
// parses '<name>:<stream>:<type>' as an NTFS alternate data stream, so a
// colon-bearing name fails to open on Windows and takes the whole run down
// with it. '.' is a legal filename character on every supported platform, and
// neither a core step name nor a well-formed gate label can contain one, so
// the encoding stays reversible.
const (
	CustomGateStepPrefix    = "gate."
	CustomGateStepSeparator = "."
)

// MaxCustomGateLabelLen bounds a gate label so the derived step name stays a
// short, valid filename and a compact entry in the step tables, the PR body,
// and the attestation payload.
const MaxCustomGateLabelLen = 40

// ValidCustomGateLabel reports whether label is a well-formed gate label:
// non-empty, at most MaxCustomGateLabelLen bytes, and lowercase letters,
// digits, and inner hyphens only.
//
// This is the single owner of that syntax, because it is what makes a gate
// step name safe to join onto a path. Both the config that mints gate names
// and CustomGateAnchor, which decides whether an arbitrary string IS a gate
// step, answer from here, so a name that reports true for IsCustomGate can
// never carry a separator or a traversal segment into a log path.
func ValidCustomGateLabel(label string) bool {
	if label == "" || len(label) > MaxCustomGateLabelLen {
		return false
	}
	for i, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(label)-1:
		default:
			return false
		}
	}
	return true
}

// CustomGateStepName encodes a gate's anchor into its step name so Order can
// place the gate without consulting the configuration that declared it.
func CustomGateStepName(anchor StepName, name string) StepName {
	return StepName(CustomGateStepPrefix + string(anchor) + CustomGateStepSeparator + name)
}

// decodeCustomGate reverses CustomGateStepName and is the single owner of that
// decoding: IsCustomGate, CustomGateAnchor, and CustomGateLabel all answer from
// here, so the separator handling lives in one place and the three can never
// disagree about whether a name is a gate. It reports false for a core step,
// and for any name whose encoded anchor is not itself a core step or whose
// label is not well-formed, so a malformed name can never be ordered, or turned
// into a log path, as if it were valid.
func decodeCustomGate(s StepName) (StepName, string, bool) {
	rest, ok := strings.CutPrefix(string(s), CustomGateStepPrefix)
	if !ok {
		return "", "", false
	}
	anchor, label, ok := strings.Cut(rest, CustomGateStepSeparator)
	if !ok || !ValidCustomGateLabel(label) {
		return "", "", false
	}
	if !IsCoreStepName(StepName(anchor)) {
		return "", "", false
	}
	return StepName(anchor), label, true
}

// IsCustomGate reports whether the step is a repository-declared extra gate.
func (s StepName) IsCustomGate() bool {
	_, _, ok := decodeCustomGate(s)
	return ok
}

// CustomGateAnchor returns the core step a custom gate runs after.
func (s StepName) CustomGateAnchor() (StepName, bool) {
	anchor, _, ok := decodeCustomGate(s)
	return anchor, ok
}

// CustomGateLabel returns the repository-declared name of a custom gate, or
// empty for a core step or a malformed gate name.
func (s StepName) CustomGateLabel() string {
	_, label, _ := decodeCustomGate(s)
	return label
}

// IsCoreStepName reports whether the name is one of the fixed core steps.
func IsCoreStepName(s StepName) bool {
	for _, step := range AllSteps() {
		if step == s {
			return true
		}
	}
	return false
}

// StepStatus represents the lifecycle state of a pipeline step.
type StepStatus string

const (
	StepStatusPending          StepStatus = "pending"
	StepStatusRunning          StepStatus = "running"
	StepStatusAwaitingApproval StepStatus = "awaiting_approval"
	StepStatusFixing           StepStatus = "fixing"
	StepStatusFixReview        StepStatus = "fix_review"
	StepStatusCompleted        StepStatus = "completed"
	StepStatusSkipped          StepStatus = "skipped"
	StepStatusFailed           StepStatus = "failed"
)

// ApprovalAction represents user responses at approval points.
type ApprovalAction string

const (
	ActionApprove ApprovalAction = "approve"
	ActionFix     ApprovalAction = "fix"
	ActionSkip    ApprovalAction = "skip"
	ActionAbort   ApprovalAction = "abort"
)

// AgentName identifies a supported agent backend. Explicit ACP targets use
// dynamic acp:<target> values; first-class ACP aliases have constants below.
type AgentName string

const (
	AgentAuto        AgentName = "auto"
	AgentClaude      AgentName = "claude"
	AgentCodex       AgentName = "codex"
	AgentGrok        AgentName = "grok"
	AgentRovoDev     AgentName = "rovodev"
	AgentOpenCode    AgentName = "opencode"
	AgentPi          AgentName = "pi"
	AgentCopilot     AgentName = "copilot"
	AgentCursor      AgentName = "cursor"
	AgentAntigravity AgentName = "antigravity"
)

// ACPAlias describes a first-class agent name that resolves to an ACP target.
type ACPAlias struct {
	Name           AgentName
	Target         string
	DefaultCommand string
}

var acpAliases = []ACPAlias{
	{Name: AgentCursor, Target: "cursor", DefaultCommand: "cursor-agent acp"},
}

// ACPAliasFor returns the ACP alias metadata for a first-class agent name.
func ACPAliasFor(name AgentName) (ACPAlias, bool) {
	for _, alias := range acpAliases {
		if alias.Name == name {
			return alias, true
		}
	}
	return ACPAlias{}, false
}

// ACPAliasForTarget returns the ACP alias metadata for a raw ACP target.
func ACPAliasForTarget(target string) (ACPAlias, bool) {
	for _, alias := range acpAliases {
		if alias.Target == target {
			return alias, true
		}
	}
	return ACPAlias{}, false
}

// ACPAliases returns all first-class ACP aliases.
func ACPAliases() []ACPAlias {
	out := make([]ACPAlias, len(acpAliases))
	copy(out, acpAliases)
	return out
}

// DefaultCommandBinary returns the executable named by the alias default command.
func (a ACPAlias) DefaultCommandBinary() string {
	fields := strings.Fields(a.DefaultCommand)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ACPTargetFor resolves the ACP target an agent name drives: the alias target
// for a first-class alias, or the parsed target of an explicit acp:<target>
// name. Returns false for non-ACP agent names.
func ACPTargetFor(name AgentName) (string, bool) {
	if alias, ok := ACPAliasFor(name); ok {
		return alias.Target, true
	}
	value := string(name)
	if !strings.HasPrefix(value, "acp:") {
		return "", false
	}
	target := strings.TrimPrefix(value, "acp:")
	if target == "" || strings.ContainsAny(target, " \t\r\n") {
		return "", false
	}
	return target, true
}

// ACPRawCommand resolves the raw command acpx runs for an ACP target: a
// registry override is trimmed and wins when non-blank, otherwise the alias
// default command is used.
// Empty means acpx dispatches the target through its own registry.
func ACPRawCommand(target string, overrides map[string]string) string {
	if override := strings.TrimSpace(overrides[target]); override != "" {
		return override
	}
	if alias, ok := ACPAliasForTarget(target); ok {
		return alias.DefaultCommand
	}
	return ""
}

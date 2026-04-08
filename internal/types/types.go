package types

// RunStatus represents the lifecycle state of a pipeline run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// StepName identifies a pipeline step.
type StepName string

const (
	StepReview  StepName = "review"
	StepTest    StepName = "test"
	StepLint    StepName = "lint"
	StepPush    StepName = "push"
	StepPR      StepName = "pr"
	StepBabysit StepName = "babysit"
)

// StepOrder returns the fixed execution order for a step (1-indexed).
func (s StepName) Order() int {
	switch s {
	case StepReview:
		return 1
	case StepTest:
		return 2
	case StepLint:
		return 3
	case StepPush:
		return 4
	case StepPR:
		return 5
	case StepBabysit:
		return 6
	default:
		return 0
	}
}

// AllSteps returns all pipeline steps in execution order.
func AllSteps() []StepName {
	return []StepName{StepReview, StepTest, StepLint, StepPush, StepPR, StepBabysit}
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

// AgentName identifies a supported agent backend.
type AgentName string

const (
	AgentClaude   AgentName = "claude"
	AgentCodex    AgentName = "codex"
	AgentRovoDev  AgentName = "rovodev"
	AgentOpenCode AgentName = "opencode"
)

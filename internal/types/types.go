package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// RunStatus represents the lifecycle state of a pipeline run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

const (
	RunCancelReasonAbortedByUser = "cancelled: aborted by user"
	RunCancelReasonSuperseded    = "cancelled: superseded by new push"
)

// StepName identifies a pipeline step.
type StepName string

const (
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
func (s StepName) Order() int {
	switch s {
	case StepRebase:
		return 1
	case StepReview:
		return 2
	case StepTest:
		return 3
	case StepDocument:
		return 4
	case StepLint:
		return 5
	case StepPush:
		return 6
	case StepPR:
		return 7
	case StepCI:
		return 8
	default:
		return 0
	}
}

// AllSteps returns all pipeline steps in execution order.
func AllSteps() []StepName {
	return []StepName{StepRebase, StepReview, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
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

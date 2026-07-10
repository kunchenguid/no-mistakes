package types

import (
	"fmt"
	"strings"
)

// Purpose is the semantic reason for an agent invocation. It deliberately says
// nothing about runners, models, providers, effort, or native command-line
// arguments.
type Purpose string

const (
	PurposeInitialReview                  Purpose = "initial_review"
	PurposeStructuredFindingRepair        Purpose = "structured_finding_repair"
	PurposeIntentSensitiveRepair          Purpose = "intent_sensitive_repair"
	PurposeUnstructuredTestRepair         Purpose = "unstructured_test_repair"
	PurposeUnstructuredCIRepair           Purpose = "unstructured_ci_repair"
	PurposeUnstructuredConflictRepair     Purpose = "unstructured_conflict_repair"
	PurposeTestEvidence                   Purpose = "test_evidence"
	PurposeLintInspection                 Purpose = "lint_inspection"
	PurposeDocumentationAuthoring         Purpose = "documentation_authoring"
	PurposeDocumentationVerification      Purpose = "documentation_verification"
	PurposePRComposition                  Purpose = "pr_composition"
	PurposeIntentSummarization            Purpose = "intent_summarization"
	PurposeIntentDisambiguation           Purpose = "intent_disambiguation"
	PurposeBranchCommitSuggestion         Purpose = "branch_commit_suggestion"
	PurposeNormalAggregateVerification    Purpose = "normal_aggregate_verification"
	PurposeEscalatedAggregateVerification Purpose = "escalated_aggregate_verification"
	// Informational (non-blocking) repair uses the cheap two-tier cascade and
	// its own tools_balanced verifier, so it never reaches a Sol/Fable profile.
	PurposeInformationalRepair             Purpose = "informational_repair"
	PurposeInformationalRepairVerification Purpose = "informational_repair_verification"
)

// InvocationRole identifies whether an invocation is allowed to produce a
// patch or must independently judge existing work.
type InvocationRole string

const (
	InvocationRoleFixer    InvocationRole = "fixer"
	InvocationRoleVerifier InvocationRole = "verifier"
)

// PurposeDefinition is the immutable registry entry for a Purpose.
type PurposeDefinition struct {
	Purpose Purpose
	Role    InvocationRole
}

var purposeRegistry = [...]PurposeDefinition{
	{Purpose: PurposeInitialReview, Role: InvocationRoleVerifier},
	{Purpose: PurposeStructuredFindingRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeIntentSensitiveRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeUnstructuredTestRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeUnstructuredCIRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeUnstructuredConflictRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeTestEvidence, Role: InvocationRoleFixer},
	{Purpose: PurposeLintInspection, Role: InvocationRoleFixer},
	{Purpose: PurposeDocumentationAuthoring, Role: InvocationRoleFixer},
	{Purpose: PurposeDocumentationVerification, Role: InvocationRoleVerifier},
	{Purpose: PurposePRComposition, Role: InvocationRoleFixer},
	{Purpose: PurposeIntentSummarization, Role: InvocationRoleVerifier},
	{Purpose: PurposeIntentDisambiguation, Role: InvocationRoleVerifier},
	{Purpose: PurposeBranchCommitSuggestion, Role: InvocationRoleFixer},
	{Purpose: PurposeNormalAggregateVerification, Role: InvocationRoleVerifier},
	{Purpose: PurposeEscalatedAggregateVerification, Role: InvocationRoleVerifier},
	{Purpose: PurposeInformationalRepair, Role: InvocationRoleFixer},
	{Purpose: PurposeInformationalRepairVerification, Role: InvocationRoleVerifier},
}

// PurposeDefinitionFor returns the registered definition for purpose.
func PurposeDefinitionFor(purpose Purpose) (PurposeDefinition, error) {
	for _, definition := range purposeRegistry {
		if definition.Purpose == purpose {
			return definition, nil
		}
	}
	return PurposeDefinition{}, fmt.Errorf("unknown invocation purpose %q", purpose)
}

// AllPurposeDefinitions returns a copy of the closed Purpose registry.
func AllPurposeDefinitions() []PurposeDefinition {
	definitions := make([]PurposeDefinition, len(purposeRegistry))
	copy(definitions, purposeRegistry[:])
	return definitions
}

// InvocationScopeKind distinguishes real pipeline ownership from a standalone
// utility scope. Utility work never fabricates runs, step results, or rounds.
type InvocationScopeKind string

const (
	InvocationScopePipeline InvocationScopeKind = "pipeline"
	InvocationScopeUtility  InvocationScopeKind = "utility"
)

// UtilityScopeKind identifies a registered standalone invocation owner.
type UtilityScopeKind string

const (
	UtilityScopeWizard UtilityScopeKind = "wizard"
)

// InvocationScope contains the durable owner of one invocation.
type InvocationScope struct {
	Kind           InvocationScopeKind
	RunID          string
	StepResultID   string
	StepRoundID    string
	UtilityScopeID string
}

// Validate rejects incomplete or mixed ownership before an agent can launch.
func (scope InvocationScope) Validate() error {
	switch scope.Kind {
	case InvocationScopePipeline:
		if strings.TrimSpace(scope.RunID) == "" || strings.TrimSpace(scope.StepResultID) == "" || strings.TrimSpace(scope.StepRoundID) == "" {
			return fmt.Errorf("pipeline invocation scope requires run, step result, and step round IDs")
		}
		if scope.UtilityScopeID != "" {
			return fmt.Errorf("pipeline invocation scope cannot contain a utility scope ID")
		}
	case InvocationScopeUtility:
		if strings.TrimSpace(scope.UtilityScopeID) == "" {
			return fmt.Errorf("utility invocation scope requires a utility scope ID")
		}
		if scope.RunID != "" || scope.StepResultID != "" || scope.StepRoundID != "" {
			return fmt.Errorf("utility invocation scope cannot contain pipeline IDs")
		}
	default:
		return fmt.Errorf("unknown invocation scope kind %q", scope.Kind)
	}
	return nil
}

// LegacyCandidateKey truthfully identifies the pre-routing candidate used
// during the expand phase. It does not pretend a Profile or model was selected.
const LegacyCandidateKey = "legacy"

// InvocationCandidate records the routed Candidate an attempt launched, so the
// full routing decision (Profile, tier, position, runner, model, effort) is
// durable and secret-free. Its zero value marks a legacy, unrouted attempt.
type InvocationCandidate struct {
	Profile        string
	Tier           int
	CandidateIndex int
	Runner         Runner
	Model          string
	Effort         Effort
}

// IsZero reports whether no routed Candidate was recorded (a legacy attempt).
func (c InvocationCandidate) IsZero() bool {
	return c == InvocationCandidate{}
}

// Validate checks a routed Candidate's fields; it is only called for routed
// attempts whose Candidate is non-zero.
func (c InvocationCandidate) Validate() error {
	if strings.TrimSpace(c.Profile) == "" {
		return fmt.Errorf("invocation candidate requires a profile")
	}
	if c.Tier < 0 || c.CandidateIndex < 0 {
		return fmt.Errorf("invocation candidate tier and index must be non-negative")
	}
	if err := c.Runner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("invocation candidate requires a model")
	}
	if err := c.Effort.Validate(); err != nil {
		return err
	}
	return nil
}

// InvocationAttemptStart is the immutable, secret-free start fact for one
// selected Candidate attempt.
type InvocationAttemptStart struct {
	Purpose      Purpose
	Role         InvocationRole
	Scope        InvocationScope
	CandidateKey string
	// Candidate is the routed Candidate facts. Zero for legacy attempts.
	Candidate InvocationCandidate
}

// InvocationOutcome is the terminal result of a Candidate attempt.
type InvocationOutcome string

const (
	InvocationOutcomeSucceeded   InvocationOutcome = "succeeded"
	InvocationOutcomeFailed      InvocationOutcome = "failed"
	InvocationOutcomeCancelled   InvocationOutcome = "cancelled"
	InvocationOutcomeInterrupted InvocationOutcome = "interrupted"
	// InvocationOutcomeSkipped marks a Candidate a run-wide open provider
	// circuit skipped without launching, recording the skipped-domain decision
	// in immutable history. Its FailureDomain names the open circuit.
	InvocationOutcomeSkipped InvocationOutcome = "skipped"
)

// FailureDomain groups operational failures expected to affect equivalent
// Candidates. Empty means that no operational failure domain was classified.
type FailureDomain string

const (
	FailureDomainOpenAI    FailureDomain = "openai"
	FailureDomainAnthropic FailureDomain = "anthropic"
)

// InvocationAttemptTerminal is the immutable terminal fact for an attempt.
type InvocationAttemptTerminal struct {
	Outcome             InvocationOutcome
	FailureDomain       FailureDomain
	DurationMS          int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Validate checks a terminal fact before it is appended.
func (terminal InvocationAttemptTerminal) Validate() error {
	switch terminal.Outcome {
	case InvocationOutcomeSucceeded, InvocationOutcomeFailed, InvocationOutcomeCancelled, InvocationOutcomeInterrupted, InvocationOutcomeSkipped:
	default:
		return fmt.Errorf("unknown invocation outcome %q", terminal.Outcome)
	}
	if terminal.FailureDomain != "" && terminal.FailureDomain != FailureDomainOpenAI && terminal.FailureDomain != FailureDomainAnthropic {
		return fmt.Errorf("unknown failure domain %q", terminal.FailureDomain)
	}
	if terminal.DurationMS < 0 || terminal.InputTokens < 0 || terminal.OutputTokens < 0 || terminal.CacheReadTokens < 0 || terminal.CacheCreationTokens < 0 {
		return fmt.Errorf("invocation terminal counters cannot be negative")
	}
	return nil
}

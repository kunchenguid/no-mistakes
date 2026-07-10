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

// InvocationAttemptStart is the immutable, secret-free start fact for one
// selected Candidate attempt.
type InvocationAttemptStart struct {
	Purpose      Purpose
	Role         InvocationRole
	Scope        InvocationScope
	CandidateKey string
}

// InvocationOutcome is the terminal result of a Candidate attempt.
type InvocationOutcome string

const (
	InvocationOutcomeSucceeded   InvocationOutcome = "succeeded"
	InvocationOutcomeFailed      InvocationOutcome = "failed"
	InvocationOutcomeCancelled   InvocationOutcome = "cancelled"
	InvocationOutcomeInterrupted InvocationOutcome = "interrupted"
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
	case InvocationOutcomeSucceeded, InvocationOutcomeFailed, InvocationOutcomeCancelled, InvocationOutcomeInterrupted:
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

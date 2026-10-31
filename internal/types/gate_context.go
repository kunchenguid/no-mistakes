package types

// GateRunContext identifies one authenticated custom-gate execution.
// It conveys provenance only, never permission to mutate the enclosing run.
type GateRunContext struct {
	RunID    string   `json:"run_id"`
	RepoID   string   `json:"repo_id"`
	Worktree string   `json:"worktree"`
	Phase    StepName `json:"phase"`
}

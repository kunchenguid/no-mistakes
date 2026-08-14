package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	// ScopeDecisionGateName is the stable operator-visible refusal raised before
	// an explicitly out-of-scope repair can be dispatched to an agent.
	ScopeDecisionGateName = types.ScopeDecisionGateName
	// ProofIdentityMovedName is the stable refusal raised when a proof result was
	// produced against one checkout identity but the checkout moved before the
	// pipeline could consume it.
	ProofIdentityMovedName = "PIPELINE_PROOF_IDENTITY_MOVED"
)

// DeclaredScope is the immutable set of repository-relative paths present in
// the submitted change. It is computed once from base..submitted HEAD and is
// never widened by a fix round.
type DeclaredScope struct {
	state *declaredScopeState
}

type declaredScopeState struct {
	mu    sync.RWMutex
	paths []string
	set   map[string]struct{}
}

func NewDeclaredScope(paths []string) DeclaredScope {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if normalized, ok := normalizeScopePath(path); ok {
			set[normalized] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(set))
	for path := range set {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return DeclaredScope{state: &declaredScopeState{paths: ordered, set: set}}
}

func normalizeScopePath(path string) (string, bool) {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "" || path == "." || path == ".." || filepath.IsAbs(path) || strings.HasPrefix(path, "../") {
		return "", false
	}
	return path, true
}

func (s DeclaredScope) Contains(path string) bool {
	normalized, ok := normalizeScopePath(path)
	if !ok {
		return false
	}
	if s.state == nil {
		return false
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	_, ok = s.state.set[normalized]
	return ok
}

func (s DeclaredScope) Paths() []string {
	if s.state == nil {
		return nil
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	return append([]string(nil), s.state.paths...)
}

func (s DeclaredScope) initialized() bool { return s.state != nil }

func (s DeclaredScope) authorize(paths []string) []string {
	if s.state == nil {
		return nil
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	var added []string
	for _, path := range paths {
		normalized, ok := normalizeScopePath(path)
		if !ok {
			continue
		}
		if _, exists := s.state.set[normalized]; exists {
			continue
		}
		s.state.set[normalized] = struct{}{}
		added = append(added, normalized)
	}
	if len(added) > 0 {
		s.state.paths = s.state.paths[:0]
		for path := range s.state.set {
			s.state.paths = append(s.state.paths, path)
		}
		sort.Strings(s.state.paths)
	}
	return added
}

// declaredScopeForRun derives the immutable path set with the same NUL-delimited
// reader the fence inspects the checkout with. A plain `--name-only` C-quotes
// any path git considers unusual (non-ASCII bytes under the default
// core.quotePath, embedded quotes, control characters), which would enter the
// scope set as `"pkg/caf\303\251.go"` while the checkout side yields the raw
// bytes - the membership test would then miss and refuse every fix round in
// that repository. `-z` emits raw paths on both sides. `--no-renames` keeps a
// renamed file's old path declared too, so repairing either end stays in scope.
func declaredScopeForRun(ctx context.Context, workDir string, runHead, baseSHA string) (DeclaredScope, error) {
	if strings.TrimSpace(baseSHA) == "" || git.IsZeroSHA(baseSHA) {
		baseSHA = git.EmptyTreeSHA
	}
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", baseSHA+".."+runHead)
	if err != nil {
		return DeclaredScope{}, fmt.Errorf("derive declared scope from %s..%s: %w", baseSHA, runHead, err)
	}
	return NewDeclaredScope(splitNulPaths(out)), nil
}

func splitNulPaths(out string) []string {
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// ScopeBoundaryError is the refusal AssertDeclaredScope returns when an agent
// left a working-tree change outside the immutable submitted path set. It is a
// decision, not a failure: the executor turns it into the same named
// ask-user gate the findings path raises, so the operator can authorize the
// exact paths (or abort) instead of losing the run and the work in the
// checkout. The edits themselves are never touched - the pipeline only refuses
// to stage, commit, or push them.
type ScopeBoundaryError struct {
	Effect string
	Paths  []string
}

func (e *ScopeBoundaryError) Error() string {
	return fmt.Sprintf("%s: refusing %s because agent changes exceed declared ticket scope: %s", ScopeDecisionGateName, e.Effect, strings.Join(e.Paths, ", "))
}

// AssertDeclaredScope refuses staging, committing, or pushing when an agent
// has left a working-tree change outside the immutable submitted path set.
// An inspection failure is a hard error (fail closed); an actually observed
// out-of-scope path is a *ScopeBoundaryError the executor parks on.
func (sctx *StepContext) AssertDeclaredScope(effect string) error {
	if sctx == nil {
		return fmt.Errorf("%s: missing step context before %s", ScopeDecisionGateName, effect)
	}
	if !sctx.DeclaredScope.initialized() {
		return nil
	}
	commands := [][]string{
		{"diff", "--name-only", "-z"},
		{"diff", "--cached", "--name-only", "-z"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	}
	outside := make(map[string]struct{})
	for _, args := range commands {
		out, err := git.Run(sctx.Ctx, sctx.WorkDir, args...)
		if err != nil {
			return fmt.Errorf("%s: inspect checkout before %s: %w", ScopeDecisionGateName, effect, err)
		}
		for _, path := range splitNulPaths(out) {
			if !sctx.DeclaredScope.Contains(path) {
				outside[strings.TrimSpace(path)] = struct{}{}
			}
		}
	}
	if len(outside) == 0 {
		return nil
	}
	paths := make([]string, 0, len(outside))
	for path := range outside {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return &ScopeBoundaryError{Effect: effect, Paths: paths}
}

// scopeBoundaryGateOutcome converts a refused effect into the parked decision
// gate. A fixer that must touch a surface the submitted delta never declared -
// the new regression test its own guidance demands, say - would otherwise kill
// the run with its work stranded uncommitted, and no fixer role can raise the
// gate itself (they are all pinned to the commit-summary schema). Returning the
// gate here is what makes the boundary an operator decision from every effect
// site: the fix rounds, the CI repair commit, and the push commit.
func scopeBoundaryGateOutcome(err error, stepName types.StepName) (*StepOutcome, bool) {
	var boundary *ScopeBoundaryError
	if !errors.As(err, &boundary) {
		return nil, false
	}
	findings := types.Findings{
		Summary:       fmt.Sprintf("%s: %s requires a declared-scope decision", ScopeDecisionGateName, boundary.Effect),
		RiskLevel:     "high",
		RiskRationale: "the pipeline changed paths the submitted change never declared",
	}
	for _, path := range boundary.Paths {
		findings.Items = append(findings.Items, types.Finding{
			Severity:      "error",
			File:          path,
			Action:        types.ActionAskUser,
			ScopeDecision: true,
			Description: fmt.Sprintf("%s: %s changed %s, which is outside the declared ticket scope; the edit is preserved in the checkout. Selecting this finding for a fix authorizes exactly this path for the next round.",
				ScopeDecisionGateName, stepName, path),
		})
	}
	return &StepOutcome{NeedsApproval: true, Findings: mustMarshalFindings(findings)}, true
}

func mustMarshalFindings(findings types.Findings) string {
	raw, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return ""
	}
	return raw
}

func (s DeclaredScope) promptBoundary() string {
	declaredPaths := s.Paths()
	paths := "(none)"
	if len(declaredPaths) > 0 {
		paths = "- " + strings.Join(declaredPaths, "\n- ")
	}
	return fmt.Sprintf(`DECLARED TICKET SCOPE (enforced):
You may inspect the repository, but you may mutate only the exact repository-relative paths listed below:
%s
If a correct repair requires any other path or surface, do not edit it and do not run a repair or runtime command for it. Report %s with the proposed path and reason so the pipeline can stop for an explicit decision. A failed receipt or missing field never authorizes an alternate runtime command.

`, paths, ScopeDecisionGateName)
}

// scopeAutoFixDecisionGate separates in-scope automatic repairs from explicit
// out-of-scope proposals. The latter become ask-user findings before the
// executor dispatches a fixer, which is the pre-effect gate AS-440 lacked.
//
// It is also the single ingress that owns the ScopeDecision flag: every
// incoming finding is stripped first, so a step's agent cannot hand back a
// self-minted gate whose selected path would later widen the very scope the
// fence exists to hold.
func scopeAutoFixDecisionGate(raw string, scope DeclaredScope) (fixableRaw, gatedRaw string) {
	if raw == "" {
		return "", ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", raw
	}
	for i := range findings.Items {
		findings.Items[i].ScopeDecision = false
	}
	if scope.initialized() {
		for i := range findings.Items {
			item := &findings.Items[i]
			if item.Action != types.ActionAutoFix || (strings.TrimSpace(item.File) != "" && scope.Contains(item.File)) {
				continue
			}
			item.Action = types.ActionAskUser
			item.ScopeDecision = true
			if strings.TrimSpace(item.File) == "" {
				item.Description = fmt.Sprintf("%s: proposed repair has no file/surface identity, so it cannot be proven inside the declared ticket scope; %s", ScopeDecisionGateName, item.Description)
			} else {
				item.Description = fmt.Sprintf("%s: proposed repair to %s is outside the declared ticket scope; %s", ScopeDecisionGateName, item.File, item.Description)
			}
		}
	}
	gated, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return "", raw
	}
	return autoFixableFindingsJSON(gated), gated
}

// authorizeSelectedScopeDecisionPaths widens scope only after an explicit
// user fix response selects the decision finding. Automatic resolution never
// calls this path. Authorization keys on the pipeline-owned ScopeDecision
// flag, never on description prose an agent controls; file-less gates cannot
// widen an unknowable surface.
func authorizeSelectedScopeDecisionPaths(scope DeclaredScope, raw string) []string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	var paths []string
	for _, item := range findings.Items {
		if !item.ScopeDecision || strings.TrimSpace(item.File) == "" {
			continue
		}
		paths = append(paths, item.File)
	}
	return scope.authorize(paths)
}

type checkoutIdentity struct {
	head string
	tree string
}

func readCheckoutIdentity(ctx context.Context, workDir string) (checkoutIdentity, error) {
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return checkoutIdentity{}, err
	}
	tree, err := git.Run(ctx, workDir, "rev-parse", head+"^{tree}")
	if err != nil {
		return checkoutIdentity{}, err
	}
	return checkoutIdentity{head: strings.TrimSpace(head), tree: strings.TrimSpace(tree)}, nil
}

// scopeAndProofFenceAgent gives every invocation its immutable mutation scope
// and refuses to return a result after HEAD or the committed tree changes.
// Deliberate checkout-moving invocations (rebase/CI conflict repair) opt out of
// the equality check explicitly; they still receive the scope contract.
type scopeAndProofFenceAgent struct {
	inner   agent.Agent
	workDir string
	scope   DeclaredScope
}

func (a *scopeAndProofFenceAgent) Name() string { return a.inner.Name() }
func (a *scopeAndProofFenceAgent) Close() error { return a.inner.Close() }

func (a *scopeAndProofFenceAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	before, err := readCheckoutIdentity(ctx, a.workDir)
	if err != nil {
		return nil, fmt.Errorf("bind agent invocation to checkout identity: %w", err)
	}
	opts.Prompt = a.scope.promptBoundary() + opts.Prompt
	opts.InvocationHeadSHA = before.head
	opts.InvocationTreeSHA = before.tree
	result, err := a.inner.Run(ctx, opts)
	if err != nil {
		return result, err
	}
	if opts.AllowCheckoutMovement {
		return result, nil
	}
	after, identityErr := readCheckoutIdentity(ctx, a.workDir)
	if identityErr != nil {
		return nil, fmt.Errorf("%s: resolve checkout identity after agent invocation: %w", ProofIdentityMovedName, identityErr)
	}
	if before != after {
		return nil, fmt.Errorf("%s: proof bound to HEAD %s TREE %s, checkout is now HEAD %s TREE %s; refusing mixed-head result", ProofIdentityMovedName, before.head, before.tree, after.head, after.tree)
	}
	if result != nil {
		result.InvocationHeadSHA = before.head
		result.InvocationTreeSHA = before.tree
	}
	return result, nil
}

func (a *scopeAndProofFenceAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *scopeAndProofFenceAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *scopeAndProofFenceAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *scopeAndProofFenceAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

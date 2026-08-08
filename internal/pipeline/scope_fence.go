package pipeline

import (
	"context"
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

func declaredScopeForRun(ctx context.Context, workDir string, runHead, baseSHA string) (DeclaredScope, error) {
	if strings.TrimSpace(baseSHA) == "" || git.IsZeroSHA(baseSHA) {
		baseSHA = git.EmptyTreeSHA
	}
	paths, err := git.DiffNameOnly(ctx, workDir, baseSHA, runHead)
	if err != nil {
		return DeclaredScope{}, fmt.Errorf("derive declared scope from %s..%s: %w", baseSHA, runHead, err)
	}
	return NewDeclaredScope(paths), nil
}

// AssertDeclaredScope refuses staging, committing, or pushing when an agent
// has left a working-tree change outside the immutable submitted path set.
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
		for _, path := range strings.Split(out, "\x00") {
			path = strings.TrimSpace(path)
			if path != "" && !sctx.DeclaredScope.Contains(path) {
				outside[path] = struct{}{}
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
	return fmt.Errorf("%s: refusing %s because agent changes exceed declared ticket scope: %s", ScopeDecisionGateName, effect, strings.Join(paths, ", "))
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
func scopeAutoFixDecisionGate(raw string, scope DeclaredScope) (fixableRaw, gatedRaw string) {
	if raw == "" {
		return "", ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", raw
	}
	if !scope.initialized() {
		fixable := types.AutoFixableFindings(findings)
		if len(fixable.Items) == 0 {
			return "", raw
		}
		encoded, marshalErr := types.MarshalFindingsJSON(fixable)
		if marshalErr != nil {
			return "", raw
		}
		return encoded, raw
	}
	for i := range findings.Items {
		item := &findings.Items[i]
		if item.Action != types.ActionAutoFix || (strings.TrimSpace(item.File) != "" && scope.Contains(item.File)) {
			continue
		}
		item.Action = types.ActionAskUser
		if strings.TrimSpace(item.File) == "" {
			item.Description = fmt.Sprintf("%s: proposed repair has no file/surface identity, so it cannot be proven inside the declared ticket scope; %s", ScopeDecisionGateName, item.Description)
		} else {
			item.Description = fmt.Sprintf("%s: proposed repair to %s is outside the declared ticket scope; %s", ScopeDecisionGateName, item.File, item.Description)
		}
	}
	gated, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return "", raw
	}
	fixable := types.AutoFixableFindings(findings)
	if len(fixable.Items) == 0 {
		return "", gated
	}
	encoded, err := types.MarshalFindingsJSON(fixable)
	if err != nil {
		return "", gated
	}
	return encoded, gated
}

// authorizeSelectedScopeDecisionPaths widens scope only after an explicit
// user fix response selects the named decision finding. Automatic resolution
// never calls this path. File-less gates cannot widen an unknowable surface.
func authorizeSelectedScopeDecisionPaths(scope DeclaredScope, raw string) []string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	var paths []string
	for _, item := range findings.Items {
		if strings.TrimSpace(item.File) == "" || !strings.Contains(item.Description, ScopeDecisionGateName) {
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

package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ProfileName identifies a normalized capability tier: a group of provider
// Candidates that share a purpose-agnostic model/effort intent.
type ProfileName string

const (
	ProfileFixFast         ProfileName = "fix_fast"
	ProfileProseFast       ProfileName = "prose_fast"
	ProfileFixBalanced     ProfileName = "fix_balanced"
	ProfileToolsBalanced   ProfileName = "tools_balanced"
	ProfileReviewStrong    ProfileName = "review_strong"
	ProfileAuthorityStrong ProfileName = "authority_strong"
)

// Candidate is one runner/model/effort attempt within a Profile. Candidates
// are ordered by provider preference (OpenAI family first, Anthropic backup).
type Candidate struct {
	Runner types.Runner `yaml:"runner"`
	Model  string       `yaml:"model"`
	Effort types.Effort `yaml:"effort"`
}

// Profile is an ordered set of provider Candidates at one capability tier.
type Profile struct {
	Name       ProfileName `yaml:"-"`
	Candidates []Candidate `yaml:"candidates"`
}

// RunnerSpec declares a runner's executable and provider failure domain.
// Runners own execution mechanics; repositories can never define them.
type RunnerSpec struct {
	Executable    string              `yaml:"executable"`
	FailureDomain types.FailureDomain `yaml:"failure_domain"`
}

// Route is a finite, ordered escalation cascade of Profile names for one
// Purpose. A single-tier Purpose has a one-element Route.
type Route []ProfileName

// RoutingConfig is the global model-selection contract. Runners own execution
// mechanics, Profiles group provider Candidates, and Routes map every
// registered Purpose to an ordered Profile cascade.
type RoutingConfig struct {
	Runners  map[types.Runner]RunnerSpec `yaml:"runners"`
	Profiles map[ProfileName]Profile     `yaml:"profiles"`
	Routes   map[types.Purpose]Route     `yaml:"routes"`
}

// IsZero reports whether the routing config carries no runners, profiles, or
// routes — i.e. it was never populated and callers should fall back to
// DefaultRoutingConfig.
func (rc RoutingConfig) IsZero() bool {
	return len(rc.Runners) == 0 && len(rc.Profiles) == 0 && len(rc.Routes) == 0
}

func defaultProfile(name ProfileName, codexModel, claudeModel string, effort types.Effort) Profile {
	return Profile{
		Name: name,
		Candidates: []Candidate{
			{Runner: types.RunnerCodex, Model: codexModel, Effort: effort},
			{Runner: types.RunnerClaude, Model: claudeModel, Effort: effort},
		},
	}
}

// DefaultRoutingConfig returns the built-in routing contract: two runners, the
// six default Profiles, and a Route for every registered Purpose. Pro model
// selectors are valid but intentionally absent from the defaults.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		Runners: map[types.Runner]RunnerSpec{
			types.RunnerCodex:  {Executable: "codex", FailureDomain: types.FailureDomainOpenAI},
			types.RunnerClaude: {Executable: "claude", FailureDomain: types.FailureDomainAnthropic},
		},
		Profiles: map[ProfileName]Profile{
			ProfileFixFast:         defaultProfile(ProfileFixFast, "gpt-5.6-luna", "claude-sonnet-5", types.EffortMedium),
			ProfileProseFast:       defaultProfile(ProfileProseFast, "gpt-5.6-luna", "claude-sonnet-5", types.EffortLow),
			ProfileFixBalanced:     defaultProfile(ProfileFixBalanced, "gpt-5.6-terra", "claude-opus-4-8", types.EffortMedium),
			ProfileToolsBalanced:   defaultProfile(ProfileToolsBalanced, "gpt-5.6-terra", "claude-opus-4-8", types.EffortHigh),
			ProfileReviewStrong:    defaultProfile(ProfileReviewStrong, "gpt-5.6-sol", "claude-fable-5", types.EffortHigh),
			ProfileAuthorityStrong: defaultProfile(ProfileAuthorityStrong, "gpt-5.6-sol", "claude-fable-5", types.EffortXHigh),
		},
		Routes: defaultRoutes(),
	}
}

// defaultRoutes maps every registered Purpose to its default Profile cascade.
func defaultRoutes() map[types.Purpose]Route {
	return map[types.Purpose]Route{
		types.PurposeInitialReview:                   {ProfileReviewStrong},
		types.PurposeStructuredFindingRepair:         {ProfileFixFast, ProfileFixBalanced, ProfileAuthorityStrong},
		types.PurposeIntentSensitiveRepair:           {ProfileFixBalanced, ProfileAuthorityStrong},
		types.PurposeUnstructuredTestRepair:          {ProfileFixBalanced, ProfileAuthorityStrong},
		types.PurposeUnstructuredCIRepair:            {ProfileFixBalanced, ProfileAuthorityStrong},
		types.PurposeUnstructuredConflictRepair:      {ProfileFixBalanced, ProfileAuthorityStrong},
		types.PurposeTestEvidence:                    {ProfileToolsBalanced},
		types.PurposeLintInspection:                  {ProfileToolsBalanced},
		types.PurposeDocumentationAuthoring:          {ProfileProseFast},
		types.PurposeDocumentationVerification:       {ProfileToolsBalanced},
		types.PurposePRComposition:                   {ProfileProseFast},
		types.PurposeIntentSummarization:             {ProfileProseFast},
		types.PurposeIntentDisambiguation:            {ProfileToolsBalanced},
		types.PurposeBranchCommitSuggestion:          {ProfileProseFast},
		types.PurposeNormalAggregateVerification:     {ProfileReviewStrong},
		types.PurposeEscalatedAggregateVerification:  {ProfileAuthorityStrong},
		types.PurposeInformationalRepair:             {ProfileFixFast, ProfileToolsBalanced},
		types.PurposeInformationalRepairVerification: {ProfileToolsBalanced},
	}
}

// Validate enforces the strict, additive routing contract. It rejects
// unsupported runners, empty Candidate lists, invalid efforts or models,
// unknown Purposes or Profiles, and incomplete or non-finite Routes.
func (rc RoutingConfig) Validate() error {
	if len(rc.Runners) == 0 {
		return fmt.Errorf("routing: no runners configured")
	}
	for _, name := range sortedRunnerNames(rc.Runners) {
		spec := rc.Runners[name]
		if err := name.Validate(); err != nil {
			return fmt.Errorf("routing runner: %w", err)
		}
		if strings.TrimSpace(spec.Executable) == "" {
			return fmt.Errorf("routing runner %q: executable required", name)
		}
		expected, err := name.FailureDomain()
		if err != nil {
			return fmt.Errorf("routing runner: %w", err)
		}
		if spec.FailureDomain != expected {
			return fmt.Errorf("routing runner %q: failure domain %q must be %q", name, spec.FailureDomain, expected)
		}
	}

	if len(rc.Profiles) == 0 {
		return fmt.Errorf("routing: no profiles configured")
	}
	for _, name := range sortedProfileNames(rc.Profiles) {
		p := rc.Profiles[name]
		if len(p.Candidates) == 0 {
			return fmt.Errorf("routing profile %q: empty candidates", name)
		}
		for i, c := range p.Candidates {
			if err := c.Runner.Validate(); err != nil {
				return fmt.Errorf("routing profile %q candidate %d: %w", name, i, err)
			}
			if _, ok := rc.Runners[c.Runner]; !ok {
				return fmt.Errorf("routing profile %q candidate %d: runner %q not declared", name, i, c.Runner)
			}
			if strings.TrimSpace(c.Model) == "" {
				return fmt.Errorf("routing profile %q candidate %d: model required", name, i)
			}
			if err := c.Effort.Validate(); err != nil {
				return fmt.Errorf("routing profile %q candidate %d: %w", name, i, err)
			}
		}
	}

	for _, purpose := range sortedRoutePurposes(rc.Routes) {
		if _, err := types.PurposeDefinitionFor(purpose); err != nil {
			return fmt.Errorf("routing: route for %w", err)
		}
	}
	for _, def := range types.AllPurposeDefinitions() {
		route, ok := rc.Routes[def.Purpose]
		if !ok {
			return fmt.Errorf("routing: purpose %q has no route", def.Purpose)
		}
		if len(route) == 0 {
			return fmt.Errorf("routing: purpose %q route is empty", def.Purpose)
		}
		seen := make(map[ProfileName]bool, len(route))
		for _, pn := range route {
			if _, ok := rc.Profiles[pn]; !ok {
				return fmt.Errorf("routing: purpose %q references unknown profile %q", def.Purpose, pn)
			}
			if seen[pn] {
				return fmt.Errorf("routing: purpose %q route repeats profile %q (non-finite cascade)", def.Purpose, pn)
			}
			seen[pn] = true
		}
	}
	return nil
}

// ValidateRunnable verifies that every routed Profile has at least one
// Candidate whose normalized runner executable can be launched. Routing may
// retain unavailable backup Candidates, but a gate must fail before starting
// if any reachable tier has no runnable provider at all.
func (rc RoutingConfig) ValidateRunnable(lookPath func(string) (string, error)) error {
	if err := rc.Validate(); err != nil {
		return err
	}
	if lookPath == nil {
		return fmt.Errorf("routing: executable resolver is nil")
	}
	checked := make(map[ProfileName]bool, len(rc.Profiles))
	for _, purpose := range sortedRoutePurposes(rc.Routes) {
		for _, profileName := range rc.Routes[purpose] {
			if checked[profileName] {
				continue
			}
			checked[profileName] = true
			profile := rc.Profiles[profileName]
			probed := make([]string, 0, len(profile.Candidates))
			runnable := false
			for _, candidate := range profile.Candidates {
				executable := rc.Runners[candidate.Runner].Executable
				probed = append(probed, executable)
				if _, err := lookPath(executable); err == nil {
					runnable = true
					break
				}
			}
			if !runnable {
				return fmt.Errorf("routing profile %q has no runnable candidate (looked for: %s); the gate cannot validate without a configured runner", profileName, strings.Join(probed, ", "))
			}
		}
	}
	return nil
}

// ValidateRunnable checks the resolved routing contract and its runner
// executables after trusted repository Route overrides have been merged.
func (c *Config) ValidateRunnable(lookPath func(string) (string, error)) error {
	if c == nil {
		return fmt.Errorf("routing config is nil")
	}
	return c.Routing.ValidateRunnable(lookPath)
}

func sortedRunnerNames(m map[types.Runner]RunnerSpec) []types.Runner {
	out := make([]types.Runner, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedProfileNames(m map[ProfileName]Profile) []ProfileName {
	out := make([]ProfileName, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedRoutePurposes(m map[types.Purpose]Route) []types.Purpose {
	out := make([]types.Purpose, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResolveRoute returns the ordered Profile cascade for a Purpose.
func (rc RoutingConfig) ResolveRoute(purpose types.Purpose) ([]Profile, error) {
	route, ok := rc.Routes[purpose]
	if !ok {
		return nil, fmt.Errorf("no route for purpose %q", purpose)
	}
	profiles := make([]Profile, 0, len(route))
	for _, pn := range route {
		p, ok := rc.Profiles[pn]
		if !ok {
			return nil, fmt.Errorf("purpose %q references unknown profile %q", purpose, pn)
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// clone deep-copies the routing config so repository overrides never mutate a
// shared default.
func (rc RoutingConfig) clone() RoutingConfig {
	out := RoutingConfig{
		Runners:  make(map[types.Runner]RunnerSpec, len(rc.Runners)),
		Profiles: make(map[ProfileName]Profile, len(rc.Profiles)),
		Routes:   make(map[types.Purpose]Route, len(rc.Routes)),
	}
	for name, spec := range rc.Runners {
		out.Runners[name] = spec
	}
	for name, p := range rc.Profiles {
		p.Candidates = append([]Candidate(nil), p.Candidates...)
		out.Profiles[name] = p
	}
	for purpose, route := range rc.Routes {
		out.Routes[purpose] = append(Route(nil), route...)
	}
	return out
}

// normalizeProfileNames fills each Profile's Name from its map key after a YAML
// decode, where the name is only present as the key.
func (rc RoutingConfig) normalizeProfileNames() {
	for name, p := range rc.Profiles {
		p.Name = name
		rc.Profiles[name] = p
	}
}

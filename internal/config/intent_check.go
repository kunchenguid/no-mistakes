package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	IntentCheckOff    = "off"
	IntentCheckWarn   = "warn"
	IntentCheckReject = "reject"

	maxIntentCheckRules      = 32
	maxIntentCheckRuleLength = 256
	maxIntentCheckBytes      = 8192
)

// IntentCheckRaw is the YAML policy for deterministic intent screening.
type IntentCheckRaw struct {
	Mode     string   `yaml:"mode" json:"mode,omitempty"`
	Terms    []string `yaml:"terms" json:"terms,omitempty"`
	Patterns []string `yaml:"patterns" json:"patterns,omitempty"`

	compiledPatterns []*regexp.Regexp
}

// IntentCheck is the resolved, additive policy used by the daemon.
type IntentCheck = IntentCheckRaw

// validateIntentCheck normalizes and compiles the policy while loading config,
// so malformed trusted rules fail before a run can start.
func validateIntentCheck(check *IntentCheckRaw) error {
	return validateIntentCheckWithLimits(check, maxIntentCheckRules, maxIntentCheckBytes)
}

func validateIntentCheckWithLimits(check *IntentCheckRaw, maxRules, maxBytes int) error {
	check.Mode = strings.ToLower(strings.TrimSpace(check.Mode))
	if check.Mode == "" {
		check.Mode = IntentCheckOff
	}
	switch check.Mode {
	case IntentCheckOff, IntentCheckWarn, IntentCheckReject:
	default:
		return fmt.Errorf("intent.check.mode must be off, warn, or reject")
	}
	if len(check.Terms)+len(check.Patterns) > maxRules {
		return fmt.Errorf("intent.check may contain at most %d rules", maxRules)
	}
	check.compiledPatterns = make([]*regexp.Regexp, 0, len(check.Patterns))
	bytes := 0
	for i, term := range check.Terms {
		term = strings.TrimSpace(term)
		if term == "" || len(term) > maxIntentCheckRuleLength {
			return fmt.Errorf("intent.check.terms[%d] must be non-empty and at most %d bytes", i, maxIntentCheckRuleLength)
		}
		bytes += len(term)
		check.Terms[i] = term
	}
	for i, pattern := range check.Patterns {
		if strings.TrimSpace(pattern) == "" || len(pattern) > maxIntentCheckRuleLength {
			return fmt.Errorf("intent.check.patterns[%d] must be non-empty and at most %d bytes", i, maxIntentCheckRuleLength)
		}
		bytes += len(pattern)
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("intent.check.patterns[%d] is not a valid RE2 expression", i)
		}
		check.Patterns[i] = pattern
		check.compiledPatterns = append(check.compiledPatterns, compiled)
	}
	if bytes > maxBytes {
		return fmt.Errorf("intent.check rules may contain at most %d bytes", maxBytes)
	}
	return nil
}

// MergeIntentChecks adds policy rules and keeps the strongest configured mode.
func MergeIntentChecks(checks ...IntentCheckRaw) IntentCheck {
	merged := IntentCheckRaw{Mode: IntentCheckOff}
	for _, check := range checks {
		merged.Terms = append(merged.Terms, check.Terms...)
		merged.Patterns = append(merged.Patterns, check.Patterns...)
		if intentCheckModeRank(check.Mode) > intentCheckModeRank(merged.Mode) {
			merged.Mode = check.Mode
		}
	}
	_ = validateIntentCheckWithLimits(&merged, 2*maxIntentCheckRules, 2*maxIntentCheckBytes) // Each input is validated by config loading.
	return merged
}

func intentCheckModeRank(mode string) int {
	switch mode {
	case IntentCheckWarn:
		return 1
	case IntentCheckReject:
		return 2
	default:
		return 0
	}
}

// Match returns rule identifiers only; it never returns the matched text.
func (check IntentCheckRaw) Match(text string) []string {
	if check.Mode == IntentCheckOff {
		return nil
	}
	folded := strings.ToLower(text)
	var matches []string
	for i, term := range check.Terms {
		if strings.Contains(folded, strings.ToLower(term)) {
			matches = append(matches, fmt.Sprintf("terms[%d]", i))
		}
	}
	for i, pattern := range check.Patterns {
		compiled := (*regexp.Regexp)(nil)
		if i < len(check.compiledPatterns) {
			compiled = check.compiledPatterns[i]
		} else {
			compiled, _ = regexp.Compile(pattern)
		}
		if compiled != nil && compiled.MatchString(text) {
			matches = append(matches, fmt.Sprintf("patterns[%d]", i))
		}
	}
	return matches
}

// ParseIntentCheckYAML reads only the intent policy so unrelated config fields
// cannot prevent admission from enforcing it.
func ParseIntentCheckYAML(data []byte) (IntentCheck, error) {
	var repo struct {
		Intent struct {
			Check IntentCheckRaw `yaml:"check"`
		} `yaml:"intent"`
	}
	if err := yaml.Unmarshal(data, &repo); err != nil {
		return IntentCheck{}, fmt.Errorf("parse intent.check: %w", err)
	}
	if err := validateIntentCheck(&repo.Intent.Check); err != nil {
		return IntentCheck{}, err
	}
	return repo.Intent.Check, nil
}

// ParseIntentCheckJSON validates a policy snapshot loaded from run storage.
func ParseIntentCheckJSON(data []byte) (IntentCheck, error) {
	var check IntentCheckRaw
	if err := json.Unmarshal(data, &check); err != nil {
		return IntentCheck{}, fmt.Errorf("parse pinned intent check: %w", err)
	}
	if err := validateIntentCheckWithLimits(&check, 2*maxIntentCheckRules, 2*maxIntentCheckBytes); err != nil {
		return IntentCheck{}, fmt.Errorf("parse pinned intent check: %w", err)
	}
	return check, nil
}

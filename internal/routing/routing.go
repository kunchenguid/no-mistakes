// Package routing owns the model policy used by no-mistakes. It deliberately
// has no repository or project-specific dependencies: project metadata is an
// input to a bounded fingerprint, never executable routing authority.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"strconv"
	"strings"
)

const PolicyVersion = "nm-routing-v1"

const (
	ModelLuna   = "gpt-5.6-luna"
	ModelTerra  = "gpt-5.6-terra"
	ModelSol    = "gpt-5.6-sol"
	EffortXHigh = "xhigh"
	EffortHigh  = "high"
)

type Risk string

const (
	RiskUnknown Risk = "unknown"
	RiskLow     Risk = "low"
	RiskMedium  Risk = "medium"
	RiskHigh    Risk = "high"
)

type Input struct {
	Harness                 string
	Purpose                 string
	Repository              string
	Risk                    Risk
	ReviewConfirmation      bool
	SourceConfiguration     string
	ConfigurationGeneration string
}

type Decision struct {
	RequestedHarness        string
	EffectiveHarness        string
	RequestedModel          string
	EffectiveModel          string
	RequestedEffort         string
	EffectiveEffort         string
	PolicyVersion           string
	Phase                   string
	Risk                    Risk
	Reason                  string
	SourceConfiguration     string
	ConfigurationGeneration string
	Repository              string
}

func Decide(in Input) Decision {
	harness := strings.TrimSpace(in.Harness)
	if harness == "" {
		harness = "unknown"
	}
	phase := phaseForPurpose(in.Purpose)
	if phase == "review" && in.ReviewConfirmation {
		phase = "review-confirmation"
	}
	risk := in.Risk
	if risk == "" {
		risk = RiskUnknown
	}
	d := Decision{
		RequestedHarness:        harness,
		EffectiveHarness:        harness,
		RequestedModel:          ModelLuna,
		EffectiveModel:          ModelLuna,
		RequestedEffort:         EffortXHigh,
		EffectiveEffort:         EffortXHigh,
		PolicyVersion:           PolicyVersion,
		Phase:                   phase,
		Risk:                    risk,
		SourceConfiguration:     bounded(in.SourceConfiguration, 512),
		ConfigurationGeneration: bounded(in.ConfigurationGeneration, 128),
		Repository:              CanonicalRepository(in.Repository),
		Reason:                  "default initial/default work",
	}
	// The first review is the bootstrap classifier. Any caller-supplied risk
	// is deliberately ignored until that review has produced its own result.
	if phase == "review" && !in.ReviewConfirmation {
		return d
	}
	if (risk == RiskMedium || risk == RiskHigh) && phase != "review" && phase != "review-confirmation" {
		d.EffectiveModel = ModelTerra
		d.EffectiveEffort = EffortHigh
		d.Reason = "medium-high risk policy"
	}
	// Bootstrap is explicit: the first review has no risk classification yet,
	// so it always remains Luna. Sol is legal only for a later review turn
	// after the earlier review classified the change as high risk.
	if phase == "review-confirmation" && in.ReviewConfirmation && risk == RiskHigh {
		d.EffectiveModel = ModelSol
		d.EffectiveEffort = EffortHigh
		d.Reason = "high-risk review confirmation"
	}
	return d
}

func phaseForPurpose(purpose string) string {
	switch strings.ToLower(strings.TrimSpace(purpose)) {
	case "review-confirmation", "review-confirm":
		return "review-confirmation"
	case "review":
		return "review"
	case "review-fix":
		return "review-fix"
	case "test-evidence", "test":
		return "test"
	case "document", "lint", "housekeeping":
		return "housekeeping"
	default:
		return "default"
	}
}

func ReviewConfirmation(in Input) Input {
	in.ReviewConfirmation = true
	in.Purpose = "review-confirmation"
	return in
}

func ConfigFingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		part = boundedFingerprintInput(part)
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func boundedFingerprintInput(part string) string {
	const edge = 512
	if len(part) <= edge*2 {
		return part
	}
	// Include length plus both edges. This is bounded, deterministic, and
	// avoids silently treating edits in the tail of a large config as stale.
	return part[:edge] + "\x00len=" + strconv.Itoa(len(part)) + "\x00" + part[len(part)-edge:]
}

func CanonicalRepository(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host := strings.ToLower(strings.TrimSuffix(u.Host, "."))
		p := strings.Trim(path.Clean(u.Path), "/")
		p = strings.TrimSuffix(p, ".git")
		return host + "/" + strings.ToLower(p)
	}
	raw = strings.TrimPrefix(raw, "git@")
	raw = strings.TrimSuffix(raw, ".git")
	return strings.ToLower(strings.Trim(strings.ReplaceAll(raw, ":", "/"), "/"))
}

func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

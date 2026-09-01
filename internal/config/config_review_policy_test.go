package config

import (
	"strings"
	"testing"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// The zero policy must reproduce the pre-policy behavior exactly: unbounded
// fix loop, only auto-fix findings fixed automatically, park on any warning.
func TestReviewPolicy_DefaultsPreservePrePolicyBehavior(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{}).Review
	if got.MaxFixRounds != 0 || got.AutoFixAskUser || got.GateSeverity != ReviewGateSeverityWarning {
		t.Fatalf("default review policy = %+v, want unbounded / no ask-user fixing / warning gate", got)
	}
	if !got.FixRoundsRemain(0) || !got.FixRoundsRemain(999) {
		t.Fatalf("an unbounded policy must always allow another fix round")
	}
}

func TestReviewPolicy_GlobalThenTrustedRepoOverride(t *testing.T) {
	global := &GlobalConfig{Review: ReviewPolicyRaw{MaxFixRounds: intPtr(3), GateSeverity: "ERROR"}}
	repo := &RepoConfig{Review: ReviewRaw{ReviewPolicyRaw: ReviewPolicyRaw{MaxFixRounds: intPtr(2), AutoFixAskUser: boolPtr(true)}}}
	got := Merge(global, repo).Review
	if got.MaxFixRounds != 2 {
		t.Fatalf("max_fix_rounds = %d, want the repo value 2 over the global 3", got.MaxFixRounds)
	}
	if !got.AutoFixAskUser {
		t.Fatalf("auto_fix_ask_user = false, want the repo's true")
	}
	if got.GateSeverity != ReviewGateSeverityError {
		t.Fatalf("gate_severity = %q, want the global %q normalized to lower case", got.GateSeverity, ReviewGateSeverityError)
	}
	if got.FixRoundsRemain(2) || !got.FixRoundsRemain(1) {
		t.Fatalf("FixRoundsRemain must allow exactly max_fix_rounds fix rounds")
	}

	// An explicit repo zero disables a global cap: the pointer distinguishes
	// "unset" from "set to 0".
	repo.Review.MaxFixRounds = intPtr(0)
	if got := Merge(global, repo).Review; got.MaxFixRounds != 0 {
		t.Fatalf("max_fix_rounds = %d, want an explicit repo 0 to win", got.MaxFixRounds)
	}
}

// The policy widens what the pipeline does on its own and where it stops, so
// like path_instructions it is read only from the trusted default-branch copy.
func TestEffectiveRepoConfig_ReviewPolicyTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Review: ReviewRaw{ReviewPolicyRaw: ReviewPolicyRaw{MaxFixRounds: intPtr(99), AutoFixAskUser: boolPtr(true), GateSeverity: "error"}}}
	trusted := &RepoConfig{Review: ReviewRaw{ReviewPolicyRaw: ReviewPolicyRaw{MaxFixRounds: intPtr(2)}}}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.Review.MaxFixRounds == nil || *got.Review.MaxFixRounds != 2 || got.Review.AutoFixAskUser != nil || got.Review.GateSeverity != "" {
		t.Fatalf("review policy = %+v, want only the trusted copy's max_fix_rounds: 2", got.Review.ReviewPolicyRaw)
	}
	for _, opt := range []bool{false, true} {
		got = EffectiveRepoConfig(pushed, &RepoConfig{}, opt)
		if got.Review.MaxFixRounds != nil || got.Review.AutoFixAskUser != nil || got.Review.GateSeverity != "" {
			t.Fatalf("allow_repo_commands=%v: pushed-only review policy %+v must be discarded", opt, got.Review.ReviewPolicyRaw)
		}
	}
	got = EffectiveRepoConfig(pushed, nil, false)
	if got.Review.MaxFixRounds != nil {
		t.Fatalf("without a trusted copy the pushed review policy must be discarded, got %+v", got.Review.ReviewPolicyRaw)
	}
}

func TestParseRepoConfig_ReviewPolicy(t *testing.T) {
	cfg, err := parseRepoConfig([]byte("review:\n  max_fix_rounds: 2\n  auto_fix_ask_user: true\n  gate_severity: error\n  path_instructions:\n    - path: \"docs/**\"\n      instructions: prose only\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Review.MaxFixRounds == nil || *cfg.Review.MaxFixRounds != 2 || cfg.Review.AutoFixAskUser == nil || !*cfg.Review.AutoFixAskUser || cfg.Review.GateSeverity != "error" {
		t.Fatalf("review policy = %+v, want 2 / true / error", cfg.Review.ReviewPolicyRaw)
	}
	if len(cfg.Review.PathInstructions) != 1 {
		t.Fatalf("path_instructions must still parse beside the inline policy, got %v", cfg.Review.PathInstructions)
	}

	if _, err := parseRepoConfig([]byte("review:\n  max_fix_rounds: -1\n")); err == nil || !strings.Contains(err.Error(), "max_fix_rounds") {
		t.Fatalf("negative max_fix_rounds must be rejected, got %v", err)
	}
	if _, err := parseRepoConfig([]byte("review:\n  gate_severity: info\n")); err == nil || !strings.Contains(err.Error(), "gate_severity") {
		t.Fatalf("gate_severity below warning must be rejected, got %v", err)
	}
}

func TestValidateReviewPolicyRaw(t *testing.T) {
	for _, ok := range []ReviewPolicyRaw{{}, {MaxFixRounds: intPtr(0)}, {GateSeverity: "Error"}, {GateSeverity: " warning "}} {
		if err := validateReviewPolicyRaw(ok); err != nil {
			t.Fatalf("%+v: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []ReviewPolicyRaw{{MaxFixRounds: intPtr(-2)}, {GateSeverity: "blocking"}} {
		if err := validateReviewPolicyRaw(bad); err == nil {
			t.Fatalf("%+v: want an error", bad)
		}
	}
}

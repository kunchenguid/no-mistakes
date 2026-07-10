package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRoutingFingerprintStableAndSensitive proves the routing fingerprint is a
// deterministic content hash of the model-selection contract: equal contracts
// hash equally regardless of map iteration order, and any change to a
// candidate or route changes the hash.
func TestRoutingFingerprintStableAndSensitive(t *testing.T) {
	a := DefaultRoutingConfig()
	if a.Fingerprint() == "" {
		t.Fatal("fingerprint must be non-empty for the default routing contract")
	}
	// Stable across calls and independently-built equal configs (map order).
	if a.Fingerprint() != DefaultRoutingConfig().Fingerprint() {
		t.Fatal("equal routing contracts must fingerprint equally")
	}

	// Sensitive to a candidate change.
	b := DefaultRoutingConfig()
	prof := b.Profiles[ProfileFixBalanced]
	prof.Candidates = append([]Candidate(nil), prof.Candidates...)
	prof.Candidates[0].Model = "some-other-model"
	b.Profiles[ProfileFixBalanced] = prof
	if b.Fingerprint() == a.Fingerprint() {
		t.Fatal("changing a candidate model must change the fingerprint")
	}

	// Sensitive to a route change.
	c := DefaultRoutingConfig()
	c.Routes[types.PurposeInitialReview] = Route{ProfileAuthorityStrong}
	if c.Fingerprint() == a.Fingerprint() {
		t.Fatal("changing a route must change the fingerprint")
	}
}

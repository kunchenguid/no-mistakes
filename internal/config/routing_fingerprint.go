package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Fingerprint returns a deterministic content hash of the routing contract
// (Runners, Profiles, Candidates, Routes). It identifies the exact
// model-selection policy a routed cohort ran under, so the canary can record
// which policy produced its comparison and later runs can detect a policy
// change. Equal contracts hash equally regardless of Go's randomized map
// iteration order (encoding/json emits map keys sorted), while candidate and
// route slices keep their significant order; any change to a runner, profile,
// candidate, or route changes the hash.
func (rc RoutingConfig) Fingerprint() string {
	data, err := json.Marshal(rc)
	if err != nil {
		// RoutingConfig contains only JSON-encodable fields, so marshalling
		// never fails; fall back to a fixed sentinel rather than an empty
		// string so an impossible error can never alias two contracts.
		return "unfingerprintable-routing-config"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

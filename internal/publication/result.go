package publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

type ResultStatus string

const (
	StatusChecking      ResultStatus = "CHECKING"
	StatusReadyForPush  ResultStatus = "READY_FOR_PUSH"
	StatusReadyForPR    ResultStatus = "READY_FOR_PR"
	StatusCIObserving   ResultStatus = "CI_OBSERVING"
	StatusReady         ResultStatus = "READY"
	StatusFailed        ResultStatus = "FAILED"
	StatusDrift         ResultStatus = "DRIFT"
	StatusDenied        ResultStatus = "DENIED"
	StatusEffectUnknown ResultStatus = "EFFECT_UNKNOWN"
)

// Result is the closed v1 machine response. READY is its sole successful
// terminal outcome.
type Result struct {
	Protocol      string           `json:"protocol"`
	PublicationID string           `json:"publication_id"`
	RunID         string           `json:"run_id"`
	HeadSHA       string           `json:"head_sha"`
	Status        ResultStatus     `json:"status"`
	Challenge     *EffectChallenge `json:"challenge,omitempty"`
}

func ParseResult(raw []byte) (Result, error) {
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("publication result is empty")
	}
	if err := rejectDuplicateObjectKeys(raw); err != nil {
		return Result{}, fmt.Errorf("invalid publication result: %w", err)
	}
	var result Result
	if err := decodeClosedJSON(raw, &result); err != nil {
		return Result{}, fmt.Errorf("invalid publication result: %w", err)
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return Result{}, fmt.Errorf("marshal canonical publication result: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return Result{}, fmt.Errorf("publication result is not canonical JSON")
	}
	if err := result.validate(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (r Result) validate() error {
	if r.Protocol != ProtocolV1 {
		return fmt.Errorf("result protocol must be %q", ProtocolV1)
	}
	if !isLowerHex(r.PublicationID, sha256.Size*2) {
		return fmt.Errorf("publication ID must be 64 lowercase hexadecimal characters")
	}
	if err := validateBoundedText("run ID", r.RunID, MaxFactoryRunIDBytes); err != nil {
		return err
	}
	if !isLowerHex(r.HeadSHA, 40) {
		return fmt.Errorf("result head SHA must be 40 lowercase hexadecimal characters")
	}
	if !r.Status.valid() {
		return fmt.Errorf("unknown publication result status %q", r.Status)
	}
	if r.Status == StatusReadyForPush || r.Status == StatusReadyForPR {
		if r.Challenge == nil {
			return fmt.Errorf("publication Owner-gate result is missing its exact effect challenge")
		}
		if err := r.validateChallenge(); err != nil {
			return err
		}
	} else if r.Challenge != nil {
		return fmt.Errorf("publication result status %s must not carry an effect challenge", r.Status)
	}
	return nil
}

func (r Result) validateChallenge() error {
	challenge := r.Challenge
	if challenge.PublicationID != r.PublicationID || challenge.CommitSHA != r.HeadSHA || challenge.Attempt != 1 {
		return fmt.Errorf("publication effect challenge is not bound to the exact result and first attempt")
	}
	if !isLowerHex(challenge.EffectDigest, sha256.Size*2) ||
		!isLowerHex(challenge.DecisionDigest, sha256.Size*2) ||
		!isLowerHex(challenge.DenyDecisionDigest, sha256.Size*2) {
		return fmt.Errorf("publication effect challenge digests must be lowercase SHA-256 values")
	}
	wantGo, err := decisionDigestFor(*challenge, DecisionGo)
	if err != nil {
		return err
	}
	wantDeny, err := decisionDigestFor(*challenge, DecisionDeny)
	if err != nil {
		return err
	}
	if challenge.DecisionDigest != wantGo || challenge.DenyDecisionDigest != wantDeny {
		return fmt.Errorf("publication effect challenge decision digests do not bind its exact fields")
	}

	switch r.Status {
	case StatusReadyForPush:
		if challenge.Kind != EffectPush || challenge.Marker != "" || challenge.PreparedDraft != "" ||
			challenge.BaseRef != "" || challenge.DraftSHA256 != "" {
			return fmt.Errorf("Push challenge carries the wrong effect kind or PR-only bindings")
		}
		if err := validateBoundedText("Push challenge remote identity", challenge.RemoteIdentity, maxRemoteIdentityBytes); err != nil {
			return err
		}
		if !isFullBranchRef(challenge.DestinationRef) || challenge.HeadRef != challenge.DestinationRef {
			return fmt.Errorf("Push challenge destination and head must be the same full branch ref")
		}
	case StatusReadyForPR:
		wantMarker := fmt.Sprintf("<!-- no-mistakes-factory-publication-v1:%s:%s -->", r.PublicationID, r.HeadSHA)
		if challenge.Kind != EffectPR ||
			!isFullBranchRef(challenge.BaseRef) || !isFullBranchRef(challenge.HeadRef) ||
			challenge.DestinationRef != challenge.HeadRef ||
			challenge.Marker != wantMarker || !isLowerHex(challenge.DraftSHA256, sha256.Size*2) {
			return fmt.Errorf("PR challenge does not carry its exact base, head, marker, and draft bindings")
		}
		if err := validateBoundedText("PR challenge remote identity", challenge.RemoteIdentity, maxRemoteIdentityBytes); err != nil {
			return err
		}
		if challenge.PreparedDraft == "" || !utf8.ValidString(challenge.PreparedDraft) || len(challenge.PreparedDraft) > MaxPreparedPRDraftBytes {
			return fmt.Errorf("PR challenge prepared draft must be bounded non-empty UTF-8")
		}
		if sha256Hex([]byte(challenge.PreparedDraft)) != challenge.DraftSHA256 ||
			bytes.Count([]byte(challenge.PreparedDraft), []byte(challenge.Marker)) != 1 {
			return fmt.Errorf("PR challenge prepared draft bytes do not match their digest and exact marker")
		}
	}
	return nil
}

func (s ResultStatus) valid() bool {
	switch s {
	case StatusChecking, StatusReadyForPush, StatusReadyForPR, StatusCIObserving,
		StatusReady, StatusFailed, StatusDrift, StatusDenied, StatusEffectUnknown:
		return true
	default:
		return false
	}
}

func (r Result) ExitCode() int {
	if r.Status == StatusReady {
		return 0
	}
	return 1
}

package publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// AuthorizationEnvelope is the closed public representation of one Owner
// decision. The domain Authorization deliberately remains independent of wire
// versioning; this envelope performs that boundary conversion.
type AuthorizationEnvelope struct {
	Protocol       string     `json:"protocol"`
	Decision       Decision   `json:"decision"`
	PublicationID  string     `json:"publication_id"`
	Kind           EffectKind `json:"kind"`
	Attempt        int        `json:"attempt"`
	CommitSHA      string     `json:"commit_sha"`
	RemoteIdentity string     `json:"remote_identity"`
	DestinationRef string     `json:"destination_ref"`
	BaseRef        string     `json:"base_ref"`
	HeadRef        string     `json:"head_ref"`
	DraftSHA256    string     `json:"draft_sha256"`
	EffectDigest   string     `json:"effect_digest"`
	DecisionDigest string     `json:"decision_digest"`
}

type StatusQuery struct {
	Protocol      string `json:"protocol"`
	PublicationID string `json:"publication_id"`
}

func ParseAuthorization(raw []byte) (AuthorizationEnvelope, error) {
	var envelope AuthorizationEnvelope
	if err := parseCanonicalEnvelope(raw, &envelope, "publication authorization"); err != nil {
		return AuthorizationEnvelope{}, err
	}
	if envelope.Protocol != ProtocolV1 {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization protocol must be %q", ProtocolV1)
	}
	if envelope.Decision != DecisionGo && envelope.Decision != DecisionDeny {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization decision must be GO or DENY")
	}
	if !isLowerHex(envelope.PublicationID, sha256.Size*2) {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization publication ID must be 64 lowercase hexadecimal characters")
	}
	if envelope.Attempt != 1 {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization attempt must be exactly 1 in protocol v1")
	}
	if !isLowerHex(envelope.CommitSHA, 40) {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization commit SHA must be 40 lowercase hexadecimal characters")
	}
	if !isLowerHex(envelope.EffectDigest, sha256.Size*2) || !isLowerHex(envelope.DecisionDigest, sha256.Size*2) {
		return AuthorizationEnvelope{}, fmt.Errorf("authorization effect and decision digests must be lowercase SHA-256 values")
	}

	switch envelope.Kind {
	case EffectPush:
		if err := validateBoundedText("authorization remote identity", envelope.RemoteIdentity, maxRemoteIdentityBytes); err != nil {
			return AuthorizationEnvelope{}, err
		}
		if !isFullBranchRef(envelope.DestinationRef) || !isFullBranchRef(envelope.HeadRef) || envelope.DestinationRef != envelope.HeadRef {
			return AuthorizationEnvelope{}, fmt.Errorf("push authorization destination and head must be the same full branch ref")
		}
		if envelope.BaseRef != "" || envelope.DraftSHA256 != "" {
			return AuthorizationEnvelope{}, fmt.Errorf("push authorization must not carry PR-only bindings")
		}
	case EffectPR:
		if err := validateBoundedText("authorization remote identity", envelope.RemoteIdentity, maxRemoteIdentityBytes); err != nil {
			return AuthorizationEnvelope{}, err
		}
		if !isFullBranchRef(envelope.BaseRef) || !isFullBranchRef(envelope.HeadRef) {
			return AuthorizationEnvelope{}, fmt.Errorf("PR authorization base and head must be full branch refs")
		}
		if envelope.DestinationRef != envelope.HeadRef {
			return AuthorizationEnvelope{}, fmt.Errorf("PR authorization destination and head must be the same full branch ref")
		}
		if !isLowerHex(envelope.DraftSHA256, sha256.Size*2) {
			return AuthorizationEnvelope{}, fmt.Errorf("PR authorization draft digest must be a lowercase SHA-256")
		}
	default:
		return AuthorizationEnvelope{}, fmt.Errorf("authorization effect kind must be push or pr")
	}
	return envelope, nil
}

func (e AuthorizationEnvelope) Authorization() Authorization {
	return Authorization{
		Decision:       e.Decision,
		PublicationID:  e.PublicationID,
		Kind:           e.Kind,
		Attempt:        e.Attempt,
		CommitSHA:      e.CommitSHA,
		RemoteIdentity: e.RemoteIdentity,
		DestinationRef: e.DestinationRef,
		BaseRef:        e.BaseRef,
		HeadRef:        e.HeadRef,
		DraftSHA256:    e.DraftSHA256,
		EffectDigest:   e.EffectDigest,
		DecisionDigest: e.DecisionDigest,
	}
}

func ParseStatusQuery(raw []byte) (StatusQuery, error) {
	var query StatusQuery
	if err := parseCanonicalEnvelope(raw, &query, "publication status query"); err != nil {
		return StatusQuery{}, err
	}
	if query.Protocol != ProtocolV1 {
		return StatusQuery{}, fmt.Errorf("status query protocol must be %q", ProtocolV1)
	}
	if !isLowerHex(query.PublicationID, sha256.Size*2) {
		return StatusQuery{}, fmt.Errorf("status query publication ID must be 64 lowercase hexadecimal characters")
	}
	return query, nil
}

func parseCanonicalEnvelope(raw []byte, dst any, name string) error {
	if len(raw) == 0 {
		return fmt.Errorf("%s is empty", name)
	}
	if err := rejectDuplicateObjectKeys(raw); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	if err := decodeClosedJSON(raw, dst); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return fmt.Errorf("marshal canonical %s: %w", name, err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not canonical JSON", name)
	}
	return nil
}

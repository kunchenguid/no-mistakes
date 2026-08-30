package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type EffectKind string

const (
	EffectPush EffectKind = "push"
	EffectPR   EffectKind = "pr"

	MaxPreparedPRDraftBytes = 1 << 20
)

type Decision string

const (
	DecisionGo   Decision = "GO"
	DecisionDeny Decision = "DENY"
)

// EffectChallenge is the exact, single-use authorization surface for one
// mutating publication effect.
type EffectChallenge struct {
	PublicationID      string     `json:"publication_id"`
	Kind               EffectKind `json:"kind"`
	Attempt            int        `json:"attempt"`
	CommitSHA          string     `json:"commit_sha"`
	RemoteIdentity     string     `json:"remote_identity"`
	DestinationRef     string     `json:"destination_ref"`
	BaseRef            string     `json:"base_ref"`
	HeadRef            string     `json:"head_ref"`
	Marker             string     `json:"marker"`
	PreparedDraft      string     `json:"prepared_draft"`
	DraftSHA256        string     `json:"draft_sha256"`
	EffectDigest       string     `json:"effect_digest"`
	DecisionDigest     string     `json:"decision_digest"`
	DenyDecisionDigest string     `json:"deny_decision_digest"`
}

type Authorization struct {
	Decision       Decision
	PublicationID  string
	Kind           EffectKind
	Attempt        int
	CommitSHA      string
	RemoteIdentity string
	DestinationRef string
	BaseRef        string
	HeadRef        string
	DraftSHA256    string
	EffectDigest   string
	DecisionDigest string
}

type PushEffectRequest struct {
	PublicationID  string
	RepositoryID   string
	CommitSHA      string
	RemoteIdentity string
	DestinationRef string
	EffectDigest   string
}

type PushObservation struct {
	RemoteHeadSHA string
}

type PushPort interface {
	PublishExact(ctx context.Context, request PushEffectRequest) error
	ObserveExact(ctx context.Context, request PushEffectRequest) (PushObservation, error)
}

type PREffectRequest struct {
	PublicationID string
	RepositoryID  string
	BaseRef       string
	HeadRef       string
	CommitSHA     string
	Marker        string
	Draft         []byte
	DraftSHA256   string
	EffectDigest  string
}

type PRReconcileQuery struct {
	PublicationID string
	RepositoryID  string
	BaseRef       string
	HeadRef       string
	CommitSHA     string
	Marker        string
	DraftSHA256   string
}

type PRObservation struct {
	RepositoryID string
	BaseRef      string
	HeadRef      string
	HeadSHA      string
	Marker       string
	DraftSHA256  string
	Number       string
}

type PRPort interface {
	CreateExact(ctx context.Context, request PREffectRequest) error
	FindExact(ctx context.Context, query PRReconcileQuery) ([]PRObservation, error)
}

type CIQuery struct {
	PublicationID string
	CommitSHA     string
}

type CICheckStatus string

const (
	CICheckPass      CICheckStatus = "PASS"
	CICheckFail      CICheckStatus = "FAIL"
	CICheckPending   CICheckStatus = "PENDING"
	CICheckCancelled CICheckStatus = "CANCELLED"
	CICheckSkipped   CICheckStatus = "SKIPPED"
	CICheckPartial   CICheckStatus = "PARTIAL"
	CICheckUnknown   CICheckStatus = "UNKNOWN"
)

type CICheck struct {
	Name    string
	HeadSHA string
	Status  CICheckStatus
}

type CIObservation struct {
	HeadSHA string
	Checks  []CICheck
}

type CIPort interface {
	ObserveExact(ctx context.Context, query CIQuery) (CIObservation, error)
}

func effectDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal publication effect binding: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func decisionDigest(challenge EffectChallenge) (string, error) {
	return decisionDigestFor(challenge, DecisionGo)
}

// BindEffectChallengeDecisions derives the two public, decision-specific
// digests from one exact challenge. The operation is deterministic and carries
// no authority; Manager still accepts a decision only when the durable effect
// journal and immutable publication request match every field.
func BindEffectChallengeDecisions(challenge EffectChallenge) (EffectChallenge, error) {
	goDigest, err := decisionDigestFor(challenge, DecisionGo)
	if err != nil {
		return EffectChallenge{}, err
	}
	denyDigest, err := decisionDigestFor(challenge, DecisionDeny)
	if err != nil {
		return EffectChallenge{}, err
	}
	challenge.DecisionDigest = goDigest
	challenge.DenyDecisionDigest = denyDigest
	return challenge, nil
}

func decisionDigestFor(challenge EffectChallenge, decision Decision) (string, error) {
	if decision != DecisionGo && decision != DecisionDeny {
		return "", fmt.Errorf("unknown publication decision %q", decision)
	}
	// Both published decision digests and the inspectable draft bytes are
	// intentionally excluded from the preimage. DraftSHA256 is the exact raw-
	// byte binding; PreparedDraft lets the Owner inspect those bytes.
	return effectDigest(struct {
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
	}{
		Decision:       decision,
		PublicationID:  challenge.PublicationID,
		Kind:           challenge.Kind,
		Attempt:        challenge.Attempt,
		CommitSHA:      challenge.CommitSHA,
		RemoteIdentity: challenge.RemoteIdentity,
		DestinationRef: challenge.DestinationRef,
		BaseRef:        challenge.BaseRef,
		HeadRef:        challenge.HeadRef,
		DraftSHA256:    challenge.DraftSHA256,
		EffectDigest:   challenge.EffectDigest,
	})
}

func authorizationMatches(challenge EffectChallenge, authorization Authorization) bool {
	expectedDigest := challenge.DecisionDigest
	if authorization.Decision == DecisionDeny {
		expectedDigest = challenge.DenyDecisionDigest
	}
	return (authorization.Decision == DecisionGo || authorization.Decision == DecisionDeny) &&
		authorization.PublicationID == challenge.PublicationID &&
		authorization.Kind == challenge.Kind &&
		authorization.Attempt == challenge.Attempt &&
		authorization.CommitSHA == challenge.CommitSHA &&
		authorization.RemoteIdentity == challenge.RemoteIdentity &&
		authorization.DestinationRef == challenge.DestinationRef &&
		authorization.BaseRef == challenge.BaseRef &&
		authorization.HeadRef == challenge.HeadRef &&
		authorization.DraftSHA256 == challenge.DraftSHA256 &&
		authorization.EffectDigest == challenge.EffectDigest &&
		authorization.DecisionDigest == expectedDigest
}

func finalizedPRDraft(body []byte, marker string) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("PR draft is empty")
	}
	if bytes.Contains(body, []byte(marker)) {
		return nil, fmt.Errorf("PR draft already contains the publication reconciliation marker")
	}
	draft := append([]byte(nil), body...)
	if !bytes.HasSuffix(draft, []byte("\n")) {
		draft = append(draft, '\n')
	}
	draft = append(draft, '\n')
	draft = append(draft, marker...)
	draft = append(draft, '\n')
	return draft, nil
}

func exactPRMatches(observations []PRObservation, query PRReconcileQuery) []PRObservation {
	matches := make([]PRObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.RepositoryID == query.RepositoryID &&
			observation.BaseRef == query.BaseRef &&
			observation.HeadRef == query.HeadRef &&
			observation.HeadSHA == query.CommitSHA &&
			observation.Marker == query.Marker &&
			observation.DraftSHA256 == query.DraftSHA256 {
			matches = append(matches, observation)
		}
	}
	return matches
}

func exactCIPassed(observation CIObservation, headSHA string) bool {
	if observation.HeadSHA != headSHA || len(observation.Checks) == 0 {
		return false
	}
	for _, check := range observation.Checks {
		if check.Name == "" || check.HeadSHA != headSHA || check.Status != CICheckPass {
			return false
		}
	}
	return true
}

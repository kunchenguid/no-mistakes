package ownerdecision

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	ChallengeSchema = "no-mistakes.owner-decision-challenge/v1"
	EnvelopeSchema  = "no-mistakes.owner-decision-envelope/v1"

	PurposeRespond    = "respond"
	PurposeCancel     = "cancel"
	PurposeCheckpoint = "checkpoint"

	MaxChallengeLifetime = 15 * time.Minute
)

var GenesisHead = DigestBytes([]byte("no-mistakes.owner-decision-history/v1\n"))

// GenesisHeadForRun makes the controller-held initial history head an
// external commitment to the exact public key and immutable submitted run
// identity. Replacing same-UID local authority or run rows after restart
// therefore cannot create a valid alternate history with the same expected
// head.
func GenesisHeadForRun(publicKey ed25519.PublicKey, repoID, branch, initialHeadSHA string) (string, error) {
	keyID, err := KeyID(publicKey)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repoID) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(initialHeadSHA) == "" {
		return "", errors.New("owner decision: immutable run identity is incomplete")
	}
	return DigestBytes([]byte(
		"no-mistakes.owner-decision-history/v1\n" +
			"key:" + keyID + "\n" +
			"repo:" + repoID + "\n" +
			"branch:" + branch + "\n" +
			"initial-head:" + initialHeadSHA + "\n",
	)), nil
}

// Challenge binds a controller decision to one immutable run and gate state.
// Its field order is the canonical JSON order used for signing.
type Challenge struct {
	Schema  string `json:"schema"`
	Purpose string `json:"purpose"`
	RunID   string `json:"run_id"`
	RepoID  string `json:"repo_id"`
	Branch  string `json:"branch"`
	// HeadSHA is the immutable submitted head sealed into the authority and
	// genesis. GateHeadSHA is the current run/worktree head at this gate; it
	// may advance only through authorized pipeline work.
	HeadSHA        string         `json:"head_sha"`
	GateHeadSHA    string         `json:"gate_head_sha"`
	Step           types.StepName `json:"step,omitempty"`
	StepResultID   string         `json:"step_result_id,omitempty"`
	RoundID        string         `json:"round_id,omitempty"`
	FindingsDigest string         `json:"findings_digest,omitempty"`
	PreviousHead   string         `json:"previous_head"`
	Nonce          string         `json:"nonce"`
	IssuedAt       int64          `json:"issued_at"`
	ExpiresAt      int64          `json:"expires_at"`
}

// Response is the complete state-changing instruction authorized by the
// controller. A cancel envelope uses ActionAbort and no finding selections.
type Response struct {
	Action        types.ApprovalAction `json:"action"`
	FindingIDs    []string             `json:"finding_ids,omitempty"`
	Instructions  map[string]string    `json:"instructions,omitempty"`
	AddedFindings []types.Finding      `json:"added_findings,omitempty"`
}

// Projection is the exact mutable round state deterministically authorized by
// a fix response and its immutable gate findings. The external history head
// chains only signed envelopes so a controller can derive it independently;
// this projection is therefore verified separately, byte for byte.
type Projection struct {
	SelectedFindingIDs string
	UserFindingsJSON   *string
}

// Envelope carries a canonical challenge and response plus an Ed25519
// signature over their exact schema-bound JSON representation.
type Envelope struct {
	Schema    string    `json:"schema"`
	Challenge Challenge `json:"challenge"`
	Response  Response  `json:"response"`
	Signature string    `json:"signature"`
}

type unsignedEnvelope struct {
	Schema    string    `json:"schema"`
	Challenge Challenge `json:"challenge"`
	Response  Response  `json:"response"`
}

func DigestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func Sign(privateKey ed25519.PrivateKey, challenge Challenge, response Response) (Envelope, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Envelope{}, fmt.Errorf("owner decision: invalid Ed25519 private key length %d", len(privateKey))
	}
	if err := validateChallenge(challenge); err != nil {
		return Envelope{}, err
	}
	if err := validateResponse(challenge.Purpose, response); err != nil {
		return Envelope{}, err
	}
	unsigned := unsignedEnvelope{Schema: EnvelopeSchema, Challenge: challenge, Response: response}
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return Envelope{}, fmt.Errorf("owner decision: encode signing payload: %w", err)
	}
	return Envelope{
		Schema:    EnvelopeSchema,
		Challenge: challenge,
		Response:  response,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}, nil
}

// Verify proves an envelope is exact for the supplied challenge and currently
// usable. Historical verification should use VerifySignature because expiry
// limits admission, not the durability of an already-admitted decision.
func Verify(publicKey ed25519.PublicKey, envelope Envelope, expected Challenge, now time.Time) error {
	if err := VerifySignature(publicKey, envelope); err != nil {
		return err
	}
	actual, err := json.Marshal(envelope.Challenge)
	if err != nil {
		return fmt.Errorf("owner decision: encode challenge: %w", err)
	}
	want, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("owner decision: encode expected challenge: %w", err)
	}
	if !bytes.Equal(actual, want) {
		return errors.New("owner decision: signed challenge does not match the active gate")
	}
	if now.Unix() < envelope.Challenge.IssuedAt {
		return errors.New("owner decision: challenge is not yet valid")
	}
	if now.Unix() >= envelope.Challenge.ExpiresAt {
		return errors.New("owner decision: challenge has expired")
	}
	return nil
}

// VerifySignature validates schemas, values, and the Ed25519 signature without
// applying the admission-time validity window.
func VerifySignature(publicKey ed25519.PublicKey, envelope Envelope) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("owner decision: invalid Ed25519 public key length %d", len(publicKey))
	}
	if envelope.Schema != EnvelopeSchema {
		return fmt.Errorf("owner decision: unsupported envelope schema %q", envelope.Schema)
	}
	if err := validateChallenge(envelope.Challenge); err != nil {
		return err
	}
	if err := validateResponse(envelope.Challenge.Purpose, envelope.Response); err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("owner decision: invalid signature encoding")
	}
	payload, err := json.Marshal(unsignedEnvelope{
		Schema:    envelope.Schema,
		Challenge: envelope.Challenge,
		Response:  envelope.Response,
	})
	if err != nil {
		return fmt.Errorf("owner decision: encode signing payload: %w", err)
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return errors.New("owner decision: signature verification failed")
	}
	return nil
}

func EnvelopeDigest(envelope Envelope) (string, error) {
	if envelope.Schema != EnvelopeSchema {
		return "", fmt.Errorf("owner decision: unsupported envelope schema %q", envelope.Schema)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("owner decision: encode envelope: %w", err)
	}
	return DigestBytes(payload), nil
}

// MaterializeProjection derives the only valid round projection for a signed
// fix response. It deliberately mirrors the pipeline's user-override wire
// format using the shared findings primitives rather than trusting a
// daemon-supplied projection.
func MaterializeProjection(findingsRaw string, response Response) (Projection, error) {
	if response.Action != types.ActionFix {
		return Projection{}, errors.New("owner decision: only fix responses have a selected-findings projection")
	}
	findings, err := types.ParseFindingsJSON(findingsRaw)
	if err != nil {
		return Projection{}, fmt.Errorf("owner decision: decode gate findings: %w", err)
	}
	var selected types.Findings
	if len(response.FindingIDs) == 0 {
		selected = types.Findings{
			Summary:        "0 selected findings",
			Tested:         findings.Tested,
			TestingSummary: findings.TestingSummary,
			RiskLevel:      findings.RiskLevel,
			RiskRationale:  findings.RiskRationale,
			RiskScope:      findings.RiskScope,
		}
	} else {
		selected = types.FilterFindings(findings, response.FindingIDs)
	}
	selectedRaw, err := types.MarshalFindingsJSON(selected)
	if err != nil {
		return Projection{}, fmt.Errorf("owner decision: encode selected findings: %w", err)
	}
	merged := selected
	if len(response.Instructions) != 0 || len(response.AddedFindings) != 0 {
		merged = types.MergeUserOverrides(selected, response.Instructions, response.AddedFindings)
	}
	mergedRaw, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		return Projection{}, fmt.Errorf("owner decision: encode projected findings: %w", err)
	}

	ids := slices.Clone(response.FindingIDs)
	seen := make(map[string]bool, len(ids)+len(merged.Items))
	for _, id := range ids {
		seen[id] = true
	}
	for _, finding := range merged.Items {
		if finding.ID == "" || seen[finding.ID] {
			continue
		}
		ids = append(ids, finding.ID)
		seen[finding.ID] = true
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return Projection{}, fmt.Errorf("owner decision: encode selected finding ids: %w", err)
	}
	projection := Projection{SelectedFindingIDs: string(idsJSON)}
	if mergedRaw != selectedRaw {
		projection.UserFindingsJSON = &mergedRaw
	}
	return projection, nil
}

func NextHead(previousHead, envelopeDigest string) (string, error) {
	if !validDigest(previousHead) {
		return "", errors.New("owner decision: invalid previous history head")
	}
	if !validDigest(envelopeDigest) {
		return "", errors.New("owner decision: invalid envelope digest")
	}
	return DigestBytes([]byte(previousHead + "\n" + envelopeDigest + "\n")), nil
}

func (envelope Envelope) Clone() Envelope {
	clone := envelope
	clone.Response.FindingIDs = slices.Clone(envelope.Response.FindingIDs)
	clone.Response.AddedFindings = slices.Clone(envelope.Response.AddedFindings)
	if envelope.Response.Instructions != nil {
		clone.Response.Instructions = make(map[string]string, len(envelope.Response.Instructions))
		for key, value := range envelope.Response.Instructions {
			clone.Response.Instructions[key] = value
		}
	}
	return clone
}

func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("owner decision: public key must be base64-encoded Ed25519 bytes")
	}
	return ed25519.PublicKey(decoded), nil
}

func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("owner decision: private key must be base64-encoded Ed25519 bytes")
	}
	return ed25519.PrivateKey(decoded), nil
}

func EncodePublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("owner decision: invalid Ed25519 public key length %d", len(publicKey))
	}
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

func EncodePrivateKey(privateKey ed25519.PrivateKey) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("owner decision: invalid Ed25519 private key length %d", len(privateKey))
	}
	return base64.StdEncoding.EncodeToString(privateKey), nil
}

func KeyID(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("owner decision: invalid Ed25519 public key length %d", len(publicKey))
	}
	return DigestBytes(publicKey), nil
}

// ValidateChallenge validates the schema and protocol bounds without signing.
func ValidateChallenge(challenge Challenge) error {
	return validateChallenge(challenge)
}

func validateChallenge(challenge Challenge) error {
	if challenge.Schema != ChallengeSchema {
		return fmt.Errorf("owner decision: unsupported challenge schema %q", challenge.Schema)
	}
	if challenge.Purpose != PurposeRespond && challenge.Purpose != PurposeCancel && challenge.Purpose != PurposeCheckpoint {
		return fmt.Errorf("owner decision: unsupported purpose %q", challenge.Purpose)
	}
	if strings.TrimSpace(challenge.RunID) == "" || strings.TrimSpace(challenge.RepoID) == "" ||
		strings.TrimSpace(challenge.Branch) == "" || strings.TrimSpace(challenge.HeadSHA) == "" || strings.TrimSpace(challenge.GateHeadSHA) == "" ||
		strings.TrimSpace(challenge.PreviousHead) == "" || strings.TrimSpace(challenge.Nonce) == "" {
		return errors.New("owner decision: challenge identity is incomplete")
	}
	if !validDigest(challenge.PreviousHead) {
		return errors.New("owner decision: invalid previous history head")
	}
	if challenge.ExpiresAt <= challenge.IssuedAt {
		return errors.New("owner decision: invalid validity window")
	}
	maxLifetimeSeconds := int64(MaxChallengeLifetime / time.Second)
	if challenge.IssuedAt > math.MaxInt64-maxLifetimeSeconds || challenge.ExpiresAt > challenge.IssuedAt+maxLifetimeSeconds {
		return errors.New("owner decision: validity window exceeds protocol maximum")
	}
	if challenge.Purpose == PurposeRespond {
		if challenge.Step == "" || challenge.StepResultID == "" || challenge.RoundID == "" ||
			!validDigest(challenge.FindingsDigest) {
			return errors.New("owner decision: response gate identity is incomplete")
		}
	} else if challenge.Step != "" || challenge.StepResultID != "" || challenge.RoundID != "" || challenge.FindingsDigest != "" {
		return errors.New("owner decision: cancel challenge contains response-gate fields")
	}
	return nil
}

func validateResponse(purpose string, response Response) error {
	switch response.Action {
	case types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAbort:
	default:
		return fmt.Errorf("owner decision: unsupported action %q", response.Action)
	}
	if purpose == PurposeCancel || purpose == PurposeCheckpoint {
		expectedAction := types.ActionAbort
		if purpose == PurposeCheckpoint {
			expectedAction = types.ActionApprove
		}
		if response.Action != expectedAction || len(response.FindingIDs) != 0 ||
			len(response.Instructions) != 0 || len(response.AddedFindings) != 0 {
			return fmt.Errorf("owner decision: %s envelope has an invalid response", purpose)
		}
		return nil
	}
	if response.Action != types.ActionFix && (len(response.FindingIDs) != 0 || len(response.Instructions) != 0 || len(response.AddedFindings) != 0) {
		return errors.New("owner decision: only a fix response may carry finding selections or overrides")
	}
	if response.Action == types.ActionFix && len(response.FindingIDs) == 0 && len(response.AddedFindings) == 0 {
		return errors.New("owner decision: fix response has no selected or added findings")
	}
	seen := make(map[string]struct{}, len(response.FindingIDs))
	for _, id := range response.FindingIDs {
		if strings.TrimSpace(id) == "" {
			return errors.New("owner decision: blank finding id")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("owner decision: duplicate finding id %q", id)
		}
		seen[id] = struct{}{}
	}
	for id := range response.Instructions {
		if _, exists := seen[id]; !exists {
			return fmt.Errorf("owner decision: instruction for unselected finding %q", id)
		}
	}
	return nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

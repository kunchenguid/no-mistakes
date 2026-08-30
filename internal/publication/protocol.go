// Package publication defines the closed machine protocol used to hand a
// completed Factory candidate to no-mistakes for publication.
package publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolV1 = "factory-publication-v1"

	PushModeExactCommit             = "exact-commit"
	PRModeCreateOrUpdateExactHead   = "create-or-update-exact-head"
	CIModeObserveExactHead          = "observe-exact-head"
	MaxFactoryRunIDBytes            = 128
	MaxBuildIntentSummaryBytes      = 4096
	MaxBuildIntentCriteria          = 64
	MaxBuildIntentCriterionBytes    = 2048
	MaxPublisherExecutablePathBytes = 4096

	maxRepositoryPathBytes = 4096
	maxRefBytes            = 1024
	maxRemoteIdentityBytes = 4096
)

// Request is the canonical v1 admission envelope. Field order is part of the
// canonical byte contract and must therefore remain stable.
type Request struct {
	Protocol     string                `json:"protocol"`
	Factory      FactoryBinding        `json:"factory"`
	WorkContract WorkContractBinding   `json:"work_contract"`
	BuildIntent  BuildIntentProjection `json:"build_intent"`
	Candidate    CandidateBinding      `json:"candidate"`
	Publisher    PublisherBinding      `json:"publisher"`
	Scopes       PublicationScopes     `json:"scopes"`
}

type FactoryBinding struct {
	RunID                string `json:"run_id"`
	TerminalT10Sequence  int64  `json:"terminal_t10_sequence"`
	RunStatePrefixSHA256 string `json:"run_state_prefix_sha256"`
	PlanBindingSHA256    string `json:"plan_binding_sha256"`
}

type WorkContractBinding struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type BuildIntentProjection struct {
	Summary            string   `json:"summary"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

type CandidateBinding struct {
	RepositoryID string `json:"repository_id"`
	HeadRef      string `json:"head_ref"`
	BaseRef      string `json:"base_ref"`
	BaseSHA      string `json:"base_sha"`
	CommitSHA    string `json:"commit_sha"`
	TreeSHA      string `json:"tree_sha"`
}

type PublisherBinding struct {
	ExecutablePath   string `json:"executable_path"`
	ExecutableSHA256 string `json:"executable_sha256"`
	BuildSHA         string `json:"build_sha"`
	Protocol         string `json:"protocol"`
}

type PublicationScopes struct {
	Push PushScope `json:"push"`
	PR   PRScope   `json:"pr"`
	CI   CIScope   `json:"ci"`
}

type PushScope struct {
	Mode           string `json:"mode"`
	RemoteIdentity string `json:"remote_identity"`
	DestinationRef string `json:"destination_ref"`
}

type PRScope struct {
	Mode    string `json:"mode"`
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
}

type CIScope struct {
	Mode string `json:"mode"`
}

// ParsedRequest retains the exact admitted bytes. PublicationID is derived
// only after those bytes have passed strict canonical and semantic validation.
type ParsedRequest struct {
	PublicationID  string
	CanonicalBytes []byte
	Request        Request
}

// ParseRequest accepts exactly the canonical JSON representation of a valid
// v1 request. In particular, it refuses duplicate and unknown keys, trailing
// input, alternate encodings, and open or mismatched bindings.
func ParseRequest(raw []byte) (ParsedRequest, error) {
	if len(raw) == 0 {
		return ParsedRequest{}, fmt.Errorf("publication request is empty")
	}
	if err := rejectDuplicateObjectKeys(raw); err != nil {
		return ParsedRequest{}, fmt.Errorf("invalid publication request: %w", err)
	}

	var request Request
	if err := decodeClosedJSON(raw, &request); err != nil {
		return ParsedRequest{}, fmt.Errorf("invalid publication request: %w", err)
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return ParsedRequest{}, fmt.Errorf("marshal canonical publication request: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ParsedRequest{}, fmt.Errorf("publication request is not canonical JSON")
	}
	if err := request.validate(); err != nil {
		return ParsedRequest{}, err
	}

	digest := sha256.Sum256(raw)
	return ParsedRequest{
		PublicationID:  hex.EncodeToString(digest[:]),
		CanonicalBytes: bytes.Clone(raw),
		Request:        request,
	}, nil
}

func (r Request) validate() error {
	if r.Protocol != ProtocolV1 {
		return fmt.Errorf("unsupported publication protocol %q", r.Protocol)
	}
	if err := validateBoundedText("factory run ID", r.Factory.RunID, MaxFactoryRunIDBytes); err != nil {
		return err
	}
	if r.Factory.TerminalT10Sequence <= 0 {
		return fmt.Errorf("terminal T10 sequence must be positive")
	}
	if !isLowerHex(r.Factory.RunStatePrefixSHA256, sha256.Size*2) {
		return fmt.Errorf("run-state prefix SHA-256 must be 64 lowercase hexadecimal characters")
	}
	if !isLowerHex(r.Factory.PlanBindingSHA256, sha256.Size*2) {
		return fmt.Errorf("PlanBinding SHA-256 must be 64 lowercase hexadecimal characters")
	}

	if !isRepositoryRelativePath(r.WorkContract.Path) {
		return fmt.Errorf("WorkContract path must be a canonical repository-relative path")
	}
	if !isLowerHex(r.WorkContract.SHA256, sha256.Size*2) {
		return fmt.Errorf("WorkContract SHA-256 must be 64 lowercase hexadecimal characters")
	}

	if err := validateBoundedText("build-intent summary", r.BuildIntent.Summary, MaxBuildIntentSummaryBytes); err != nil {
		return err
	}
	if len(r.BuildIntent.AcceptanceCriteria) == 0 || len(r.BuildIntent.AcceptanceCriteria) > MaxBuildIntentCriteria {
		return fmt.Errorf("build intent must contain between 1 and %d acceptance criteria", MaxBuildIntentCriteria)
	}
	for i, criterion := range r.BuildIntent.AcceptanceCriteria {
		if err := validateBoundedText(fmt.Sprintf("acceptance criterion %d", i), criterion, MaxBuildIntentCriterionBytes); err != nil {
			return err
		}
	}

	if !isRegisteredRepositoryID(r.Candidate.RepositoryID) {
		return fmt.Errorf("repository ID must be a canonical 26-character Crockford ULID or 12 lowercase hexadecimal characters")
	}
	if !isFullBranchRef(r.Candidate.HeadRef) {
		return fmt.Errorf("candidate head ref must be a valid full branch ref")
	}
	if !isFullBranchRef(r.Candidate.BaseRef) {
		return fmt.Errorf("candidate base ref must be a valid full branch ref")
	}
	if !isLowerHex(r.Candidate.BaseSHA, 40) {
		return fmt.Errorf("candidate base SHA must be 40 lowercase hexadecimal characters")
	}
	if !isLowerHex(r.Candidate.CommitSHA, 40) {
		return fmt.Errorf("candidate commit SHA must be 40 lowercase hexadecimal characters")
	}
	if !isLowerHex(r.Candidate.TreeSHA, 40) {
		return fmt.Errorf("candidate tree SHA must be 40 lowercase hexadecimal characters")
	}

	if !isAbsolutePortablePath(r.Publisher.ExecutablePath) || len(r.Publisher.ExecutablePath) > MaxPublisherExecutablePathBytes || !utf8.ValidString(r.Publisher.ExecutablePath) {
		return fmt.Errorf("publisher executable path must be an absolute UTF-8 path of at most %d bytes", MaxPublisherExecutablePathBytes)
	}
	if !isLowerHex(r.Publisher.ExecutableSHA256, sha256.Size*2) {
		return fmt.Errorf("publisher executable SHA-256 must be 64 lowercase hexadecimal characters")
	}
	if !isLowerHex(r.Publisher.BuildSHA, 40) {
		return fmt.Errorf("publisher build SHA must be 40 lowercase hexadecimal characters")
	}
	if r.Publisher.Protocol != ProtocolV1 {
		return fmt.Errorf("publisher protocol must be %q", ProtocolV1)
	}

	if r.Scopes.Push.Mode != PushModeExactCommit {
		return fmt.Errorf("push mode must be %q", PushModeExactCommit)
	}
	if err := validateBoundedText("push remote identity", r.Scopes.Push.RemoteIdentity, maxRemoteIdentityBytes); err != nil {
		return err
	}
	if r.Scopes.Push.DestinationRef != r.Candidate.HeadRef {
		return fmt.Errorf("push destination ref must equal the candidate head ref")
	}
	if r.Scopes.PR.Mode != PRModeCreateOrUpdateExactHead {
		return fmt.Errorf("PR mode must be %q", PRModeCreateOrUpdateExactHead)
	}
	if r.Scopes.PR.BaseRef != r.Candidate.BaseRef {
		return fmt.Errorf("PR base ref must equal the candidate base ref")
	}
	if r.Scopes.PR.HeadRef != r.Candidate.HeadRef {
		return fmt.Errorf("PR head ref must equal the candidate head ref")
	}
	if r.Scopes.CI.Mode != CIModeObserveExactHead {
		return fmt.Errorf("CI mode must be %q", CIModeObserveExactHead)
	}
	return nil
}

func decodeClosedJSON(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing input: %w", err)
	}
	return nil
}

// rejectDuplicateObjectKeys walks the token stream before typed decoding.
// encoding/json otherwise accepts the last occurrence of a duplicate key.
func rejectDuplicateObjectKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing input: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object is not closed")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array is not closed")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func validateBoundedText(name, value string, maxBytes int) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be non-empty UTF-8 text", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains NUL", name)
	}
	return nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isRegisteredRepositoryID(value string) bool {
	return isLowerHex(value, 12) || isCanonicalULID(value)
}

func isCanonicalULID(value string) bool {
	if len(value) != 26 || value[0] < '0' || value[0] > '7' {
		return false
	}
	const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for i := 0; i < len(value); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

func isRepositoryRelativePath(value string) bool {
	if !utf8.ValidString(value) || value == "" || len(value) > maxRepositoryPathBytes || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") || strings.Contains(value, "\\") || isWindowsVolumePath(value) {
		return false
	}
	if path.Clean(value) != value || value == "." {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func isAbsolutePortablePath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 && isWindowsVolumePath(value) && (value[2] == '/' || value[2] == '\\')
}

func isWindowsVolumePath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func isFullBranchRef(value string) bool {
	if !utf8.ValidString(value) || len(value) > maxRefBytes || !strings.HasPrefix(value, "refs/heads/") {
		return false
	}
	name := strings.TrimPrefix(value, "refs/heads/")
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, char := range name {
		if char <= ' ' || char == 0x7f || strings.ContainsRune(`~^:?*[\\`, char) {
			return false
		}
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return true
}

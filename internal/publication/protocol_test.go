package publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const (
	testRunStateSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testPlanSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testContractSHA = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testBaseSHA     = "9876543210abcdef9876543210abcdef98765432"
	testHeadSHA     = "dddddddddddddddddddddddddddddddddddddddd"
	testTreeSHA     = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	testBinarySHA   = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	testBuildSHA    = "0123456789abcdef0123456789abcdef01234567"
)

func validRequestBytes() []byte {
	return []byte(`{"protocol":"factory-publication-v1","factory":{"run_id":"factory-run-01","terminal_t10_sequence":10,"run_state_prefix_sha256":"` + testRunStateSHA + `","plan_binding_sha256":"` + testPlanSHA + `"},"work_contract":{"path":".agent/work-contract.toml","sha256":"` + testContractSHA + `"},"build_intent":{"summary":"Add a guarded publication handoff","acceptance_criteria":["Publish only the exact protected candidate"]},"candidate":{"repository_id":"012345abcdef","head_ref":"refs/heads/codex/factory-publication-v1","base_ref":"refs/heads/main","base_sha":"` + testBaseSHA + `","commit_sha":"` + testHeadSHA + `","tree_sha":"` + testTreeSHA + `"},"publisher":{"executable_path":"/opt/no-mistakes/bin/no-mistakes","executable_sha256":"` + testBinarySHA + `","build_sha":"` + testBuildSHA + `","protocol":"factory-publication-v1"},"scopes":{"push":{"mode":"exact-commit","remote_identity":"github.com/kunchenguid/no-mistakes","destination_ref":"refs/heads/codex/factory-publication-v1"},"pr":{"mode":"create-or-update-exact-head","base_ref":"refs/heads/main","head_ref":"refs/heads/codex/factory-publication-v1"},"ci":{"mode":"observe-exact-head"}}}`)
}

func TestParseRequestCanonicalV1BindsEveryInputAndDerivesPublicationID(t *testing.T) {
	raw := validRequestBytes()

	got, err := ParseRequest(raw)
	if err != nil {
		t.Fatalf("ParseRequest() error = %v", err)
	}

	wantID := fmt.Sprintf("%x", sha256.Sum256(raw))
	if got.PublicationID != wantID {
		t.Errorf("PublicationID = %q, want SHA-256 %q", got.PublicationID, wantID)
	}
	if !bytes.Equal(got.CanonicalBytes, raw) {
		t.Errorf("CanonicalBytes changed accepted input:\n got: %q\nwant: %q", got.CanonicalBytes, raw)
	}

	req := got.Request
	if req.Protocol != ProtocolV1 {
		t.Errorf("Protocol = %q, want %q", req.Protocol, ProtocolV1)
	}
	if req.Factory.RunID != "factory-run-01" || req.Factory.TerminalT10Sequence != 10 {
		t.Errorf("Factory binding = %+v", req.Factory)
	}
	if req.Factory.RunStatePrefixSHA256 != testRunStateSHA || req.Factory.PlanBindingSHA256 != testPlanSHA {
		t.Errorf("Factory hashes = %+v", req.Factory)
	}
	if req.WorkContract.Path != ".agent/work-contract.toml" || req.WorkContract.SHA256 != testContractSHA {
		t.Errorf("WorkContract = %+v", req.WorkContract)
	}
	if req.BuildIntent.Summary != "Add a guarded publication handoff" || len(req.BuildIntent.AcceptanceCriteria) != 1 || req.BuildIntent.AcceptanceCriteria[0] != "Publish only the exact protected candidate" {
		t.Errorf("BuildIntent = %+v", req.BuildIntent)
	}
	if req.Candidate.RepositoryID != "012345abcdef" || req.Candidate.HeadRef != "refs/heads/codex/factory-publication-v1" || req.Candidate.BaseRef != "refs/heads/main" || req.Candidate.BaseSHA != testBaseSHA || req.Candidate.CommitSHA != testHeadSHA || req.Candidate.TreeSHA != testTreeSHA {
		t.Errorf("Candidate = %+v", req.Candidate)
	}
	if req.Publisher.ExecutablePath != "/opt/no-mistakes/bin/no-mistakes" || req.Publisher.ExecutableSHA256 != testBinarySHA || req.Publisher.BuildSHA != testBuildSHA || req.Publisher.Protocol != ProtocolV1 {
		t.Errorf("Publisher = %+v", req.Publisher)
	}
	if req.Scopes.Push.Mode != PushModeExactCommit || req.Scopes.Push.RemoteIdentity != "github.com/kunchenguid/no-mistakes" || req.Scopes.Push.DestinationRef != req.Candidate.HeadRef {
		t.Errorf("Push scope = %+v", req.Scopes.Push)
	}
	if req.Scopes.PR.Mode != PRModeCreateOrUpdateExactHead || req.Scopes.PR.BaseRef != req.Candidate.BaseRef || req.Scopes.PR.HeadRef != req.Candidate.HeadRef {
		t.Errorf("PR scope = %+v", req.Scopes.PR)
	}
	if req.Scopes.CI.Mode != CIModeObserveExactHead {
		t.Errorf("CI scope = %+v", req.Scopes.CI)
	}
}

func TestParseRequestRejectsNonCanonicalAndStructurallyInvalidJSON(t *testing.T) {
	canonical := string(validRequestBytes())
	tests := map[string]string{
		"duplicate field":  strings.Replace(canonical, `{"protocol":"factory-publication-v1",`, `{"protocol":"factory-publication-v1","protocol":"factory-publication-v1",`, 1),
		"unknown field":    strings.TrimSuffix(canonical, "}") + `,"publication_id":"` + strings.Repeat("0", 64) + `"}`,
		"missing field":    strings.Replace(canonical, `,"work_contract":{"path":".agent/work-contract.toml","sha256":"`+testContractSHA+`"}`, "", 1),
		"trailing value":   canonical + `{}`,
		"leading space":    " " + canonical,
		"trailing newline": canonical + "\n",
		"reordered fields": strings.Replace(canonical,
			`{"protocol":"factory-publication-v1","factory":`,
			`{"factory":`, 1),
		"noncanonical escaped slash": strings.Replace(canonical, `refs/heads/main`, `refs\/heads\/main`, 1),
		"malformed":                  strings.TrimSuffix(canonical, "}"),
	}
	// Keep the reordered case semantically complete while moving protocol after
	// factory. Its rejection proves that canonical field order is part of the
	// byte contract, not merely an encoding preference.
	tests["reordered fields"] = strings.Replace(tests["reordered fields"],
		`},"work_contract":`, `},"protocol":"factory-publication-v1","work_contract":`, 1)

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRequest([]byte(raw)); err == nil {
				t.Fatal("ParseRequest() accepted input that is not the exact canonical v1 request")
			}
		})
	}
}

func TestParseRequestRejectsOpenOrMismatchedBindings(t *testing.T) {
	tests := map[string][]byte{
		"protocol":                 mutateRequest(t, `{"protocol":"factory-publication-v1","factory":`, `{"protocol":"factory-publication-v2","factory":`),
		"empty factory run":        mutateRequest(t, `"run_id":"factory-run-01"`, `"run_id":""`),
		"zero T10 sequence":        mutateRequest(t, `"terminal_t10_sequence":10`, `"terminal_t10_sequence":0`),
		"empty intent summary":     mutateRequest(t, `"summary":"Add a guarded publication handoff"`, `"summary":""`),
		"empty criteria":           mutateRequest(t, `"acceptance_criteria":["Publish only the exact protected candidate"]`, `"acceptance_criteria":[]`),
		"empty criterion":          mutateRequest(t, `"acceptance_criteria":["Publish only the exact protected candidate"]`, `"acceptance_criteria":[""]`),
		"run-state uppercase":      mutateRequest(t, testRunStateSHA, strings.ToUpper(testRunStateSHA)),
		"plan hash short":          mutateRequest(t, testPlanSHA, testPlanSHA[:63]),
		"contract hash non-hex":    mutateRequest(t, testContractSHA, strings.Repeat("z", 64)),
		"absolute contract path":   mutateRequest(t, `.agent/work-contract.toml`, `/tmp/work-contract.toml`),
		"escaping contract path":   mutateRequest(t, `.agent/work-contract.toml`, `../work-contract.toml`),
		"repository ID":            mutateRequest(t, `"repository_id":"012345abcdef"`, `"repository_id":"not-a-repo-id"`),
		"short candidate base":     mutateRequest(t, testBaseSHA, testBaseSHA[:39]),
		"uppercase candidate base": mutateRequest(t, testBaseSHA, strings.ToUpper(testBaseSHA)),
		"short candidate H":        mutateRequest(t, testHeadSHA, testHeadSHA[:39]),
		"uppercase tree":           mutateRequest(t, testTreeSHA, strings.ToUpper(testTreeSHA)),
		"short executable hash":    mutateRequest(t, testBinarySHA, testBinarySHA[:63]),
		"relative publisher path": mutateRequest(t,
			`"executable_path":"/opt/no-mistakes/bin/no-mistakes"`,
			`"executable_path":"bin/no-mistakes"`),
		"publisher protocol": mutateRequest(t,
			`"publisher":{"executable_path":"/opt/no-mistakes/bin/no-mistakes","executable_sha256":"`+testBinarySHA+`","build_sha":"`+testBuildSHA+`","protocol":"factory-publication-v1"}`,
			`"publisher":{"executable_path":"/opt/no-mistakes/bin/no-mistakes","executable_sha256":"`+testBinarySHA+`","build_sha":"`+testBuildSHA+`","protocol":"factory-publication-v2"}`),
		"publisher build SHA": mutateRequest(t, testBuildSHA, "dev"),
		"relative head ref": mutateRequest(t,
			`"candidate":{"repository_id":"012345abcdef","head_ref":"refs/heads/codex/factory-publication-v1"`,
			`"candidate":{"repository_id":"012345abcdef","head_ref":"codex/factory-publication-v1"`),
		"invalid base ref": mutateRequest(t,
			`"base_ref":"refs/heads/main","base_sha"`,
			`"base_ref":"refs/heads/../main","base_sha"`),
		"push mode":                 mutateRequest(t, `"mode":"exact-commit"`, `"mode":"force"`),
		"PR mode":                   mutateRequest(t, `"mode":"create-or-update-exact-head"`, `"mode":"upsert"`),
		"CI mode":                   mutateRequest(t, `"mode":"observe-exact-head"`, `"mode":"rerun"`),
		"empty remote identity":     mutateRequest(t, `"remote_identity":"github.com/kunchenguid/no-mistakes"`, `"remote_identity":""`),
		"push destination mismatch": mutateRequest(t, `"destination_ref":"refs/heads/codex/factory-publication-v1"`, `"destination_ref":"refs/heads/other"`),
		"PR base mismatch":          mutateRequest(t, `"pr":{"mode":"create-or-update-exact-head","base_ref":"refs/heads/main"`, `"pr":{"mode":"create-or-update-exact-head","base_ref":"refs/heads/other"`),
		"PR head mismatch":          mutateRequest(t, `"head_ref":"refs/heads/codex/factory-publication-v1"},"ci"`, `"head_ref":"refs/heads/other"},"ci"`),
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRequest(raw); err == nil {
				t.Fatal("ParseRequest() accepted an open, malformed, or mismatched binding")
			}
		})
	}
}

func TestParseRequestAcceptsClosedRegisteredRepositoryIDFormats(t *testing.T) {
	tests := map[string]string{
		"generated Crockford ULID": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"legacy deterministic hex": "012345abcdef",
	}
	for name, repositoryID := range tests {
		t.Run(name, func(t *testing.T) {
			raw := mutateRequest(t, `"repository_id":"012345abcdef"`, `"repository_id":"`+repositoryID+`"`)
			got, err := ParseRequest(raw)
			if err != nil {
				t.Fatalf("ParseRequest() rejected registered repository ID %q: %v", repositoryID, err)
			}
			if got.Request.Candidate.RepositoryID != repositoryID {
				t.Fatalf("repository ID = %q, want %q", got.Request.Candidate.RepositoryID, repositoryID)
			}
		})
	}
}

func TestParseRequestRejectsNonCanonicalRepositoryULIDs(t *testing.T) {
	tests := map[string]string{
		"lowercase":           "01arz3ndektsv4rrffq69g5fav",
		"mixed case":          "01ARZ3NDEKTSV4RRFFQ69G5FaV",
		"forbidden I":         "01ARZ3NDEKTSV4RRFFQ69G5FAI",
		"forbidden L":         "01ARZ3NDEKTSV4RRFFQ69G5FAL",
		"forbidden O":         "01ARZ3NDEKTSV4RRFFQ69G5FAO",
		"forbidden U":         "01ARZ3NDEKTSV4RRFFQ69G5FAU",
		"too short":           "01ARZ3NDEKTSV4RRFFQ69G5FA",
		"too long":            "01ARZ3NDEKTSV4RRFFQ69G5FAV0",
		"overflow first char": "81ARZ3NDEKTSV4RRFFQ69G5FAV",
	}
	for name, repositoryID := range tests {
		t.Run(name, func(t *testing.T) {
			raw := mutateRequest(t, `"repository_id":"012345abcdef"`, `"repository_id":"`+repositoryID+`"`)
			if _, err := ParseRequest(raw); err == nil {
				t.Fatalf("ParseRequest() accepted non-canonical repository ULID %q", repositoryID)
			}
		})
	}
}

func TestParseRequestEnforcesClosedTextBounds(t *testing.T) {
	if MaxFactoryRunIDBytes != 128 || MaxBuildIntentSummaryBytes != 4096 || MaxBuildIntentCriteria != 64 || MaxBuildIntentCriterionBytes != 2048 || MaxPublisherExecutablePathBytes != 4096 {
		t.Fatalf("unexpected public bounds: run=%d summary=%d criteria=%d criterion=%d publisher_path=%d",
			MaxFactoryRunIDBytes, MaxBuildIntentSummaryBytes, MaxBuildIntentCriteria, MaxBuildIntentCriterionBytes, MaxPublisherExecutablePathBytes)
	}

	criteriaAtLimit := make([]string, MaxBuildIntentCriteria)
	for i := range criteriaAtLimit {
		criteriaAtLimit[i] = fmt.Sprintf("criterion-%02d", i)
	}
	tests := map[string][]byte{
		"factory run too long": mutateRequest(t, `"run_id":"factory-run-01"`, `"run_id":`+strconv.Quote(strings.Repeat("r", MaxFactoryRunIDBytes+1))),
		"summary too long":     mutateRequest(t, `"summary":"Add a guarded publication handoff"`, `"summary":`+strconv.Quote(strings.Repeat("s", MaxBuildIntentSummaryBytes+1))),
		"too many criteria":    mutateRequest(t, `"acceptance_criteria":["Publish only the exact protected candidate"]`, `"acceptance_criteria":`+jsonStringArray(append(criteriaAtLimit, "one-too-many"))),
		"criterion too long":   mutateRequest(t, `"acceptance_criteria":["Publish only the exact protected candidate"]`, `"acceptance_criteria":`+jsonStringArray([]string{strings.Repeat("c", MaxBuildIntentCriterionBytes+1)})),
		"publisher path too long": mutateRequest(t,
			`"executable_path":"/opt/no-mistakes/bin/no-mistakes"`,
			`"executable_path":`+strconv.Quote("/"+strings.Repeat("p", MaxPublisherExecutablePathBytes))),
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRequest(raw); err == nil {
				t.Fatal("ParseRequest() accepted a value beyond the v1 bound")
			}
		})
	}

	atLimit := mutateRequest(t, `"acceptance_criteria":["Publish only the exact protected candidate"]`, `"acceptance_criteria":`+jsonStringArray(criteriaAtLimit))
	atLimit = mutateBytes(t, atLimit, `"summary":"Add a guarded publication handoff"`, `"summary":`+strconv.Quote(strings.Repeat("s", MaxBuildIntentSummaryBytes)))
	atLimit = mutateBytes(t, atLimit, `"run_id":"factory-run-01"`, `"run_id":`+strconv.Quote(strings.Repeat("r", MaxFactoryRunIDBytes)))
	if _, err := ParseRequest(atLimit); err != nil {
		t.Fatalf("ParseRequest() rejected values exactly at the v1 bounds: %v", err)
	}
}

func TestParseResultAcceptsOnlyClosedCanonicalStatuses(t *testing.T) {
	statuses := []ResultStatus{
		StatusChecking,
		StatusReadyForPush,
		StatusReadyForPR,
		StatusCIObserving,
		StatusReady,
		StatusFailed,
		StatusDrift,
		StatusDenied,
		StatusEffectUnknown,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			got, err := ParseResult(validResultBytes(status))
			if err != nil {
				t.Fatalf("ParseResult() error = %v", err)
			}
			if got.Status != status || got.Protocol != ProtocolV1 || got.PublicationID != testRunStateSHA || got.RunID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || got.HeadSHA != testHeadSHA {
				t.Fatalf("ParseResult() = %+v", got)
			}
		})
	}

	tests := map[string][]byte{
		"unknown status":  validResultBytes(ResultStatus("UNKNOWN")),
		"duplicate field": []byte(strings.Replace(string(validResultBytes(StatusReady)), `{"protocol":"factory-publication-v1",`, `{"protocol":"factory-publication-v1","protocol":"factory-publication-v1",`, 1)),
		"unknown field":   []byte(strings.TrimSuffix(string(validResultBytes(StatusReady)), "}") + `,"success":true}`),
		"missing field":   []byte(strings.Replace(string(validResultBytes(StatusReady)), `,"head_sha":"`+testHeadSHA+`"`, "", 1)),
		"trailing value":  append(validResultBytes(StatusReady), []byte(`{}`)...),
		"noncanonical":    append([]byte(" "), validResultBytes(StatusReady)...),
		"bad ID":          []byte(strings.Replace(string(validResultBytes(StatusReady)), testRunStateSHA, strings.ToUpper(testRunStateSHA), 1)),
		"bad head":        []byte(strings.Replace(string(validResultBytes(StatusReady)), testHeadSHA, testHeadSHA[:39], 1)),
	}
	for name, raw := range tests {
		t.Run("reject_"+name, func(t *testing.T) {
			if _, err := ParseResult(raw); err == nil {
				t.Fatal("ParseResult() accepted a non-canonical or open result")
			}
		})
	}
}

func TestResultExitSuccessIsExactlyREADY(t *testing.T) {
	statuses := []ResultStatus{
		StatusChecking,
		StatusReadyForPush,
		StatusReadyForPR,
		StatusCIObserving,
		StatusReady,
		StatusFailed,
		StatusDrift,
		StatusDenied,
		StatusEffectUnknown,
		ResultStatus("UNKNOWN"),
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			got := (Result{Status: status}).ExitCode()
			if status == StatusReady && got != 0 {
				t.Fatalf("READY ExitCode() = %d, want 0", got)
			}
			if status != StatusReady && got == 0 {
				t.Fatalf("%s ExitCode() = 0; only READY may report success", status)
			}
		})
	}
}

func validResultBytes(status ResultStatus) []byte {
	result := Result{
		Protocol:      ProtocolV1,
		PublicationID: testRunStateSHA,
		RunID:         "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		HeadSHA:       testHeadSHA,
		Status:        status,
	}
	if status == StatusReadyForPush {
		result.Challenge = pointerChallenge(pushResultChallenge(testRunStateSHA, testHeadSHA))
	} else if status == StatusReadyForPR {
		result.Challenge = pointerChallenge(prResultChallenge(testRunStateSHA, testHeadSHA))
	}
	raw, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return raw
}

func mutateRequest(t *testing.T, old, replacement string) []byte {
	t.Helper()
	return mutateBytes(t, validRequestBytes(), old, replacement)
}

func mutateBytes(t *testing.T, raw []byte, old, replacement string) []byte {
	t.Helper()
	if strings.Count(string(raw), old) != 1 {
		t.Fatalf("test mutation target %q occurs %d times, want exactly once", old, strings.Count(string(raw), old))
	}
	return []byte(strings.Replace(string(raw), old, replacement, 1))
}

func jsonStringArray(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

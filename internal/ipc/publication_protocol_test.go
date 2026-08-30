package ipc

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPublicationMethodConstantsAreClosedAndDistinct(t *testing.T) {
	want := []string{
		"publication_handshake",
		"publication_start",
		"publication_authorize",
		"publication_status",
	}
	got := []string{
		MethodPublicationHandshake,
		MethodPublicationStart,
		MethodPublicationAuthorize,
		MethodPublicationStatus,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publication methods = %#v, want %#v", got, want)
	}
	seen := make(map[string]struct{}, len(got))
	for _, method := range got {
		if _, exists := seen[method]; exists {
			t.Fatalf("duplicate publication method %q", method)
		}
		seen[method] = struct{}{}
	}
}

func TestPublicationHandshakeWireShapeBindsExactPublisher(t *testing.T) {
	identity := PublicationIdentity{
		ExecutablePath:   "/opt/pinned/no-mistakes",
		ExecutableSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BuildSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Protocol:         "factory-publication-v1",
	}
	params := PublicationHandshakeParams{Identity: identity}
	wantParams := `{"identity":{"executable_path":"/opt/pinned/no-mistakes","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","build_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol":"factory-publication-v1"}}`
	assertPublicationWireJSON(t, params, wantParams)

	result := PublicationHandshakeResult{Identity: identity}
	wantResult := `{"identity":{"executable_path":"/opt/pinned/no-mistakes","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","build_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","protocol":"factory-publication-v1"}}`
	assertPublicationWireJSON(t, result, wantResult)
}

func TestPublicationMutationAndReadParamsPreserveCanonicalPayloadBytes(t *testing.T) {
	startPayload := json.RawMessage(`{"protocol":"factory-publication-v1","factory":{"run_id":"factory-run"}}`)
	boundPayload := json.RawMessage(`{"protocol":"factory-publication-v1","publication_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "start",
			in:   PublicationStartParams{Request: startPayload},
			want: `{"request":{"protocol":"factory-publication-v1","factory":{"run_id":"factory-run"}}}`,
		},
		{
			name: "authorize",
			in:   PublicationAuthorizeParams{Authorization: boundPayload},
			want: `{"authorization":{"protocol":"factory-publication-v1","publication_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
		{
			name: "status",
			in:   PublicationStatusParams{Query: boundPayload},
			want: `{"query":{"protocol":"factory-publication-v1","publication_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertPublicationWireJSON(t, test.in, test.want)
		})
	}
}

func assertPublicationWireJSON(t *testing.T, value any, want string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal publication wire value: %v", err)
	}
	if string(raw) != want {
		t.Fatalf("publication wire JSON = %s, want %s", raw, want)
	}
}

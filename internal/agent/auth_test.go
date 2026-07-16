package agent

import (
	"context"
	"errors"
	"testing"
)

func TestAuthorizationRequiredClassification(t *testing.T) {
	for _, text := range []string{
		"Transport channel closed, when Auth(AuthorizationRequired)",
		"provider says authentication required",
		"not authenticated; please reauthenticate",
	} {
		if !IsAuthorizationRequired(errors.New(text)) {
			t.Errorf("%q was not classified as authorization-required", text)
		}
	}
	if _, ok := authorizationError("account transition").(*AuthorizationRequiredError); !ok {
		t.Fatal("authorizationError did not return typed error")
	}
}

func TestRunWithRetryDoesNotRetryAuthorizationRequired(t *testing.T) {
	calls := 0
	_, err := runWithRetry(context.Background(), "codex", RunOpts{}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return nil, &AuthorizationRequiredError{Agent: "codex", Detail: "account rotation"}
	})
	if !IsAuthorizationRequired(err) {
		t.Fatalf("error = %v, want authorization-required", err)
	}
	if calls != 1 {
		t.Fatalf("authorization error caused %d attempts, want 1", calls)
	}
}

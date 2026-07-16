package agent

import (
	"errors"
	"strings"
)

var ErrAuthorizationRequired = errors.New("agent authorization required")

type AuthorizationRequiredError struct {
	Agent  string
	Detail string
}

func (e *AuthorizationRequiredError) Error() string {
	if e == nil || strings.TrimSpace(e.Detail) == "" {
		return ErrAuthorizationRequired.Error()
	}
	return ErrAuthorizationRequired.Error() + ": " + e.Detail
}

func (e *AuthorizationRequiredError) Unwrap() error { return ErrAuthorizationRequired }

func IsAuthorizationRequired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuthorizationRequired) {
		return true
	}
	text := strings.ToLower(err.Error())
	return authorizationRequiredText(text)
}

func authorizationRequiredText(text string) bool {
	text = strings.ToLower(text)
	for _, needle := range []string{
		"authorizationrequired", "authorization required", "auth(authorizationrequired)",
		"not authenticated", "authentication required", "reauthenticate",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func authorizationError(detail string) error {
	return &AuthorizationRequiredError{Detail: strings.TrimSpace(detail)}
}

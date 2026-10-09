package cli

import (
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

const verificationPlanPushOptionPrefix = "no-mistakes.verification-plan="

func formatVerificationPlanPushOptions(id string) []string {
	if id == "" {
		return nil
	}
	return []string{verificationPlanPushOptionPrefix + id}
}

func parseVerificationPlanPushOptions(options []string) (string, error) {
	id := ""
	for _, option := range options {
		value, ok := strings.CutPrefix(option, verificationPlanPushOptionPrefix)
		if !ok {
			continue
		}
		if id != "" {
			return "", fmt.Errorf("duplicate verification plan push option")
		}
		if _, err := ulid.ParseStrict(value); err != nil {
			return "", fmt.Errorf("invalid verification plan capture ID")
		}
		id = value
	}
	return id, nil
}

package config

import (
	"fmt"
	"strconv"
)

func validateRequiredReviewBotID(botID *string) error {
	if botID != nil {
		id, err := strconv.ParseInt(*botID, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("ci.required_review_bot_id must be a positive decimal account ID")
		}
	}
	return nil
}

func validateRequiredReviewAppID(appID *int64) error {
	if appID != nil && *appID <= 0 {
		return fmt.Errorf("ci.required_review_app_id must be a positive GitHub App ID")
	}
	return nil
}

func validateRequiredReviews(reviewers []string) error {
	seen := map[string]bool{}
	for _, reviewer := range reviewers {
		if reviewer != "coderabbit" {
			return fmt.Errorf("ci.required_reviews: unsupported reviewer %q (supported: coderabbit)", reviewer)
		}
		if seen[reviewer] {
			return fmt.Errorf("ci.required_reviews: duplicate reviewer %q", reviewer)
		}
		seen[reviewer] = true
	}
	return nil
}

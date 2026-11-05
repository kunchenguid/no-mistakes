package config

import "testing"

func TestRequiredReviewsConfiguration(t *testing.T) {
	t.Parallel()
	const on = "ci:\n  required_reviews: [coderabbit]\n"
	const pinned = "ci:\n  required_reviews: [coderabbit]\n  required_review_app_id: 987\n  required_review_bot_id: '987654321'\n"
	const off = "ci:\n  required_reviews: []\n"
	for _, tc := range []struct {
		name, global, trusted, pushed string
		want                          int
	}{
		{"default", "{}", "{}", "{}", 0}, {"global", on, "{}", "{}", 1}, {"trusted", "{}", on, off, 1},
		{"trusted opt out", on, off, on, 0}, {"pushed cannot enable", "{}", "{}", on, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.global))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := LoadRepoFromBytes([]byte(tc.pushed))
			if err != nil {
				t.Fatal(err)
			}
			for _, optin := range []bool{false, true} {
				got := Merge(global, EffectiveRepoConfig(pushed, trusted, optin)).CI.RequiredReviews
				if len(got) != tc.want {
					t.Fatalf("optin=%v got %v, want %d requirements", optin, got, tc.want)
				}
			}
		})
	}
	global, _ := LoadGlobalFromBytes([]byte("{}"))
	trusted, _ := LoadRepoFromBytes([]byte(pinned))
	pushed, _ := LoadRepoFromBytes([]byte("{}"))
	effective := Merge(global, EffectiveRepoConfig(pushed, trusted, false)).CI
	if effective.RequiredReviewAppID != 987 || effective.RequiredReviewBotID != "987654321" {
		t.Fatalf("trusted review identity=%d/%q", effective.RequiredReviewAppID, effective.RequiredReviewBotID)
	}
	for _, bad := range []string{"[other]", "[coderabbit, coderabbit]", "[CodeRabbit]", "[123]", "coderabbit"} {
		data := []byte("ci:\n  required_reviews: " + bad + "\n")
		if _, err := LoadGlobalFromBytes(data); err == nil {
			t.Errorf("global accepted %s", bad)
		}
		if _, err := LoadRepoFromBytes(data); err == nil {
			t.Errorf("repo accepted %s", bad)
		}
	}
	for _, invalidID := range []string{"''", "0", "-1", "azure-user-guid", "1.2"} {
		if _, err := LoadRepoFromBytes([]byte("ci:\n  required_review_bot_id: " + invalidID + "\n")); err == nil {
			t.Errorf("repo accepted bot ID %s", invalidID)
		}
	}
	for _, invalidID := range []string{"0", "-1"} {
		data := []byte("ci:\n  required_review_app_id: " + invalidID + "\n")
		if _, err := LoadRepoFromBytes(data); err == nil {
			t.Errorf("repo accepted app ID %s", invalidID)
		}
	}
}

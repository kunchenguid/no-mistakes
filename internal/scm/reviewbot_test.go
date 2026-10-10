package scm

import "testing"

// The registry is read from two sides - the check suite's app slug on the
// pull request rollup and the comment author's login on review threads - and
// both must resolve to the same bot, or a red Greptile check would be routed
// to a decision without the comments that explain it.
func TestReviewBotRegistry_ResolvesAppAndLoginToTheSameBot(t *testing.T) {
	t.Parallel()
	byApp, ok := ReviewBotForApp("greptile-apps")
	if !ok {
		t.Fatal("greptile-apps must be a registered review bot app")
	}
	for _, login := range []string{"greptile-apps[bot]", "greptile-apps", " Greptile-Apps[bot] "} {
		byLogin, ok := ReviewBotForLogin(ProviderGitHub, login)
		if !ok || byLogin.AppSlug != byApp.AppSlug {
			t.Fatalf("ReviewBotForLogin(%q) = %+v, %v; want the greptile-apps bot", login, byLogin, ok)
		}
		if !IsReviewBotLogin(ProviderGitHub, login) {
			t.Fatalf("IsReviewBotLogin(%q) = false, want true", login)
		}
	}
	for _, slug := range []string{"", "github-actions", "codecov"} {
		if _, ok := ReviewBotForApp(slug); ok {
			t.Fatalf("ReviewBotForApp(%q) matched a bot; an unknown or empty app identity must never be a review bot", slug)
		}
	}
	if IsReviewBotLogin(ProviderGitHub, "octocat") {
		t.Fatal("a human login must never be a review bot")
	}
}

func TestReviewBotLoginsAreProviderScoped(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		login    string
		want     bool
	}{
		{ProviderGitHub, "greptile-apps", true},
		{ProviderGitHub, "greptile-apps[bot]", true},
		{ProviderGitHub, "greptileai", false},
		{ProviderGitLab, "greptileai", true},
		{ProviderGitLab, " GreptileAI ", true},
		{ProviderGitLab, "greptile-apps", false},
		{ProviderGitLab, "greptile-apps[bot]", false},
		{ProviderGitLab, "octocat", false},
		{ProviderGitea, "greptileai", false},
	} {
		bot, ok := ReviewBotForLogin(tc.provider, tc.login)
		if ok != tc.want || IsReviewBotLogin(tc.provider, tc.login) != tc.want || ok && bot.AppSlug != "greptile-apps" {
			t.Fatalf("provider %s login %q = %+v, %v; want match %v", tc.provider, tc.login, bot, ok, tc.want)
		}
	}
}

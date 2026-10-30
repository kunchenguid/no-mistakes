package scm

import "testing"

// The registry is read from two sides - the check suite's app slug on the
// pull request rollup and the comment author's login on review threads - and
// both must resolve to the same bot, or a red bot check would be routed to a
// decision without the comments that explain it.
func TestReviewBotRegistry_ResolvesAppAndLoginToTheSameBot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		slug   string
		logins []string
	}{
		{"greptile-apps", []string{"greptile-apps[bot]", "greptile-apps", " Greptile-Apps[bot] "}},
		{"cubic-dev-ai", []string{"cubic-dev-ai[bot]", "cubic-dev-ai", " Cubic-Dev-AI[bot] "}},
	} {
		byApp, ok := ReviewBotForCheck(ReviewBotsFor(nil), Check{App: tc.slug})
		if !ok {
			t.Fatalf("%s must be a registered review bot app", tc.slug)
		}
		for _, login := range tc.logins {
			byLogin, ok := ReviewBotForLogin(ProviderGitHub, login)
			if !ok || byLogin.AppSlug != byApp.AppSlug {
				t.Fatalf("ReviewBotForLogin(%q) = %+v, %v; want the %s bot", login, byLogin, ok, tc.slug)
			}
			if !IsReviewBotLogin(ProviderGitHub, login) {
				t.Fatalf("IsReviewBotLogin(%q) = false, want true", login)
			}
		}
	}
	for _, slug := range []string{"", "github-actions", "codecov"} {
		if _, ok := ReviewBotForCheck(ReviewBotsFor(nil), Check{App: slug}); ok {
			t.Fatalf("ReviewBotForCheck(%q) matched a bot; an unknown or empty app identity must never be a review bot", slug)
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
		want     string
	}{
		{ProviderGitHub, "greptile-apps", "greptile-apps"},
		{ProviderGitHub, "greptile-apps[bot]", "greptile-apps"},
		{ProviderGitHub, "greptileai", ""},
		{ProviderGitLab, "greptileai", "greptile-apps"},
		{ProviderGitLab, " GreptileAI ", "greptile-apps"},
		{ProviderGitLab, "greptile-apps", ""},
		{ProviderGitLab, "greptile-apps[bot]", ""},
		{ProviderGitLab, "octocat", ""},
		{ProviderGitea, "greptileai", ""},
		{ProviderGitHub, "cubic-dev-ai", "cubic-dev-ai"},
		{ProviderGitHub, "cubic-dev-ai[bot]", "cubic-dev-ai"},
		{ProviderGitLab, "cubic-dev-ai", ""},
		{ProviderGitLab, "cubic-dev-ai[bot]", ""},
		{ProviderGitea, "cubic-dev-ai[bot]", ""},
	} {
		want := tc.want != ""
		bot, ok := ReviewBotForLogin(tc.provider, tc.login)
		if ok != want || IsReviewBotLogin(tc.provider, tc.login) != want || bot.AppSlug != tc.want {
			t.Fatalf("provider %s login %q = %+v, %v; want slug %q", tc.provider, tc.login, bot, ok, tc.want)
		}
	}
}

func TestCodeRabbitRegistryRequiresImmutableGitHubIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		check Check
		want  bool
	}{
		{Check{App: "coderabbitai"}, false},
		{Check{App: "coderabbitai", AppID: 1, AppHost: "github.com"}, false},
		{Check{App: "coderabbitai", AppID: CodeRabbitAppID, AppHost: "ghe.example.com"}, false},
		{Check{AppID: CodeRabbitAppID, AppHost: "github.com"}, true},
		{Check{App: "greptile-apps"}, true},
	} {
		_, ok := ReviewBotForCheck(ReviewBotsFor([]string{"coderabbit"}), tc.check)
		if ok != tc.want {
			t.Fatalf("check=%+v matched=%v want=%v", tc.check, ok, tc.want)
		}
	}
	if _, ok := ReviewBotForCheck(ReviewBotsFor(nil), Check{AppID: CodeRabbitAppID, AppHost: "github.com"}); ok {
		t.Fatal("CodeRabbit must not be a review bot unless ci.required_reviews opts in")
	}
	bot, ok := ReviewBotForLogin(ProviderGitHub, "coderabbitai[bot]")
	if !ok || bot.AppSlug != "coderabbitai" || bot.AuthorID != 136622811 || bot.AppID != 347564 {
		t.Fatalf("CodeRabbit registry = %+v, %v", bot, ok)
	}
	if bot, ok := ReviewBotForLogin(ProviderGitHub, "coderabbitai"); !ok || bot.AuthorID != CodeRabbitAuthorID {
		t.Fatal("GraphQL login must resolve to the immutable CodeRabbit identity")
	}
}

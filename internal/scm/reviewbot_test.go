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
		byLogin, ok := ReviewBotForLogin(login)
		if !ok || byLogin.AppSlug != byApp.AppSlug {
			t.Fatalf("ReviewBotForLogin(%q) = %+v, %v; want the greptile-apps bot", login, byLogin, ok)
		}
		if !IsReviewBotLogin(login) {
			t.Fatalf("IsReviewBotLogin(%q) = false, want true", login)
		}
	}
	for _, slug := range []string{"", "github-actions", "codecov"} {
		if _, ok := ReviewBotForApp(slug); ok {
			t.Fatalf("ReviewBotForApp(%q) matched a bot; an unknown or empty app identity must never be a review bot", slug)
		}
	}
	if IsReviewBotLogin("octocat") {
		t.Fatal("a human login must never be a review bot")
	}
}

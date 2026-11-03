package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestGlobalPRAppendixPrecedence(t *testing.T) {
	t.Parallel()
	modes := []string{"", PRAppendixFull, PRAppendixCollapsed, PRAppendixMinimal}
	for _, globalMode := range modes {
		for _, overrideMode := range modes {
			for _, trustedMode := range modes {
				for _, match := range []bool{false, true} {
					name := fmt.Sprintf("global=%s/override=%s/trusted=%s/match=%v", globalMode, overrideMode, trustedMode, match)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						data := fmt.Sprintf("pr:\n  appendix: %q\nrepository_overrides:\n  https://github.com/acme/widget:\n    pr:\n      appendix: %q\n", globalMode, overrideMode)
						global, err := LoadGlobalFromBytes([]byte(data))
						if err != nil {
							t.Fatal(err)
						}
						trusted, err := LoadRepoFromBytes([]byte(fmt.Sprintf("pr:\n  appendix: %q\n", trustedMode)))
						if err != nil {
							t.Fatal(err)
						}
						remote := "https://github.com/acme/other"
						if match {
							remote = "git@github.com:acme/widget.git"
						}
						want := PRAppendixFull
						if globalMode != "" {
							want = globalMode
						}
						if match && overrideMode != "" {
							want = overrideMode
						}
						if trustedMode != "" {
							want = trustedMode
						}
						for _, allow := range []bool{false, true} {
							for _, pushedMode := range modes {
								pushed := &RepoConfig{PR: PRRaw{Appendix: pushedMode}}
								effective := EffectiveRepoConfig(pushed, trusted, allow)
								got := MergeForRemote(global, effective, remote).PR.AppendixMode()
								if got != want {
									t.Fatalf("allow=%v pushed=%q: appendix=%q, want %q", allow, pushedMode, got, want)
								}
							}
						}
					})
				}
			}
		}
	}
}

func TestGlobalPRAppendixValidation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"hidden", "FULL", "none", "off", " collapsed "} {
		_, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("pr:\n  appendix: %q\n", mode)))
		if err == nil || !strings.Contains(err.Error(), "pr.appendix") {
			t.Errorf("invalid mode %q: want pr.appendix error, got %v", mode, err)
		}
	}
	for _, field := range []string{"template: template.md", "publish_intent: false", "base_branch: main", "title_format: '{{.Title}}'"} {
		if _, err := LoadGlobalFromBytes([]byte("pr:\n  " + field + "\n")); err == nil {
			t.Errorf("out-of-scope global field %q accepted", field)
		}
	}
}

func TestGlobalPRAppendixWithoutRepositoryConfig(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"{}", "pr: {}", "pr:\n  appendix: ''", "pr:\n  appendix: collapsed"} {
		global, err := LoadGlobalFromBytes([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		want := PRAppendixFull
		if strings.Contains(data, "collapsed") {
			want = PRAppendixCollapsed
		}
		for _, allow := range []bool{false, true} {
			pushed := &RepoConfig{PR: PRRaw{Appendix: PRAppendixMinimal}}
			got := Merge(global, EffectiveRepoConfig(pushed, nil, allow)).PR.AppendixMode()
			if got != want {
				t.Errorf("data=%q allow=%v: appendix=%q, want %q", data, allow, got, want)
			}
		}
	}
}

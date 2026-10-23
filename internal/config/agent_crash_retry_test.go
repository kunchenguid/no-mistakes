package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestAgentCrashRetriesConfig(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 3, 4} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("agent_crash_retries: %d\n", n)))
			if n < 0 || n > 3 {
				if err == nil || !strings.Contains(err.Error(), "agent_crash_retries") {
					t.Fatalf("invalid budget accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := Merge(global, &RepoConfig{}).AgentCrashRetries; got != n {
				t.Fatalf("budget=%d want=%d", got, n)
			}
		})
	}
	global, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil || global.AgentCrashRetries != 0 || Merge(global, &RepoConfig{}).AgentCrashRetries != 0 {
		t.Fatalf("unconfigured retry enabled: %+v, %v", global, err)
	}
	repo, err := LoadRepoFromBytes([]byte("agent_crash_retries: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(global, repo).AgentCrashRetries != 0 {
		t.Fatal("repository enabled operator-owned crash retry budget")
	}
}

func TestAgentCrashRetryExplicitTimeouts(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value string
	}{
		{"agent_timeout", "31m"},
		{"review_agent_timeout", "31m"},
		{"test_agent_timeout", "31m"},
		{"agent_working_timeout", "30m"},
		{"review_agent_working_timeout", "30m"},
		{"test_agent_working_timeout", "30m"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.key + ": " + tc.value + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !global.AgentTimeoutsConfigured || !Merge(global, &RepoConfig{}).AgentTimeoutsConfigured {
				t.Fatal("explicit timeout not retained through merge")
			}
		})
	}
	global, err := LoadGlobalFromBytes([]byte("agent_timeout: 30m\nreview_agent_timeout: 30m\ntest_agent_timeout: 30m\n"))
	if err != nil || global.AgentTimeoutsConfigured {
		t.Fatalf("shipped timeout values marked explicit: %+v, %v", global, err)
	}
}

func TestAgentCrashRetriesWithShippedDefaults(t *testing.T) {
	data := strings.Replace(defaultConfigYAML, "agent_crash_retries: 0", "agent_crash_retries: 1", 1)
	global, err := LoadGlobalFromBytes([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if global.AgentCrashRetries != 1 || global.AgentTimeoutsConfigured {
		t.Fatalf("shipped defaults disabled default-budget retries: retries=%d configured=%t", global.AgentCrashRetries, global.AgentTimeoutsConfigured)
	}
}

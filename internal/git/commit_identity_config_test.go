package git

import (
	"strings"
	"testing"
)

func TestCommitIdentityFiltersCommandScopeConfig(t *testing.T) {
	identity := CommitIdentity{}
	base := []string{
		"GIT_AUTHOR_NAME=Daemon", "GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=AUTHOR.name", "GIT_CONFIG_VALUE_0=Daemon",
		"GIT_CONFIG_KEY_1=user.email", "GIT_CONFIG_VALUE_1=daemon@example.test",
		"GIT_CONFIG_KEY_2=test.value", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=user.signingKey", "GIT_CONFIG_VALUE_3=keep",
		"GIT_CONFIG_PARAMETERS='committer.email=daemon@example.test' 'test.parameter'='a b=c' 'test.boolean'=",
	}
	overlay, err := identity.SubprocessEnvironment(base)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, entry := range overlay.Apply(base) {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "test.value", "GIT_CONFIG_VALUE_0": "",
		"GIT_CONFIG_KEY_1": "user.signingKey", "GIT_CONFIG_VALUE_1": "keep",
		"GIT_CONFIG_PARAMETERS": "'test.parameter'='a b=c' 'test.boolean'=",
	} {
		if value, exists := got[key]; !exists || value != want {
			t.Errorf("%s = %q, present=%v, want %q", key, value, exists, want)
		}
	}
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2", "GIT_CONFIG_KEY_3", "GIT_CONFIG_VALUE_3"} {
		if _, exists := got[key]; exists {
			t.Errorf("stale override %s survived", key)
		}
	}
}

func TestCommitIdentityParameterQuoting(t *testing.T) {
	for _, raw := range []string{
		"'author.name=Daemon' 'test.value=O'\\''Brien'",
		"'author.name'='Daemon'\t'test.value=O'\\''Brien'",
	} {
		got, err := filterIdentityParameters(raw)
		if err != nil || got != "'test.value=O'\\''Brien'" {
			t.Errorf("filter = %q, %v", got, err)
		}
	}
	for _, raw := range []string{"'test.value=escaped'\\!'bang'", "'test.value'='a=b'", "'test.boolean'=", "'test.empty'=''", "'test.value=two\nlines'"} {
		if got, err := filterIdentityParameters(raw); err != nil || got != raw {
			t.Errorf("unrelated parameter changed: %q, %v", got, err)
		}
	}
}

func TestCommitIdentityRefusesMalformedCommandScopeConfig(t *testing.T) {
	for _, base := range [][]string{
		{"GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=-1"},
		{"GIT_CONFIG_COUNT=2147483648"},
		{"GIT_CONFIG_COUNT=1x"},
		{"GIT_CONFIG_PARAMETERS=unquoted=value"},
		{"GIT_CONFIG_PARAMETERS= 'test.value=leading space'"},
		{"GIT_CONFIG_PARAMETERS='author.name'='unterminated"},
		{"GIT_CONFIG_PARAMETERS='test.value=one''test.value=two'"},
	} {
		if _, err := (CommitIdentity{}).SubprocessEnvironment(base); err == nil {
			t.Error("malformed inherited Git config was accepted")
		}
	}
}

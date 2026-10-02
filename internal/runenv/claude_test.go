package runenv

import "testing"

func TestCallerClaudeConfigDir(t *testing.T) {
	for _, tc := range []struct {
		env, want string
		refused   bool
	}{
		{env: "", want: ""},
		{env: "  ", want: ""},
		{env: "/caller/.claude1", want: "/caller/.claude1"},
		{env: " /caller/.claude1/ ", want: "/caller/.claude1/"},
		{env: ".claude1", refused: true},
		{env: "~/.claude1", refused: true},
	} {
		t.Setenv(ClaudeConfigDirEnvVar, tc.env)
		got, err := CallerClaudeConfigDir()
		if (err != nil) != tc.refused || got != tc.want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: got %q, %v; want %q, refused=%v", tc.env, got, err, tc.want, tc.refused)
		}
	}
}

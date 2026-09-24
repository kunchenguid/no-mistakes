package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The jev.review_assist pre-brief and its jev.candidate_excerpt_bytes option
// were removed after the offline trial showed the candidate generator excludes
// changed files by construction while nearly all recorded finding locations
// are changed files, so the listing cannot reach what it ranks for. The keys
// are gone from the resolved config and the documentation; the raw decoder
// names them only so a global config that still sets one keeps parsing, with a
// deprecation warning and no effect.

// TestRetiredJevKeysParseWithoutEffect pins the retirement contract: a global
// config carrying either removed key loads cleanly and merges exactly like a
// config without the block - there is no Jev setting left on GlobalConfig or
// the merged Config to set.
func TestRetiredJevKeysParseWithoutEffect(t *testing.T) {
	withKeys, err := LoadGlobalFromBytes([]byte("log_level: info\njev:\n  review_assist: true\n  candidate_excerpt_bytes: 1024\n"))
	if err != nil {
		t.Fatalf("global config with retired jev keys must still parse: %v", err)
	}
	withoutKeys, err := LoadGlobalFromBytes([]byte("log_level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	if withKeys.LogLevel != withoutKeys.LogLevel || withKeys.Eval != withoutKeys.Eval || withKeys.SessionReuse != withoutKeys.SessionReuse || withKeys.Agent != withoutKeys.Agent {
		t.Fatalf("retired jev block changed the parsed config: with = %#v, without = %#v", withKeys, withoutKeys)
	}
	mergedWith := Merge(withKeys, &RepoConfig{})
	mergedWithout := Merge(withoutKeys, &RepoConfig{})
	if mergedWith.Eval != mergedWithout.Eval || mergedWith.LogLevel != mergedWithout.LogLevel || mergedWith.Agent != mergedWithout.Agent {
		t.Fatalf("retired jev block changed the merged config: with = %#v, without = %#v", mergedWith, mergedWithout)
	}
}

// TestRetiredJevKeysWarnAtLoad pins that an operator whose config still sets a
// retired key is told it has no effect, once per set key, on the load-time
// diagnostic channel.
func TestRetiredJevKeysWarnAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{"both", "jev:\n  review_assist: true\n  candidate_excerpt_bytes: 1024\n", []string{"jev.review_assist", "jev.candidate_excerpt_bytes"}},
		{"review_assist only", "jev:\n  review_assist: false\n", []string{"jev.review_assist"}},
		{"absent", "log_level: info\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			if _, err := LoadGlobalFromBytes([]byte(tc.yaml)); err != nil {
				t.Fatalf("load: %v", err)
			}
			logged := buf.String()
			if got, want := strings.Count(logged, "deprecated"), len(tc.want); got != want {
				t.Fatalf("deprecation lines = %d, want %d: %s", got, want, logged)
			}
			for _, key := range tc.want {
				if !strings.Contains(logged, key) {
					t.Fatalf("missing deprecation for %s: %s", key, logged)
				}
				if !strings.Contains(logged, "no effect") {
					t.Fatalf("deprecation does not state the setting has no effect: %s", logged)
				}
			}
		})
	}
}

// TestUnknownJevSubkeyFails pins that the tombstone tolerates exactly the two
// retired keys: anything else under jev: is rejected like any unknown field.
func TestUnknownJevSubkeyFails(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("jev:\n  something_else: true\n"))
	if err == nil || !strings.Contains(err.Error(), "not found in type") {
		t.Fatalf("unknown jev subkey err = %v, want a known-fields rejection", err)
	}
}

// TestEmptyJevBlockStillParses covers `jev:` with nothing under it, which must
// behave exactly like an absent key.
func TestEmptyJevBlockStillParses(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("jev:\nlog_level: info\n")); err != nil {
		t.Fatalf("empty jev block must parse: %v", err)
	}
}

// TestUnknownTopLevelKeyStillFails pins that the tombstone did not weaken the
// strict decoder anywhere else: a genuinely unknown top-level key is still
// rejected, so the retired block is tolerated by name, not by turning off
// known-fields checking.
func TestUnknownTopLevelKeyStillFails(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("not_a_real_key: true\n"))
	if err == nil || !strings.Contains(err.Error(), "not found in type") {
		t.Fatalf("unknown top-level key err = %v, want a known-fields rejection", err)
	}
}

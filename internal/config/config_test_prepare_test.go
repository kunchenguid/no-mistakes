package config

import "testing"

func TestTestPrepareTrustAndMerge(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, trustedValue := range []bool{false, true} {
			pushed, err := LoadRepoFromBytes([]byte("commands:\n  prepare: pushed-setup\ntest:\n  prepare: true\n"))
			if err != nil {
				t.Fatal(err)
			}
			trusted := &RepoConfig{Commands: Commands{Prepare: "trusted-setup"}, Test: TestRaw{Prepare: trustedValue}}
			for _, copy := range []*RepoConfig{trusted, nil} {
				effective := EffectiveRepoConfig(pushed, copy, allow)
				got := Merge(&GlobalConfig{Test: TestRaw{Prepare: true}}, effective)
				if want := copy != nil && trustedValue; got.Test.Prepare != want {
					t.Fatalf("allow=%v trusted=%v: prepare=%v, want %v", allow, copy, got.Test.Prepare, want)
				}
				wantCommand := ""
				if allow {
					wantCommand = "pushed-setup"
				} else if copy != nil {
					wantCommand = "trusted-setup"
				}
				if got.Commands.Prepare != wantCommand || got.Commands.Test != "" {
					t.Fatalf("command selection changed: %+v", got.Commands)
				}
			}
			if !pushed.Test.Prepare {
				t.Fatal("mutated pushed config")
			}
		}
	}
	parsed, err := LoadRepoFromBytes([]byte("test:\n  prepare: true\n"))
	if err != nil || !Merge(&GlobalConfig{}, parsed).Test.Prepare {
		t.Fatalf("opt-in did not parse/merge: %v", err)
	}
	if Merge(&GlobalConfig{}, &RepoConfig{}).Test.Prepare {
		t.Fatal("preparation must default off")
	}
}

package config

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestCommandOverrides_KeepTrustedCommandsAndMatchOnlyTheRemote(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/acme/widget.git:
    commands:
      test:
        additional: ['machine-test --all']
        env:
          GOMAXPROCS: '2'
          PATH: '/opt/toolchain/bin:/usr/bin:/bin'
`))
	if err != nil {
		t.Fatal(err)
	}
	trusted := &RepoConfig{Commands: Commands{Test: "team-test --all", Lint: "team-lint"}}
	pushed := &RepoConfig{Commands: Commands{Test: "exit 0"}}
	effective := EffectiveRepoConfig(pushed, trusted, false)
	got := MergeForRemote(global, effective, "git@github.com:ACME/Widget.git")
	if got.Commands != trusted.Commands {
		t.Fatalf("team commands changed: %+v", got.Commands)
	}
	override := got.CommandOverrides["test"]
	if !reflect.DeepEqual(override.Additional, []string{"machine-test --all"}) || override.Env["GOMAXPROCS"] != "2" || override.Env["PATH"] != "/opt/toolchain/bin:/usr/bin:/bin" {
		t.Fatalf("local override = %+v", override)
	}
	unmatched := MergeForRemote(global, effective, "git@github.com:acme/other.git")
	if !reflect.DeepEqual(unmatched, Merge(global, effective)) {
		t.Fatal("unmatched override changed legacy merge behavior")
	}
	override.Env["GOMAXPROCS"] = "99"
	override.Additional[0] = "changed"
	again := MergeForRemote(global, effective, "git@github.com:acme/widget.git")
	if again.CommandOverrides["test"].Env["GOMAXPROCS"] != "2" || again.CommandOverrides["test"].Additional[0] != "machine-test --all" {
		t.Fatal("effective override aliases mutable global configuration")
	}
}

func TestCommandOverrides_NoOverrideIsTheExistingMerge(t *testing.T) {
	global := DefaultGlobalConfig()
	repo := &RepoConfig{Commands: Commands{Prepare: "setup", Test: "tests", Lint: "lint", Format: "format"}}
	if got := MergeForRemote(global, repo, "git@github.com:acme/widget.git"); !reflect.DeepEqual(got, Merge(global, repo)) || got.CommandOverrides != nil {
		t.Fatalf("no override changed merge behavior: %+v", got)
	}
}

func TestCommandOverrides_RejectWeakeningAndMalformedExecutionSettings(t *testing.T) {
	for _, value := range []string{
		"test: 'exit 0'",
		"test: {command: 'exit 0'}",
		"test: {replace: 'exit 0'}",
		"test: {skip: true}",
		"review: {additional: ['exit 0']}",
		"prepare: {additional: ['exit 0']}",
		"format: {additional: ['exit 0']}",
		"test: {nice: -1}",
		"test: {nice: 20}",
		"test: {additional: ['   ']}",
		"test: {additional: [\"echo\\0bad\"]}",
		"test: {env: {'BAD=KEY': value}}",
		"test: {env: {'9BAD': value}}",
		"test: {env: {'PATH': one, 'path': two}}",
		"test: {env: {'OK': \"value\\0bad\"}}",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget:\n    commands:\n      " + value + "\n"))
			if err == nil {
				t.Fatal("accepted a replacement, skip, or invalid execution context")
			}
		})
	}
}

func TestCommandOverrides_NiceIsOptInAndPlatformChecked(t *testing.T) {
	data := []byte("repository_overrides:\n  https://github.com/acme/widget:\n    commands:\n      test: {nice: 10}\n")
	global, err := LoadGlobalFromBytes(data)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "Windows") {
			t.Fatalf("Windows nice = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := MergeForRemote(global, &RepoConfig{}, "git@github.com:acme/widget.git").CommandOverrides["test"].Nice; got != 10 {
		t.Fatalf("niceness = %d", got)
	}
}

func TestCommandOverrides_PushedConfigCannotSupplyLocalOverrides(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte(`commands:
  test: exit 0
repository_overrides:
  https://github.com/acme/widget:
    commands:
      test:
        env: {PATH: /untrusted/bin}
        additional: [untrusted-command]
`))
	if err != nil {
		t.Fatal(err)
	}
	trusted := &RepoConfig{Commands: Commands{Test: "team-test"}}
	cfg := MergeForRemote(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, trusted, false), "https://github.com/acme/widget")
	if len(cfg.CommandOverrides) != 0 || cfg.Commands != trusted.Commands {
		t.Fatalf("pushed branch supplied local execution settings: %+v", cfg)
	}
}

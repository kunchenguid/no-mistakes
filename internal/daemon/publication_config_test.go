package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type exactPublicationConfigFixture struct {
	repo    *db.Repo
	paths   *paths.Paths
	raw     []byte
	baseSHA string
	headSHA string
	treeSHA string
}

func newExactPublicationConfigFixture(t *testing.T, trustedYAML, pushedYAML string) *exactPublicationConfigFixture {
	t.Helper()
	source := filepath.Join(t.TempDir(), "registered-checkout")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "init", "--initial-branch=main")
	gitCmd(t, source, "config", "user.email", "publication-config@example.com")
	gitCmd(t, source, "config", "user.name", "Publication Config")
	gitCmd(t, source, "config", "commit.gpgsign", "false")
	if trustedYAML != "<absent>" {
		if err := os.WriteFile(filepath.Join(source, ".no-mistakes.yaml"), []byte(trustedYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "product.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", ".")
	gitCmd(t, source, "commit", "-m", "trusted base")
	baseSHA := gitOutput(t, source, "rev-parse", "HEAD")

	gitCmd(t, source, "checkout", "-b", "feature/exact-config")
	configPath := filepath.Join(source, ".no-mistakes.yaml")
	if pushedYAML == "<absent>" {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(configPath, []byte(pushedYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "product.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "-A")
	gitCmd(t, source, "commit", "-m", "exact candidate")
	headSHA := gitOutput(t, source, "rev-parse", "HEAD")
	treeSHA := gitOutput(t, source, "rev-parse", "HEAD^{tree}")
	gitCmd(t, source, "remote", "add", "origin", "https://127.0.0.1:1/must-not-fetch.git")

	repo := &db.Repo{
		ID: "012345abcdef", WorkingPath: source,
		UpstreamURL: "https://github.com/example/project.git", DefaultBranch: "main",
	}
	request := publication.Request{
		Protocol: publication.ProtocolV1,
		Factory: publication.FactoryBinding{
			RunID: "factory-config", TerminalT10Sequence: 10,
			RunStatePrefixSHA256: strings.Repeat("1", 64), PlanBindingSHA256: strings.Repeat("2", 64),
		},
		WorkContract: publication.WorkContractBinding{Path: ".agent/work-contract.toml", SHA256: strings.Repeat("3", 64)},
		BuildIntent:  publication.BuildIntentProjection{Summary: "load exact publication config", AcceptanceCriteria: []string{"trusted bytes remain authoritative"}},
		Candidate: publication.CandidateBinding{
			RepositoryID: repo.ID, HeadRef: "refs/heads/feature/exact-config", BaseRef: "refs/heads/main",
			BaseSHA: baseSHA, CommitSHA: headSHA, TreeSHA: treeSHA,
		},
		Publisher: publication.PublisherBinding{
			ExecutablePath: "/opt/pinned/no-mistakes", ExecutableSHA256: strings.Repeat("4", 64),
			BuildSHA: strings.Repeat("5", 40), Protocol: publication.ProtocolV1,
		},
		Scopes: publication.PublicationScopes{
			Push: publication.PushScope{Mode: publication.PushModeExactCommit, RemoteIdentity: "github.com/example/project", DestinationRef: "refs/heads/feature/exact-config"},
			PR:   publication.PRScope{Mode: publication.PRModeCreateOrUpdateExactHead, BaseRef: "refs/heads/main", HeadRef: "refs/heads/feature/exact-config"},
			CI:   publication.CIScope{Mode: publication.CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publication.ParseRequest(raw); err != nil {
		t.Fatalf("fixture request: %v", err)
	}
	p := paths.WithRoot(filepath.Join(t.TempDir(), "state"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return &exactPublicationConfigFixture{repo: repo, paths: p, raw: raw, baseSHA: baseSHA, headSHA: headSHA, treeSHA: treeSHA}
}

func TestLoadExactPublicationConfigUsesExactCandidateAndTrustedBytesWithoutGitEffects(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t,
		"allow_repo_commands: false\ncommands:\n  lint: trusted-lint\ndocument:\n  instructions: trusted-docs\nignore_patterns:\n  - trusted-only\n",
		"allow_repo_commands: true\ncommands:\n  lint: pushed-lint\ndocument:\n  instructions: pushed-docs\nignore_patterns:\n  - pushed-only\n",
	)
	global := config.DefaultGlobalConfig()
	global.Agent = types.AgentCodex
	if err := os.WriteFile(fixture.paths.ConfigFile(), []byte("{{malformed startup file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo.WorkingPath, ".no-mistakes.yaml"), []byte("{{malformed worktree copy"), 0o644); err != nil {
		t.Fatal(err)
	}

	beforeRefs := gitOutput(t, fixture.repo.WorkingPath, "for-each-ref", "--format=%(refname) %(objectname)")
	beforeConfig := gitOutput(t, fixture.repo.WorkingPath, "config", "--local", "--null", "--list")
	beforeStatus := gitOutput(t, fixture.repo.WorkingPath, "status", "--porcelain=v1", "--untracked-files=all")

	loaded, err := loadExactPublicationConfig(context.Background(), fixture.paths, global, fixture.repo, fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Commands.Lint != "trusted-lint" || loaded.Config.Document.Instructions != "trusted-docs" {
		t.Fatalf("trusted gate controls lost: commands=%#v document=%#v", loaded.Config.Commands, loaded.Config.Document)
	}
	if !reflect.DeepEqual(loaded.Config.IgnorePatterns, []string{"pushed-only"}) {
		t.Fatalf("non-executing pushed config = %v", loaded.Config.IgnorePatterns)
	}
	if loaded.Config.Agent != types.AgentCodex || loaded.Config.TrustedConfigSHA != fixture.baseSHA {
		t.Fatalf("injected global/base binding lost: agent=%q trusted=%q", loaded.Config.Agent, loaded.Config.TrustedConfigSHA)
	}
	if loaded.Forge != nil || !loaded.Environment.Empty() {
		t.Fatalf("unexpected forge routing: forge=%#v env=%#v", loaded.Forge, loaded.Environment)
	}
	if got := gitOutput(t, fixture.repo.WorkingPath, "for-each-ref", "--format=%(refname) %(objectname)"); got != beforeRefs {
		t.Fatalf("config load changed refs:\n%s\nwant:\n%s", got, beforeRefs)
	}
	if got := gitOutput(t, fixture.repo.WorkingPath, "config", "--local", "--null", "--list"); got != beforeConfig {
		t.Fatal("config load changed registered Git config")
	}
	if got := gitOutput(t, fixture.repo.WorkingPath, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("config load changed registered worktree: %q, want %q", got, beforeStatus)
	}
}

func TestLoadExactPublicationConfigHonorsPushedCommandsOnlyWhenTrustedBaseOptsIn(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t,
		"allow_repo_commands: true\ncommands:\n  lint: trusted-lint\n",
		"commands:\n  lint: pushed-lint\n",
	)
	loaded, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Commands.Lint != "pushed-lint" {
		t.Fatalf("trusted opt-in did not select pushed command: %q", loaded.Config.Commands.Lint)
	}
}

func TestLoadExactPublicationConfigTreatsAbsentCopiesAsEmpty(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
	loaded, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Commands != (config.Commands{}) || loaded.Config.TrustedConfigSHA != fixture.baseSHA {
		t.Fatalf("absent config projection = %#v", loaded.Config)
	}
}

func TestLoadExactPublicationConfigFailsClosedOnMalformedUnreadableOrInvalidEvidenceConfig(t *testing.T) {
	t.Run("malformed candidate YAML", func(t *testing.T) {
		fixture := newExactPublicationConfigFixture(t, "commands:\n  lint: trusted\n", "{{malformed")
		if _, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, fixture.raw); err == nil {
			t.Fatal("malformed exact-H config accepted")
		}
	})

	t.Run("malformed trusted YAML", func(t *testing.T) {
		fixture := newExactPublicationConfigFixture(t, "{{malformed", "commands:\n  lint: pushed\n")
		if _, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, fixture.raw); err == nil {
			t.Fatal("malformed exact-BaseSHA config accepted")
		}
	})

	t.Run("symlink candidate config", func(t *testing.T) {
		fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
		gitCmd(t, fixture.repo.WorkingPath, "checkout", "-B", "feature/symlink", fixture.baseSHA)
		if err := os.Symlink("product.txt", filepath.Join(fixture.repo.WorkingPath, ".no-mistakes.yaml")); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, fixture.repo.WorkingPath, "add", ".no-mistakes.yaml")
		gitCmd(t, fixture.repo.WorkingPath, "commit", "-m", "symlink config")
		head := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD")
		tree := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD^{tree}")
		var request publication.Request
		if err := json.Unmarshal(fixture.raw, &request); err != nil {
			t.Fatal(err)
		}
		request.Candidate.HeadRef = "refs/heads/feature/symlink"
		request.Candidate.CommitSHA = head
		request.Candidate.TreeSHA = tree
		request.Scopes.Push.DestinationRef = request.Candidate.HeadRef
		request.Scopes.PR.HeadRef = request.Candidate.HeadRef
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, raw); err == nil {
			t.Fatal("symlink config accepted as readable regular bytes")
		}
	})

	t.Run("invalid evidence root", func(t *testing.T) {
		fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
		global := config.DefaultGlobalConfig()
		root := filepath.Join(fixture.paths.WorktreesDir(), "attacker-selected")
		global.Test.Evidence.LocalRoot = &root
		if _, err := loadExactPublicationConfig(context.Background(), fixture.paths, global, fixture.repo, fixture.raw); err == nil {
			t.Fatal("invalid evidence root accepted")
		}
	})
}

func TestLoadExactPublicationConfigRevalidatesCandidateBaseAndTreeAfterReads(t *testing.T) {
	tests := map[string]func(*testing.T, *exactPublicationConfigFixture){
		"candidate ref drift": func(t *testing.T, fixture *exactPublicationConfigFixture) {
			gitCmd(t, fixture.repo.WorkingPath, "update-ref", "refs/heads/feature/exact-config", fixture.baseSHA)
		},
		"base ref drift": func(t *testing.T, fixture *exactPublicationConfigFixture) {
			gitCmd(t, fixture.repo.WorkingPath, "update-ref", "refs/heads/main", fixture.headSHA)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newExactPublicationConfigFixture(t, "commands:\n  lint: trusted\n", "commands:\n  lint: pushed\n")
			_, err := loadExactPublicationConfigWithHooks(
				context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, fixture.raw,
				publicationConfigLoadHooks{AfterConfigReads: func() { mutate(t, fixture) }},
			)
			if err == nil {
				t.Fatalf("loader accepted %s during exact-byte reads", name)
			}
		})
	}

	t.Run("request tree mismatch", func(t *testing.T) {
		fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
		var request publication.Request
		if err := json.Unmarshal(fixture.raw, &request); err != nil {
			t.Fatal(err)
		}
		request.Candidate.TreeSHA = strings.Repeat("f", 40)
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadExactPublicationConfig(context.Background(), fixture.paths, config.DefaultGlobalConfig(), fixture.repo, raw); err == nil {
			t.Fatal("loader accepted request tree that does not match exact H")
		}
	})
}

func TestLoadExactPublicationConfigReturnsResolvedForgeContextAndFrozenOverlay(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte("github.com:\n  user: codex-bot\n  users:\n    codex-bot: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	global := config.DefaultGlobalConfig()
	global.ForgeProfiles = config.ForgeProfiles{
		"github.com": {GHConfigDir: ghDir, ExpectedLogin: "codex-bot"},
	}
	loaded, err := loadExactPublicationConfig(context.Background(), fixture.paths, global, fixture.repo, fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Forge == nil || loaded.Forge.Host != "github.com" || loaded.Environment.Set["GH_CONFIG_DIR"] != ghDir {
		t.Fatalf("forge projection = forge %#v overlay %#v", loaded.Forge, loaded.Environment)
	}
	if !reflect.DeepEqual(loaded.Environment, loaded.Forge.Environment) {
		t.Fatal("returned overlay is not the resolved forge context overlay")
	}
}

package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type candidatePortFixture struct {
	database     *db.DB
	repo         *db.Repo
	source       string
	root         string
	publication  *db.Publication
	parsed       ParsedRequest
	contractRaw  []byte
	headSHA      string
	baseSHA      string
	treeSHA      string
	sourceRefs   string
	sourceConfig string
}

func candidateGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_CONFIG_") {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_COUNT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func candidateSHA256(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func newCandidatePortFixture(t *testing.T, suffix string) *candidatePortFixture {
	t.Helper()
	stateRoot := t.TempDir()
	database, err := db.Open(filepath.Join(stateRoot, "state.sqlite"))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	source := filepath.Join(t.TempDir(), "registered-checkout")
	os.MkdirAll(source, 0o755)
	candidateGit(t, source, "init", "-b", "main")
	candidateGit(t, source, "config", "user.email", "candidate@example.com")
	candidateGit(t, source, "config", "user.name", "Candidate Test")
	contractRaw := []byte("version = 1\n\n[work]\nsummary = \"exact protected candidate\"\n")
	if err := os.MkdirAll(filepath.Join(source, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".agent", "work-contract.toml"), contractRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "product.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, source, "add", ".")
	candidateGit(t, source, "commit", "-m", "base")
	baseSHA := candidateGit(t, source, "rev-parse", "HEAD")
	candidateGit(t, source, "checkout", "-b", "feature/exact-"+suffix)
	if err := os.WriteFile(filepath.Join(source, "product.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, source, "add", "product.txt")
	candidateGit(t, source, "commit", "-m", "candidate")
	headSHA := candidateGit(t, source, "rev-parse", "HEAD")
	treeSHA := candidateGit(t, source, "rev-parse", "HEAD^{tree}")

	repo, err := database.InsertRepo(
		source,
		"https://github.com/example/project.git",
		"main",
	)
	if err != nil {
		t.Fatalf("register repository: %v", err)
	}
	request := Request{
		Protocol: ProtocolV1,
		Factory: FactoryBinding{
			RunID:                "factory-run-" + suffix,
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: candidateSHA256([]byte("run-state-" + suffix)),
			PlanBindingSHA256:    candidateSHA256([]byte("plan-" + suffix)),
		},
		WorkContract: WorkContractBinding{
			Path:   ".agent/work-contract.toml",
			SHA256: candidateSHA256(contractRaw),
		},
		BuildIntent: BuildIntentProjection{
			Summary:            "publish exact candidate " + suffix,
			AcceptanceCriteria: []string{"candidate remains exact", "CI passes at H"},
		},
		Candidate: CandidateBinding{
			RepositoryID: repo.ID,
			HeadRef:      "refs/heads/feature/exact-" + suffix,
			BaseRef:      "refs/heads/main",
			BaseSHA:      baseSHA,
			CommitSHA:    headSHA,
			TreeSHA:      treeSHA,
		},
		Publisher: PublisherBinding{
			ExecutablePath:   "/opt/pinned/no-mistakes",
			ExecutableSHA256: candidateSHA256([]byte("publisher")),
			BuildSHA:         strings.Repeat("b", 40),
			Protocol:         ProtocolV1,
		},
		Scopes: PublicationScopes{
			Push: PushScope{Mode: PushModeExactCommit, RemoteIdentity: "github.com/example/project", DestinationRef: "refs/heads/feature/exact-" + suffix},
			PR:   PRScope{Mode: PRModeCreateOrUpdateExactHead, BaseRef: "refs/heads/main", HeadRef: "refs/heads/feature/exact-" + suffix},
			CI:   CIScope{Mode: CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse candidate request: %v", err)
	}
	publicationRow, _, created, err := database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID:    parsed.PublicationID,
		CanonicalRequest: parsed.CanonicalBytes,
		RepoID:           repo.ID,
		CandidateRef:     request.Candidate.HeadRef,
		BaseRef:          request.Candidate.BaseRef,
		BaseSHA:          request.Candidate.BaseSHA,
		HeadSHA:          headSHA,
		TreeSHA:          treeSHA,
	})
	if err != nil {
		t.Fatalf("persist publication: %v", err)
	}
	if !created {
		t.Fatal("first publication was not created")
	}

	return &candidatePortFixture{
		database:     database,
		repo:         repo,
		source:       source,
		root:         filepath.Join(stateRoot, "publication-candidates"),
		publication:  publicationRow,
		parsed:       parsed,
		contractRaw:  contractRaw,
		headSHA:      headSHA,
		baseSHA:      baseSHA,
		treeSHA:      treeSHA,
		sourceRefs:   candidateGit(t, source, "for-each-ref", "--format=%(refname) %(objectname)"),
		sourceConfig: candidateGit(t, source, "config", "--local", "--null", "--list"),
	}
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestGitCandidatePortMaterializesExactPrivateReadOnlyViewAndExternalScratch(t *testing.T) {
	fixture := newCandidatePortFixture(t, "materialize")
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatalf("new candidate port: %v", err)
	}
	view, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatalf("prepare candidate view: %v", err)
	}
	t.Cleanup(func() {
		_ = port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	})

	if !pathWithin(view.WorktreeDir, fixture.root) || view.WorktreeDir == fixture.source {
		t.Fatalf("candidate worktree %q is not private under %q", view.WorktreeDir, fixture.root)
	}
	if pathWithin(view.ScratchDir, view.WorktreeDir) || !pathWithin(view.ScratchDir, fixture.root) {
		t.Fatalf("scratch %q must be outside candidate %q but inside owned root %q", view.ScratchDir, view.WorktreeDir, fixture.root)
	}
	if !bytes.Equal(view.WorkContractRaw, fixture.contractRaw) {
		t.Fatalf("WorkContract bytes = %q, want exact committed bytes %q", view.WorkContractRaw, fixture.contractRaw)
	}
	if err := os.WriteFile(filepath.Join(view.ScratchDir, "writable"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("external scratch is not writable: %v", err)
	}
	for _, path := range []string{view.WorktreeDir, filepath.Join(view.WorktreeDir, "product.txt")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("candidate path %s is writable with mode %o", path, info.Mode().Perm())
		}
	}

	snapshot, err := port.Inspect(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatalf("inspect candidate: %v", err)
	}
	if snapshot.CommitSHA != fixture.headSHA || snapshot.TreeSHA != fixture.treeSHA ||
		!snapshot.TrackedClean || !snapshot.IndexClean || !snapshot.UntrackedClean {
		t.Fatalf("candidate snapshot lost exact clean H/tree binding: %#v", snapshot)
	}
	for name, digest := range map[string]string{"refs": snapshot.RefsSHA256, "config": snapshot.ConfigSHA256, "replace": snapshot.ReplaceRefsSHA256} {
		if !isLowerHex(digest, 64) {
			t.Errorf("%s guard digest = %q, want lowercase SHA-256", name, digest)
		}
	}
}

func TestGitCandidatePortRejectsRefTreeAndWorkContractDriftBeforeDefense(t *testing.T) {
	t.Run("registered ref moved", func(t *testing.T) {
		fixture := newCandidatePortFixture(t, "ref-moved")
		if err := os.WriteFile(filepath.Join(fixture.source, "later.txt"), []byte("later\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		candidateGit(t, fixture.source, "add", "later.txt")
		candidateGit(t, fixture.source, "commit", "-m", "later")
		port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepTest); err == nil {
			t.Fatal("candidate admission accepted a registered ref that no longer equals H")
		}
	})

	for _, testCase := range []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "tree", mutate: func(request *Request) { request.Candidate.TreeSHA = strings.Repeat("f", 40) }},
		{name: "work contract", mutate: func(request *Request) { request.WorkContract.SHA256 = strings.Repeat("f", 64) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCandidatePortFixture(t, "binding-"+strings.ReplaceAll(testCase.name, " ", "-"))
			request := fixture.parsed.Request
			refName := "other-" + strings.ReplaceAll(testCase.name, " ", "-")
			candidateGit(t, fixture.source, "branch", refName, fixture.headSHA)
			request.Candidate.HeadRef = "refs/heads/" + refName
			request.Scopes.Push.DestinationRef = request.Candidate.HeadRef
			request.Scopes.PR.HeadRef = request.Candidate.HeadRef
			testCase.mutate(&request)
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			other, _, _, err := fixture.database.CreateOrGetPublication(db.CreatePublicationInput{
				PublicationID:    parsed.PublicationID,
				CanonicalRequest: parsed.CanonicalBytes,
				RepoID:           fixture.repo.ID,
				CandidateRef:     request.Candidate.HeadRef,
				BaseRef:          request.Candidate.BaseRef,
				BaseSHA:          request.Candidate.BaseSHA,
				HeadSHA:          request.Candidate.CommitSHA,
				TreeSHA:          request.Candidate.TreeSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := port.PrepareStep(context.Background(), other.PublicationID, types.StepTest); err == nil {
				t.Fatalf("candidate admission accepted %s drift", testCase.name)
			}
		})
	}
}

func TestGitCandidatePortDefenseMutationCannotReachSourceOrNextFreshView(t *testing.T) {
	fixture := newCandidatePortFixture(t, "mutation")
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	view, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}

	// The ordinary write path must be denied by filesystem permissions.
	if err := os.WriteFile(filepath.Join(view.WorktreeDir, "product.txt"), []byte("mutated\n"), 0o644); err == nil {
		t.Fatal("technically read-only candidate accepted an ordinary tracked-file write")
	}
	// Even if an in-process adversary changes its disposable permissions, the
	// mutation must remain confined to this view and disappear on disposal.
	if err := os.Chmod(filepath.Join(view.WorktreeDir, "product.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.WorktreeDir, "product.txt"), []byte("disposable mutation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepReview); err != nil {
		t.Fatalf("dispose mutation attempt: %v", err)
	}

	if got := candidateGit(t, fixture.source, "show", "HEAD:product.txt"); got != "candidate" {
		t.Fatalf("defense mutation reached registered source: %q", got)
	}
	if got := candidateGit(t, fixture.source, "for-each-ref", "--format=%(refname) %(objectname)"); got != fixture.sourceRefs {
		t.Fatalf("defense mutation changed registered refs:\n%s\nwant:\n%s", got, fixture.sourceRefs)
	}
	if got := candidateGit(t, fixture.source, "config", "--local", "--null", "--list"); got != fixture.sourceConfig {
		t.Fatal("defense mutation changed registered repository config")
	}

	fresh, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatalf("prepare fresh candidate after disposal: %v", err)
	}
	t.Cleanup(func() {
		_ = port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	})
	if got := candidateGit(t, fresh.WorktreeDir, "show", "HEAD:product.txt"); got != "candidate" {
		t.Fatalf("mutation persisted into fresh candidate view: %q", got)
	}
	snapshot, err := port.Inspect(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CommitSHA != fixture.headSHA || snapshot.TreeSHA != fixture.treeSHA || !snapshot.TrackedClean || !snapshot.IndexClean || !snapshot.UntrackedClean {
		t.Fatalf("fresh candidate is not exact and clean: %#v", snapshot)
	}
}

func TestGitCandidatePortKeepsUncertainCleanupEvidenceUntilCertainDisposal(t *testing.T) {
	fixture := newCandidatePortFixture(t, "uncertain-cleanup")
	port, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	view, err := port.PrepareStep(context.Background(), fixture.publication.PublicationID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	container := filepath.Dir(view.WorktreeDir)
	if _, err := os.Lstat(container); err != nil {
		t.Fatalf("inspect prepared evidence container: %v", err)
	}

	// An uncertain process teardown deliberately makes no DisposeStep call. A
	// fresh port construction must not sweep that evidence merely because the
	// daemon or composition was recreated.
	if _, err := NewGitCandidatePort(GitCandidatePortOptions{DB: fixture.database, Root: fixture.root}); err != nil {
		t.Fatalf("reopen candidate root with retained evidence: %v", err)
	}
	if _, err := os.Lstat(container); err != nil {
		t.Fatalf("uncertain-cleanup evidence was removed without a certain boundary return: %v", err)
	}
	if _, err := port.Inspect(context.Background(), fixture.publication.PublicationID, types.StepReview); err != nil {
		t.Fatalf("retained candidate is not inspectable for recovery: %v", err)
	}

	if err := port.DisposeStep(context.Background(), fixture.publication.PublicationID, types.StepReview); err != nil {
		t.Fatalf("dispose after certain recovery: %v", err)
	}
	if _, err := os.Lstat(container); !os.IsNotExist(err) {
		t.Fatalf("certain disposal left candidate evidence container behind: %v", err)
	}
}

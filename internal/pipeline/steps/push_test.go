package steps

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushStep_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	t.Parallel()
	// When push retries after a prior UpdateRunHeadSHA failure, there are no
	// uncommitted changes. The step must still reconcile the DB if HeadSHA is stale.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with a stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA // intentionally wrong
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// In-memory HeadSHA must match actual HEAD
	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}

	// DB record must also be updated
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
	if dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != actualHeadSHA || dbRun.PushGeneration == nil || *dbRun.PushGeneration != 1 {
		t.Fatalf("already-up-to-date push binding = %#v", dbRun)
	}
	if dbRun.PushActive {
		t.Fatal("push-active marker remained set after successful step")
	}
}

func TestPushStep_ForceAddsInRepoEvidenceArtifacts(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", "https://github.com/example/widgets.git")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	evidenceDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir, "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imageData := testPNGBytes()
	imageHash := sha256.Sum256(imageData)
	imageHashText := fmt.Sprintf("%x", imageHash[:])
	publishedName := imageHashText[:32] + ".png"
	publishedPath := filepath.Join(evidenceDir, publishedName)
	if err := os.WriteFile(publishedPath, imageData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "unreferenced.png"), testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "evidence", "feature"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "evidence", "feature", "helper.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Checkout","path":%q,"sha256":%q,"size":%d}]}`, filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", publishedName)), imageHashText, len(imageData))
	if err := sctx.DB.SetStepFindings(testResult.ID, findings); err != nil {
		t.Fatal(err)
	}

	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	if _, err := os.Stat(filepath.Join(clone, fixedEvidenceRepoDir, generatedEvidenceDir, "feature", publishedName)); err != nil {
		t.Fatalf("expected ignored evidence artifact to be pushed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, fixedEvidenceRepoDir, generatedEvidenceDir, "feature", "unreferenced.png")); !os.IsNotExist(err) {
		t.Fatalf("ignored unreferenced evidence was pushed, stat error = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(clone, "evidence", "feature", "helper.go")); err != nil || string(data) != "package feature\n" {
		t.Fatalf("ordinary untracked file in overlapping evidence directory was omitted: data=%q err=%v", data, err)
	}
}

func TestPushStep_TargetsForkWhenConfigured(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	fork := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", parent)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", fork, "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork
	sctx.Run.Branch = "feature"

	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	forkSHA := gitCmd(t, fork, "rev-parse", "refs/heads/feature")
	if forkSHA != headSHA {
		t.Fatalf("fork branch SHA = %s, want %s", forkSHA, headSHA)
	}
	if out, err := exec.Command("git", "-C", parent, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", strings.TrimSpace(string(out)))
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.LastPushedSHA == nil || *dbRun.LastPushedSHA != headSHA || dbRun.PushTargetKind == nil || *dbRun.PushTargetKind != "fork" || dbRun.PushRef == nil || *dbRun.PushRef != "refs/heads/feature" {
		t.Fatalf("fork push binding = %#v", dbRun)
	}
	if dbRun.PushTargetFingerprint == nil || strings.Contains(*dbRun.PushTargetFingerprint, fork) {
		t.Fatalf("push target fingerprint persisted a URL: %#v", dbRun.PushTargetFingerprint)
	}
}

func TestPushStep_RedactsForkURLInGitErrors(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-remote-error")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/parent/project.git"
	sctx.Repo.ForkURL = "https://user:secret@example.com/fork/project.git"
	sctx.Run.Branch = "refs/heads/feature"

	step := &PushStep{}
	_, err = step.Execute(sctx)
	if err == nil {
		t.Fatal("expected push error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected error to redact fork credentials, got %v", err)
	}
	if !strings.Contains(err.Error(), "https://redacted@example.com/fork/project.git") {
		t.Fatalf("expected redacted fork URL in error, got %v", err)
	}
}

func TestPushStep_DoesNotForceAddIgnoredEvidenceDirectory(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("evidence/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore evidence")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	evidenceDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir, "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "stale.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	step := &PushStep{}
	if err := step.stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("ignored evidence directory was staged: %q", status)
	}
}

func TestPushStep_EvidenceStagingPreservesOverlappingSourceChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	sourceDir := filepath.Join(dir, "src", "feature")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "handler.go")
	if err := os.WriteFile(sourcePath, []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "src/feature/handler.go")
	gitCmd(t, dir, "commit", "-m", "add handler")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(sourcePath, []byte("package feature\n\nconst fixed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "src/feature/handler.go")

	imageData := testPNGBytes()
	imageHash := sha256.Sum256(imageData)
	hash := fmt.Sprintf("%x", imageHash[:])
	imageRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(imageRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(imageRel)), imageData, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "src"}
	setTestEvidenceManifest(t, sctx, imageRel, hash, int64(len(imageData)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Fields(gitCmd(t, dir, "diff", "--cached", "--name-only"))
	if !slices.Contains(staged, "src/feature/handler.go") {
		t.Fatalf("overlapping source change was unstaged: %v", staged)
	}
	if !slices.Contains(staged, imageRel) {
		t.Fatalf("manifest evidence was not staged: %v", staged)
	}
}

func TestPushStep_AgentStagingPreservesUntrackedFilesInOverlappingEvidenceDirectory(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	destinationDir := filepath.Join(dir, "src", "feature")
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceRel := filepath.ToSlash(filepath.Join("src", "feature", "new_handler.go"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	imageData := testPNGBytes()
	imageHash := sha256.Sum256(imageData)
	hash := fmt.Sprintf("%x", imageHash[:])
	imageRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(imageRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(imageRel)), imageData, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "src"}
	setTestEvidenceManifest(t, sctx, imageRel, hash, int64(len(imageData)))

	step := &PushStep{}
	if err := step.stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Fields(gitCmd(t, dir, "diff", "--cached", "--name-only"))
	if !slices.Contains(staged, sourceRel) {
		t.Fatalf("untracked overlapping source file was not staged: %v", staged)
	}
	if slices.Contains(staged, imageRel) {
		t.Fatalf("manifest evidence was staged as an ordinary source file: %v", staged)
	}
}

func TestPushStep_AgentStagingReservesManagedSymlinkDestination(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "unrelated.png")
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if slices.Contains(staged, rel) {
		t.Fatalf("managed symlink destination was staged as source: %v", staged)
	}
}

func TestPushStep_AgentStagingReservesManagedDirectorySubtree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	descendantRel := filepath.ToSlash(filepath.Join(rel, "payload.txt"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(descendantRel)), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if slices.Contains(staged, descendantRel) {
		t.Fatalf("managed directory descendant was staged as source: %v", staged)
	}
}

func TestPushStep_FinalStagingExcludesEvidenceManagedByPriorRounds(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	evidenceDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir, "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleData := coloredPNGBytes(41)
	staleSum := sha256.Sum256(staleData)
	staleHash := fmt.Sprintf("%x", staleSum[:])
	staleRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", staleHash[:32]+".png"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(staleRel)), staleData, 0o644); err != nil {
		t.Fatal(err)
	}
	currentData := coloredPNGBytes(42)
	currentSum := sha256.Sum256(currentData)
	currentHash := fmt.Sprintf("%x", currentSum[:])
	currentRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", currentHash[:32]+".png"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(currentRel)), currentData, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	prior := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Stale","path":%q,"sha256":%q,"size":%d}]}`, staleRel, staleHash, len(staleData))
	if _, err := sctx.DB.InsertStepRound(testResult.ID, 1, "initial", &prior, nil, 1); err != nil {
		t.Fatal(err)
	}
	current := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Current","path":%q,"sha256":%q,"size":%d}]}`, currentRel, currentHash, len(currentData))
	if err := sctx.DB.SetStepFindings(testResult.ID, current); err != nil {
		t.Fatal(err)
	}

	step := &PushStep{}
	if err := step.stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	if err := step.stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if slices.Contains(staged, staleRel) {
		t.Fatalf("stale evidence from a prior round was staged: %v", staged)
	}
	if !slices.Contains(staged, currentRel) {
		t.Fatalf("current evidence was not staged: %v", staged)
	}
}

func TestPushStep_ReplacesOnlyPriorManifestOwnedEvidence(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	oldData := coloredPNGBytes(61)
	oldSum := sha256.Sum256(oldData)
	oldHash := fmt.Sprintf("%x", oldSum[:])
	oldRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "old", oldHash[:32]+".png"))
	manifestRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "manifest.json"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(oldRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(oldRel)), oldData, 0o644); err != nil {
		t.Fatal(err)
	}
	oldManifest := fmt.Sprintf("{\"version\":1,\"files\":[{\"path\":%q,\"sha256\":%q,\"size\":%d}]}\n", oldRel, oldHash, len(oldData))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(manifestRel)), []byte(oldManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-f", "--", filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir)))
	gitCmd(t, dir, "commit", "-m", "publish old evidence")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	currentData := coloredPNGBytes(62)
	currentSum := sha256.Sum256(currentData)
	currentHash := fmt.Sprintf("%x", currentSum[:])
	currentRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", currentHash[:32]+".png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(currentRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(currentRel)), currentData, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, baseSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true}
	setTestEvidenceManifest(t, sctx, currentRel, currentHash, int64(len(currentData)))

	step := &PushStep{}
	if err := step.stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	if err := step.stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	for _, want := range []string{oldRel, currentRel, manifestRel} {
		if !slices.Contains(staged, want) {
			t.Fatalf("manifest replacement did not stage %q: %q", want, staged)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(oldRel))); !os.IsNotExist(err) {
		t.Fatalf("obsolete manifest-owned image remains: %v", err)
	}
	manifestData, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifestRel)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestData), oldRel) || !strings.Contains(string(manifestData), currentRel) {
		t.Fatalf("unexpected replacement manifest: %s", manifestData)
	}
}

func TestPushStep_AgentStagingReservesGeneratedEvidenceAcrossRuns(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	generatedDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir)
	if err := os.MkdirAll(filepath.Join(generatedDir, "old-branch"), 0o755); err != nil {
		t.Fatal(err)
	}
	trackedData := testPNGBytes()
	trackedSum := sha256.Sum256(trackedData)
	trackedHash := fmt.Sprintf("%x", trackedSum[:])
	trackedRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "old-branch", trackedHash[:32]+".png"))
	untrackedRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "crashed-run", strings.Repeat("b", 32)+".png"))
	siblingRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, "helper.go"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(trackedRel)), trackedData, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, generatedEvidenceManifestName))
	manifest := fmt.Sprintf("{\"version\":1,\"files\":[{\"path\":%q,\"sha256\":%q,\"size\":%d}]}\n", trackedRel, trackedHash, len(trackedData))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(manifestRel)), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--", trackedRel, manifestRel)
	gitCmd(t, dir, "commit", "-m", "track old generated evidence")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(trackedRel)), coloredPNGBytes(51), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(untrackedRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(untrackedRel)), coloredPNGBytes(52), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(siblingRel)), []byte("package evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if slices.Contains(staged, trackedRel) || slices.Contains(staged, untrackedRel) {
		t.Fatalf("cross-run generated evidence was staged: %v", staged)
	}
	if !slices.Contains(staged, siblingRel) {
		t.Fatalf("unrelated evidence sibling was not staged: %v", staged)
	}
}

func TestPushStep_AgentStagingPreservesUntrackedPathBytes(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	paths := []string{" leading.go", "line\nbreak.go"}
	for _, rel := range paths {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "-z")
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(string(raw), "\x00")
	for _, rel := range paths {
		if !slices.Contains(staged, rel) {
			t.Fatalf("path %q was not staged byte-for-byte: %q", rel, raw)
		}
	}
}

func TestPushStep_AgentStagingTreatsUntrackedPathsLiterally(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	generatedRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", strings.Repeat("a", 32)+".png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(generatedRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(generatedRel)), testPNGBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	magicRel := filepath.ToSlash(filepath.Join(":(top,glob).no-mistakes", "evidence", generatedEvidenceDir, "**"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(magicRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(magicRel)), []byte("literal"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if !slices.Contains(staged, magicRel) {
		t.Fatalf("literal pathspec-magic filename was not staged: %q", staged)
	}
	if slices.Contains(staged, generatedRel) {
		t.Fatalf("pathspec magic staged reserved evidence: %q", staged)
	}
}

func TestPushStep_PublicationDisabledDoesNotReserveGeneratedNamespace(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sourceRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "source.go"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(sourceRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("package generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--", sourceRel)
	gitCmd(t, dir, "commit", "-m", "add generated namespace source")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("package generated\n\nconst Changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test.Evidence = config.Evidence{}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Split(gitCmd(t, dir, "diff", "--cached", "--name-only", "-z"), "\x00")
	if !slices.Contains(staged, sourceRel) {
		t.Fatalf("disabled publication omitted source change: %q", staged)
	}
}

func TestPushStep_FirstOptInRejectsUnownedGeneratedNamespace(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	sourceRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "source.go"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(sourceRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("package generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--", sourceRel)
	gitCmd(t, dir, "commit", "-m", "add namespace source")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("package generated\n\nconst Changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, baseSHA, config.Commands{})
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true}

	err := (&PushStep{}).stageAgentChanges(sctx)
	if err == nil || !strings.Contains(err.Error(), "not tool-owned") {
		t.Fatalf("expected namespace ownership failure, got %v", err)
	}
	if staged := gitCmd(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("ownership failure changed staging: %q", staged)
	}
	if got := gitCmd(t, dir, "diff", "--name-only"); got != sourceRel {
		t.Fatalf("ownership failure lost source modification: %q", got)
	}
}

func TestPushStep_AgentStagingBatchesLargeUntrackedTrees(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	for i := 0; i < 200; i++ {
		rel := filepath.Join("generated", fmt.Sprintf("file-%03d.txt", i))
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("generated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	logFile := filepath.Join(t.TempDir(), "git.log")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-passthrough")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_LOG", logFile)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "branch-controlled-source"}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatal(err)
	}

	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(logged), "add --"); count > 4 {
		t.Fatalf("large tree used %d per-path git add commands:\n%s", count, logged)
	}
}

func TestPushStep_RejectsDriftedPreparedEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	evidenceDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir, "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := testPNGBytes()
	sum := sha256.Sum256(original)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), coloredPNGBytes(42), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(original)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if staged := gitCmd(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("drifted evidence was staged: %q", staged)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(original)) ||
		artifact.Published || artifact.Content != unpublishedImageExplanation {
		t.Fatalf("drifted evidence lost retry identity: %#v", artifact)
	}
	if strings.Contains(*steps[len(steps)-1].FindingsJSON, `"published":true`) {
		t.Fatalf("drifted evidence remained marked published: %s", *steps[len(steps)-1].FindingsJSON)
	}
}

func TestPushStep_RejectsSymlinkedPreparedEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	evidenceDir := filepath.Join(dir, fixedEvidenceRepoDir, generatedEvidenceDir, "feature")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	source := filepath.Join(dir, "source.png")
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if staged := gitCmd(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("symlinked evidence was staged: %q", staged)
	}
}

func TestPushStep_MarksVerifiedPreparedEvidencePublished(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; artifact.Path != rel {
		t.Fatalf("verified evidence was not marked published: %#v", artifact)
	}
	if !strings.Contains(*steps[len(steps)-1].FindingsJSON, `"published":true`) {
		t.Fatalf("verified evidence manifest was not marked published: %s", *steps[len(steps)-1].FindingsJSON)
	}
}

func TestPushStep_StagesDuplicatePreparedEvidenceExactlyOnce(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"First","path":%q,"sha256":%q,"size":%d},{"kind":"screenshot","label":"Duplicate","path":%q,"sha256":%q,"size":%d}]}`, rel, hash, len(data), rel, hash, len(data))
	if err := sctx.DB.SetStepFindings(testResult.ID, findings); err != nil {
		t.Fatal(err)
	}

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Fields(gitCmd(t, dir, "diff", "--cached", "--name-only"))
	manifestRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, generatedEvidenceManifestName))
	if len(staged) != 2 || !slices.Contains(staged, rel) || !slices.Contains(staged, manifestRel) {
		t.Fatalf("duplicate evidence staging = %v, want image and tool manifest", staged)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	for i, artifact := range manifest.Artifacts {
		if !artifact.Published {
			t.Fatalf("duplicate artifact %d was not marked published: %#v", i, artifact)
		}
	}
}

func TestPushStep_RejectsEvidenceSwappedInIndexAfterAdd(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	replacement := coloredPNGBytes(73)
	replacementFile := filepath.Join(dir, "replacement.png")
	if err := os.WriteFile(replacementFile, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	replacementOID := gitCmd(t, dir, "hash-object", "-w", "--", replacementFile)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-swap-evidence-index")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_EVIDENCE_PATH", rel)
	t.Setenv("FAKE_CLI_REPLACEMENT_OID", replacementOID)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if staged := gitCmd(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("index-swapped evidence remained staged: %q", staged)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; artifact.Published || artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(data)) {
		t.Fatalf("index-swapped evidence lost retry identity: %#v", artifact)
	}
}

func TestPushStep_RejectsEvidenceMutatedByCommitHook(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.png")
	if err := os.WriteFile(replacement, coloredPNGBytes(91), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	script := fmt.Sprintf("#!/bin/sh\nobject=$(git hash-object -w -- %q)\ngit update-index --cacheinfo 100644,$object,%q\n", replacement, rel)
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if _, err := (&PushStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "verify committed test evidence") {
		t.Fatalf("Execute() error = %v, want committed evidence verification failure", err)
	}
	if remote := gitCmd(t, dir, "ls-remote", upstream, "refs/heads/feature"); remote != "" {
		t.Fatalf("mutated evidence commit was pushed: %q", remote)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; artifact.Published || artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(data)) {
		t.Fatalf("commit-hook failure lost retry identity: %#v", artifact)
	}
}

func TestPushStep_RecoversCorruptUnpushedEvidenceHEADOnRetry(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", "https://github.com/example/widgets.git")
	gitCmd(t, dir, "push", "origin", "main")

	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.png")
	if err := os.WriteFile(replacement, coloredPNGBytes(91), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	script := fmt.Sprintf("#!/bin/sh\nobject=$(git hash-object -w -- %q)\ngit update-index --cacheinfo 100644,$object,%q\n", replacement, rel)
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if _, err := (&PushStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "verify committed test evidence") {
		t.Fatalf("first Execute() error = %v, want committed evidence verification failure", err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-fix.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("retry Execute() after corrupt unpushed HEAD: %v", err)
	}
	pushed := gitCmd(t, dir, "ls-remote", upstream, "refs/heads/feature")
	if pushed == "" {
		t.Fatal("retry did not push recovered evidence commit")
	}
	pushedSHA := strings.Fields(pushed)[0]
	gotHash := gitCmd(t, dir, "rev-parse", pushedSHA+":"+rel)
	wantOID := gitCmd(t, dir, "hash-object", "--", target)
	if gotHash != wantOID {
		t.Fatalf("pushed evidence blob = %s, want %s", gotHash, wantOID)
	}
	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	if data, err := os.ReadFile(filepath.Join(clone, "agent-fix.go")); err != nil || string(data) != "package feature\n" {
		t.Fatalf("agent fix omitted after evidence recovery: data=%q err=%v", data, err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; !artifact.Published || artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(data)) {
		t.Fatalf("recovered evidence identity = %#v", artifact)
	}
}

func TestPushStep_RecoversCorruptDivergedPublishedEvidenceHEADOnRetry(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", "https://github.com/example/widgets.git")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "-u", "origin", "feature")
	publishedTip := gitCmd(t, dir, "rev-parse", "HEAD")

	// Simulate a rebase/force-push rewrite: remote tip is no longer an ancestor of HEAD.
	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "rewritten.txt"), []byte("rewritten feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rewritten feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if _, err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", publishedTip, headSHA).CombinedOutput(); err == nil {
		t.Fatal("expected diverged HEAD where published tip is not an ancestor")
	}
	if tracking := gitCmd(t, dir, "rev-parse", "refs/remotes/origin/feature"); tracking != publishedTip {
		t.Fatalf("origin/feature = %s, want published tip %s", tracking, publishedTip)
	}

	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.png")
	if err := os.WriteFile(replacement, coloredPNGBytes(91), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	script := fmt.Sprintf("#!/bin/sh\nobject=$(git hash-object -w -- %q)\ngit update-index --cacheinfo 100644,$object,%q\n", replacement, rel)
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if _, err := (&PushStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "verify committed test evidence") {
		t.Fatalf("first Execute() error = %v, want committed evidence verification failure", err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-fix.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("retry Execute() after corrupt diverged published HEAD: %v", err)
	}
	pushed := gitCmd(t, dir, "ls-remote", upstream, "refs/heads/feature")
	if pushed == "" {
		t.Fatal("retry did not push recovered evidence commit")
	}
	pushedSHA := strings.Fields(pushed)[0]
	if pushedSHA == publishedTip {
		t.Fatal("retry left the previously published tip unchanged")
	}
	gotHash := gitCmd(t, dir, "rev-parse", pushedSHA+":"+rel)
	wantOID := gitCmd(t, dir, "hash-object", "--", target)
	if gotHash != wantOID {
		t.Fatalf("pushed evidence blob = %s, want %s", gotHash, wantOID)
	}
	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	if data, err := os.ReadFile(filepath.Join(clone, "agent-fix.go")); err != nil || string(data) != "package feature\n" {
		t.Fatalf("agent fix omitted after diverged evidence recovery: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(clone, "rewritten.txt")); err != nil || string(data) != "rewritten feature\n" {
		t.Fatalf("rewritten feature content omitted after recovery: data=%q err=%v", data, err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if artifact := manifest.Artifacts[0]; !artifact.Published || artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(data)) {
		t.Fatalf("recovered diverged evidence identity = %#v", artifact)
	}
}

func TestCanRewriteUnpushedEvidenceHEAD_RefusesEqualOrBehindAllowsAheadOrDiverged(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "-u", "origin", "feature")
	publishedTip := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	if canRewriteUnpushedEvidenceHEAD(sctx, publishedTip, true) {
		t.Fatal("equal HEAD must refuse rewrite")
	}

	gitCmd(t, dir, "reset", "--hard", baseSHA)
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	if canRewriteUnpushedEvidenceHEAD(sctx, publishedTip, true) {
		t.Fatal("behind HEAD must refuse rewrite")
	}

	if err := os.WriteFile(filepath.Join(dir, "ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "ahead of published tip")
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	if canRewriteUnpushedEvidenceHEAD(sctx, publishedTip, true) != true {
		t.Fatal("diverged HEAD must allow rewrite")
	}

	// Linear ahead: publish tip is ancestor of HEAD.
	gitCmd(t, dir, "reset", "--hard", publishedTip)
	if err := os.WriteFile(filepath.Join(dir, "linear-ahead.txt"), []byte("linear\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "linear ahead")
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	if canRewriteUnpushedEvidenceHEAD(sctx, publishedTip, true) != true {
		t.Fatal("ahead HEAD must allow rewrite")
	}
	if canRewriteUnpushedEvidenceHEAD(sctx, "", false) != true {
		t.Fatal("missing remote branch must allow rewrite")
	}
}

func TestPushStep_VerifiesUnpublishedManifestArtifactInCandidateCommit(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, coloredPNGBytes(77), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--", rel)
	gitCmd(t, dir, "commit", "-m", "invalid evidence")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "branch-controlled-source"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	err := (&PushStep{}).verifyCommittedInRepoEvidence(sctx, gitCmd(t, dir, "rev-parse", "HEAD"))
	if err == nil || !strings.Contains(err.Error(), "commit does not match prepared manifest") {
		t.Fatalf("verification error = %v, want manifest mismatch", err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := types.ParseFindingsJSON(*steps[len(steps)-1].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	artifact := manifest.Artifacts[0]
	if artifact.Published || artifact.Path != rel || artifact.SHA256 != hash || artifact.Size != int64(len(data)) {
		t.Fatalf("retry identity changed: %#v", artifact)
	}
}

func TestPushStep_CommitsWhitespaceOnlyFilename(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(dir, " "), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	clone := t.TempDir()
	gitCmd(t, clone, "clone", "--branch", "feature", upstream, ".")
	if data, err := os.ReadFile(filepath.Join(clone, " ")); err != nil || string(data) != "kept" {
		t.Fatalf("whitespace filename was not pushed: data=%q err=%v", data, err)
	}
}

func TestPushStep_DisablesEvidenceForUnsupportedRemote(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	attachLocalPushOrigin(t, dir)
	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(rel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "http://github.example.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	if err := (&PushStep{}).stageInRepoEvidence(sctx); err != nil {
		t.Fatal(err)
	}
	if staged := gitCmd(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("evidence for unsupported remote was staged: %q", staged)
	}
}

func attachLocalPushOrigin(t *testing.T, dir string) string {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").CombinedOutput(); err == nil {
		url := strings.TrimSpace(string(out))
		if url != "" {
			return url
		}
	}
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "HEAD:refs/heads/main")
	return upstream
}

func setTestEvidenceManifest(t *testing.T, sctx *pipeline.StepContext, rel, hash string, size int64) {
	t.Helper()
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := fmt.Sprintf(`{"findings":[],"summary":"","artifacts":[{"kind":"screenshot","label":"Evidence","path":%q,"sha256":%q,"size":%d}]}`, rel, hash, size)
	if err := sctx.DB.SetStepFindings(testResult.ID, findings); err != nil {
		t.Fatal(err)
	}
}

func TestPushStep_FetchesAbsentRemoteTipBeforeEvidenceOwnership(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	publisher := t.TempDir()
	gitCmd(t, publisher, "init")
	gitCmd(t, publisher, "config", "user.name", "test")
	gitCmd(t, publisher, "config", "user.email", "test@test.com")
	gitCmd(t, publisher, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(publisher, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, publisher, "add", "-A")
	gitCmd(t, publisher, "commit", "-m", "initial")
	gitCmd(t, publisher, "remote", "add", "origin", upstream)
	gitCmd(t, publisher, "push", "origin", "main")

	dir := t.TempDir()
	gitCmd(t, dir, "clone", "--branch", "main", "--single-branch", upstream, ".")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "local feature")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, publisher, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(publisher, "remote-only.txt"), []byte("remote tip"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, publisher, "add", "-A")
	gitCmd(t, publisher, "commit", "-m", "remote feature tip")
	gitCmd(t, publisher, "push", "origin", "feature")
	remoteTip := gitCmd(t, publisher, "rev-parse", "HEAD")
	if _, err := exec.Command("git", "-C", dir, "cat-file", "-e", remoteTip+"^{commit}").CombinedOutput(); err == nil {
		t.Fatal("remote tip unexpectedly present locally before ownership fetch")
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true}

	if err := (&PushStep{}).stageAgentChanges(sctx); err != nil {
		t.Fatalf("stageAgentChanges with absent remote tip: %v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "cat-file", "-e", remoteTip+"^{commit}").CombinedOutput(); err != nil {
		t.Fatalf("ownership path did not fetch absent remote tip: %v", err)
	}
}

func TestPushStep_LsRemoteFailureFailsClosedForEvidenceOwnership(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sourceRel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "corrupt.png"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(sourceRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(sourceRel)), []byte("not-an-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "--", sourceRel)
	gitCmd(t, dir, "commit", "-m", "corrupt generated namespace")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-remote-error")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)

	data := testPNGBytes()
	sum := sha256.Sum256(data)
	hash := fmt.Sprintf("%x", sum[:])
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, "feature", hash[:32]+".png"))
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/example/widgets.git"
	sctx.Run.Branch = "feature"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true}
	setTestEvidenceManifest(t, sctx, rel, hash, int64(len(data)))

	err = (&PushStep{}).stageAgentChanges(sctx)
	if err == nil {
		t.Fatal("expected ls-remote failure to fail closed")
	}
	if !strings.Contains(err.Error(), "resolve pushed tip for evidence ownership") {
		t.Fatalf("ls-remote failure error = %v, want resolve pushed tip failure", err)
	}
	if strings.Contains(err.Error(), "not tool-owned at HEAD") {
		t.Fatalf("ls-remote failure incorrectly fell through to HEAD ownership rewrite: %v", err)
	}
}

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These static tests pin the macOS Developer ID code-signing contract of
// .github/workflows/release.yml. The workflow cannot be exercised end to end
// from `go test` (it signs on a GitHub macOS runner with a Developer ID cert
// that only exists in CI), so the invariant "every official macOS artifact is
// Developer ID signed with a permanent identifier, verified before archive" is
// guarded here. Against the pre-signing workflow these fail because the darwin
// legs cross-compile on ubuntu with no codesign (arm64 ad-hoc, amd64 unsigned) -
// that failing run is the reproduction. The permanent-identity invariant and the
// signing contract are owned by the "macOS Release Signing" section of AGENTS.md.

// The permanent signing identity. These MUST NEVER change once the first signed
// release ships: the executable identifier and Team ID are the invariant part of
// the Developer ID designated requirement that lets macOS permissions survive
// `no-mistakes update`.
const (
	signingIdentifier = "com.kunchenguid.no-mistakes"
	signingTeamID     = "9T2J7MNUP9"

	cscLinkSecret    = "CSC_LINK"
	cscKeyPassSecret = "CSC_KEY_PASSWORD"
	signingEnv       = "release-signing"
)

// signingDarwinArches is the set of macOS architectures every official release
// must ship as a signed thin binary.
var signingDarwinArches = []string{"amd64", "arm64"}

// expectedLipoArch maps a Go GOARCH to the name `lipo -archs` prints, which the
// signing job asserts per matrix leg.
var expectedLipoArch = map[string]string{"amd64": "x86_64", "arm64": "arm64"}

// wfDoc is a minimal typed view of the workflow, capturing only the fields the
// signing contract cares about. Flexible fields (runs-on, environment, needs)
// are decoded as any and normalized by the helpers below.
type wfDoc struct {
	Jobs map[string]*wfJob `yaml:"jobs"`
}

type wfJob struct {
	name        string
	RunsOn      any        `yaml:"runs-on"`
	Environment any        `yaml:"environment"`
	Needs       any        `yaml:"needs"`
	If          string     `yaml:"if"`
	Strategy    wfStrategy `yaml:"strategy"`
	Steps       []wfStep   `yaml:"steps"`
}

type wfStrategy struct {
	Matrix struct {
		Include []map[string]string `yaml:"include"`
	} `yaml:"matrix"`
}

type wfStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

func loadReleaseWorkflowDoc(t *testing.T) *wfDoc {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	var wf wfDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	for name, job := range wf.Jobs {
		job.name = name
	}
	return &wf
}

func wfScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if n, ok := t["name"].(string); ok {
			return n
		}
	}
	return ""
}

func wfList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (j *wfJob) runsOn() string    { return wfScalar(j.RunsOn) }
func (j *wfJob) environ() string   { return wfScalar(j.Environment) }
func (j *wfJob) needs() []string   { return wfList(j.Needs) }
func (j *wfJob) runsOnMacOS() bool { return strings.Contains(j.runsOn(), "macos") }

func (j *wfJob) allRun() string {
	var b strings.Builder
	for _, s := range j.Steps {
		b.WriteString(s.Run)
		b.WriteString("\n")
	}
	return b.String()
}

func (j *wfJob) coversDarwinArch(goarch string) bool {
	for _, e := range j.Strategy.Matrix.Include {
		if e["goos"] == "darwin" && e["goarch"] == goarch {
			return true
		}
	}
	return false
}

func (j *wfJob) coversAnyDarwin() bool {
	for _, e := range j.Strategy.Matrix.Include {
		if e["goos"] == "darwin" {
			return true
		}
	}
	return false
}

func (wf *wfDoc) darwinBuildJobs() []*wfJob {
	var out []*wfJob
	for _, job := range wf.Jobs {
		if job.coversAnyDarwin() {
			out = append(out, job)
		}
	}
	return out
}

// darwinJobForArch returns the single darwin build job covering goarch, or nil
// if none or more than one (ambiguity is itself a failure).
func (wf *wfDoc) darwinJobForArch(goarch string) *wfJob {
	var found *wfJob
	for _, job := range wf.Jobs {
		if job.coversDarwinArch(goarch) {
			if found != nil {
				return nil
			}
			found = job
		}
	}
	return found
}

func (wf *wfDoc) nonDarwinBuildJob() *wfJob {
	for _, job := range wf.Jobs {
		if job.coversAnyDarwin() {
			continue
		}
		for _, e := range job.Strategy.Matrix.Include {
			if e["goos"] == "linux" || e["goos"] == "windows" {
				return job
			}
		}
	}
	return nil
}

func (wf *wfDoc) jobByRunContains(substrs ...string) *wfJob {
	for _, job := range wf.Jobs {
		if wfContainsAll(job.allRun(), substrs...) {
			return job
		}
	}
	return nil
}

func wfStepIndex(steps []wfStep, substrs ...string) int {
	for i, s := range steps {
		if wfContainsAll(s.Run, substrs...) {
			return i
		}
	}
	return -1
}

func wfContainsAll(hay string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			return false
		}
	}
	return true
}

var wfSecretRefRe = regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`)

func (j *wfJob) secretRefs() map[string]bool {
	refs := map[string]bool{}
	add := func(s string) {
		for _, m := range wfSecretRefRe.FindAllStringSubmatch(s, -1) {
			refs[m[1]] = true
		}
	}
	add(wfScalar(j.Environment))
	for _, st := range j.Steps {
		add(st.Run)
		for k, v := range st.Env {
			add(k)
			add(v)
		}
		for k, v := range st.With {
			add(k)
			add(v)
		}
	}
	return refs
}

func signStepIndex(j *wfJob) int {
	return wfStepIndex(j.Steps, "codesign", "--sign", "--options runtime",
		"--identifier", signingIdentifier, "--timestamp")
}

func verifyStepIndex(j *wfJob) int {
	return wfStepIndex(j.Steps, "codesign --verify", "anchor apple generic")
}

func archiveStepIndex(j *wfJob) int {
	return wfStepIndex(j.Steps, "tar", "czf", ".tar.gz")
}

func uploadStepIndex(j *wfJob) int {
	return wfStepIndex(j.Steps, "gh release upload")
}

// TestReleaseWorkflowSignsDarwinArtifactsWithDeveloperID is the primary
// reproduction/regression: it fails on any workflow that can publish an ad-hoc
// or unsigned macOS artifact.
func TestReleaseWorkflowSignsDarwinArtifactsWithDeveloperID(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("darwin/%s has no single dedicated build job; macOS artifacts would ship unsigned", arch)
		}
		if !job.runsOnMacOS() {
			t.Errorf("darwin/%s build job %q runs on %q, not a macOS runner; codesign cannot run and the artifact ships ad-hoc/unsigned",
				arch, job.name, job.runsOn())
		}
		if signStepIndex(job) < 0 {
			t.Errorf("darwin/%s build job %q has no Developer ID codesign step (--sign --options runtime --identifier %s --timestamp)",
				arch, job.name, signingIdentifier)
		}
		if verifyStepIndex(job) < 0 {
			t.Errorf("darwin/%s build job %q has no post-sign verification step", arch, job.name)
		}
	}
}

// TestReleaseWorkflowSignsBothDarwinArches pins that BOTH arm64 and amd64 go
// through the signing path (the audit stressed the two arches start in different
// states) and that each leg asserts its expected architecture.
func TestReleaseWorkflowSignsBothDarwinArches(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("no single darwin build job covers goarch=%s", arch)
		}
		if !job.runsOnMacOS() {
			t.Errorf("darwin/%s job %q must run on macOS", arch, job.name)
		}
		if !strings.Contains(job.allRun(), expectedLipoArch[arch]) {
			t.Errorf("darwin/%s job %q never asserts expected architecture %q (lipo -archs)", arch, job.name, expectedLipoArch[arch])
		}
	}

	built := map[string]bool{}
	for _, job := range wf.darwinBuildJobs() {
		for _, e := range job.Strategy.Matrix.Include {
			if e["goos"] == "darwin" {
				built[e["goarch"]] = true
			}
		}
	}
	for _, arch := range signingDarwinArches {
		if !built[arch] {
			t.Errorf("darwin/%s missing from the signed build matrix", arch)
		}
	}
}

// TestReleaseWorkflowScopesSigningSecretsToDarwin enforces that the cert secrets
// are gated behind the release-signing environment and referenced only by the
// darwin signing job, and that the CI keychain password is generated at runtime
// rather than stored as a secret.
func TestReleaseWorkflowScopesSigningSecretsToDarwin(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	darwinJobs := map[string]bool{}
	for _, job := range wf.darwinBuildJobs() {
		darwinJobs[job.name] = true

		if job.environ() != signingEnv {
			t.Errorf("darwin signing job %q must be gated behind environment %q, got %q", job.name, signingEnv, job.environ())
		}
		refs := job.secretRefs()
		if !refs[cscLinkSecret] || !refs[cscKeyPassSecret] {
			t.Errorf("darwin signing job %q must reference both %s and %s", job.name, cscLinkSecret, cscKeyPassSecret)
		}
		for name := range refs {
			if name != cscLinkSecret && name != cscKeyPassSecret {
				t.Errorf("darwin signing job %q references unexpected secret %q; the keychain password must be runtime-generated, not a secret", job.name, name)
			}
		}
		if !strings.Contains(job.allRun(), "openssl rand") {
			t.Errorf("darwin signing job %q must generate the ephemeral keychain password at runtime (openssl rand)", job.name)
		}
	}

	for _, job := range wf.Jobs {
		if darwinJobs[job.name] {
			continue
		}
		refs := job.secretRefs()
		if refs[cscLinkSecret] || refs[cscKeyPassSecret] {
			t.Errorf("job %q references signing secrets but is not the darwin signing job; secrets must stay scoped", job.name)
		}
		if job.environ() == signingEnv {
			t.Errorf("job %q is gated behind %q but does not sign; the environment must scope only the signing job", job.name, signingEnv)
		}
	}
}

// TestReleaseWorkflowSignsBeforeArchiveAndChecksum enforces the load-bearing
// ordering: sign -> verify -> archive -> upload within the darwin job, and the
// checksums job depending on the darwin build job so checksums cover signed
// archives.
func TestReleaseWorkflowSignsBeforeArchiveAndChecksum(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("no darwin build job for %s", arch)
		}
		sign := signStepIndex(job)
		verify := verifyStepIndex(job)
		archive := archiveStepIndex(job)
		upload := uploadStepIndex(job)
		if sign < 0 || verify < 0 || archive < 0 || upload < 0 {
			t.Fatalf("darwin job %q missing a required step (sign=%d verify=%d archive=%d upload=%d)", job.name, sign, verify, archive, upload)
		}
		if sign >= verify {
			t.Errorf("darwin job %q: signing (step %d) must come before verification (step %d)", job.name, sign, verify)
		}
		if verify >= archive {
			t.Errorf("darwin job %q: verification (step %d) must come before archiving (step %d)", job.name, verify, archive)
		}
		if archive >= upload {
			t.Errorf("darwin job %q: archiving (step %d) must come before upload (step %d)", job.name, archive, upload)
		}
	}

	checksums := wf.jobByRunContains("sha256sum", "checksums.txt")
	if checksums == nil {
		t.Fatal("no checksums job found")
	}
	for _, arch := range signingDarwinArches {
		darwinJob := wf.darwinJobForArch(arch)
		found := false
		for _, dep := range checksums.needs() {
			if dep == darwinJob.name {
				found = true
			}
		}
		if !found {
			t.Errorf("checksums job must depend on darwin build job %q so checksums are computed over signed archives", darwinJob.name)
		}
	}
}

// TestReleaseWorkflowFailsClosedOnBadSignature verifies the gate refuses to
// publish when secrets, identity discovery, signing, or verification is missing
// or ambiguous.
func TestReleaseWorkflowFailsClosedOnBadSignature(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("no darwin build job for %s", arch)
		}
		run := job.allRun()

		if !strings.Contains(run, "set -euo pipefail") {
			t.Errorf("darwin job %q scripts must use `set -euo pipefail` to fail closed", job.name)
		}
		if !wfContainsAll(run, cscLinkSecret, cscKeyPassSecret) {
			t.Errorf("darwin job %q must check both signing secrets are present", job.name)
		}
		if !regexp.MustCompile(`(?s)-z.*CSC_LINK`).MatchString(run) ||
			!strings.Contains(run, "exit 1") {
			t.Errorf("darwin job %q must abort when CSC_LINK is empty", job.name)
		}

		// Identity discovery must fail closed on anything but exactly one
		// Developer ID Application identity.
		if !strings.Contains(run, "Developer ID Application") {
			t.Errorf("darwin job %q must discover a Developer ID Application identity", job.name)
		}
		if !regexp.MustCompile(`-ne 1|!= 1|-eq 0`).MatchString(run) {
			t.Errorf("darwin job %q must fail closed unless exactly one signing identity is found", job.name)
		}

		verifyAsserts := []string{
			"codesign --verify --strict",
			signingTeamID,
			signingIdentifier,
			"anchor apple generic",
			"subject.OU",
		}
		for _, a := range verifyAsserts {
			if !strings.Contains(run, a) {
				t.Errorf("darwin job %q verification missing assertion %q", job.name, a)
			}
		}
		if !strings.Contains(run, "adhoc") {
			t.Errorf("darwin job %q must reject an ad-hoc signature", job.name)
		}
		if !strings.Contains(run, "cdhash H") {
			t.Errorf("darwin job %q must reject a content-based (cdhash) designated requirement", job.name)
		}
		if !strings.Contains(run, "runtime") {
			t.Errorf("darwin job %q must assert the hardened runtime flag", job.name)
		}
		if !regexp.MustCompile(`(?s)TIMESTAMP=.*Timestamp=.*-z "\$TIMESTAMP"`).MatchString(run) ||
			!regexp.MustCompile(`(?i)grep -qi ['"]?\^none\$`).MatchString(run) {
			t.Errorf("darwin job %q must reject an empty or none secure timestamp", job.name)
		}
	}
}

// TestReleaseWorkflowCleansUpKeychainAlways requires the ephemeral keychain to be
// torn down on both success and failure paths.
func TestReleaseWorkflowCleansUpKeychainAlways(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("no darwin build job for %s", arch)
		}
		cleanup := -1
		for i, st := range job.Steps {
			if strings.Contains(st.Run, "delete-keychain") {
				cleanup = i
				if !strings.Contains(strings.ReplaceAll(st.If, " ", ""), "always()") {
					t.Errorf("darwin job %q keychain cleanup step must run with if: always(), got %q", job.name, st.If)
				}
			}
		}
		if cleanup < 0 {
			t.Errorf("darwin job %q has no keychain cleanup step (security delete-keychain)", job.name)
		}
	}
}

// TestReleaseWorkflowPreservesArtifactContract pins the installer/updater and
// checksum contracts: per-arch tarball names, an unchanged linux/windows path,
// and finalize still publishing a prerelease.
func TestReleaseWorkflowPreservesArtifactContract(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	for _, arch := range signingDarwinArches {
		job := wf.darwinJobForArch(arch)
		if job == nil {
			t.Fatalf("no darwin build job for %s", arch)
		}
		run := job.allRun()
		if !strings.Contains(run, "no-mistakes-") || !strings.Contains(run, "${GOOS}-${GOARCH}.tar.gz") {
			t.Errorf("darwin job %q must preserve the no-mistakes-<tag>-<goos>-<goarch>.tar.gz name", job.name)
		}
	}

	lw := wf.nonDarwinBuildJob()
	if lw == nil {
		t.Fatal("no linux/windows build job found")
	}
	if !strings.Contains(lw.runsOn(), "ubuntu") {
		t.Errorf("linux/windows job %q must stay on ubuntu, got %q", lw.name, lw.runsOn())
	}
	wantTargets := map[string]bool{
		"linux/amd64": true, "linux/arm64": true,
		"windows/amd64": true, "windows/arm64": true,
	}
	gotTargets := map[string]bool{}
	for _, e := range lw.Strategy.Matrix.Include {
		gotTargets[e["goos"]+"/"+e["goarch"]] = true
	}
	if len(gotTargets) != len(wantTargets) {
		t.Errorf("linux/windows job %q matrix = %v, want %v", lw.name, gotTargets, wantTargets)
	}
	for target := range wantTargets {
		if !gotTargets[target] {
			t.Errorf("linux/windows job %q missing target %s", lw.name, target)
		}
	}
	if strings.Contains(lw.allRun(), "codesign") {
		t.Errorf("linux/windows job %q must not sign", lw.name)
	}
	if uploadStepIndex(lw) < 0 {
		t.Errorf("linux/windows job %q must still upload the release asset", lw.name)
	}

	checksums := wf.jobByRunContains("sha256sum", "checksums.txt")
	if checksums == nil || !strings.Contains(checksums.allRun(), "sha256sum no-mistakes-*") {
		t.Error("checksums job must still compute `sha256sum no-mistakes-*`")
	}

	finalize := wf.jobByRunContains("--prerelease=true")
	if finalize == nil || !wfContainsAll(finalize.allRun(), "--draft=false", "--prerelease=true") {
		t.Error("finalize job must still run `gh release edit --draft=false --prerelease=true`")
	}
}

// TestReleaseWorkflowStaysPhase1NoNotarization enforces the Phase 1 scope:
// signing only, no notarization/stapling/pkg/homebrew/universal binaries.
func TestReleaseWorkflowStaysPhase1NoNotarization(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)

	var blob strings.Builder
	for _, job := range wf.Jobs {
		blob.WriteString(job.allRun())
		blob.WriteString("\n")
	}
	lower := strings.ToLower(blob.String())
	for _, f := range []string{
		"notarytool", "stapler", "pkgbuild", "productbuild",
		"lipo -create", "--universal", "brew ", "homebrew",
	} {
		if strings.Contains(lower, strings.ToLower(f)) {
			t.Errorf("release workflow must not add out-of-scope tooling %q in Phase 1", f)
		}
	}
}

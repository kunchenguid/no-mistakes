package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInternalRefMutationCapabilityIsExactAndOneTime(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo-1", "/tmp/repo-1", "https://example.test/repo", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	spec := InternalRefMutationSpec{
		RepoID: "repo-1", GatePath: "/tmp/repo-1.git", Branch: "feature",
		Ref: "refs/heads/feature", OldSHA: "old", NewSHA: "new",
		Operation: "reference-transaction", Scope: InternalRefMutationScopeOrdinary,
	}
	capability, err := d.IssueInternalRefMutation(spec, "test-authority")
	if err != nil {
		t.Fatalf("issue capability: %v", err)
	}

	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", spec.GatePath, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err != nil {
		t.Fatalf("prepare capability: %v", err)
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "committed", spec.GatePath, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err != nil {
		t.Fatalf("commit capability: %v", err)
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", spec.GatePath, spec.Branch, "refs/heads/other", spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err == nil {
		t.Fatal("capability replay on the wrong ref unexpectedly succeeded")
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", spec.GatePath, spec.Branch, spec.Ref, "wrong-old", spec.NewSHA, spec.Operation, spec.Scope, capability); err == nil {
		t.Fatal("capability use with the wrong old object unexpectedly succeeded")
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", spec.GatePath, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, "wrong-operation", spec.Scope, capability); err == nil {
		t.Fatal("capability use with the wrong operation unexpectedly succeeded")
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "committed", spec.GatePath, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err == nil {
		t.Fatal("committed capability replay unexpectedly succeeded")
	}
	private := spec
	private.Ref = "refs/no-mistakes/recover/run-1"
	private.Scope = InternalRefMutationScopePrivate
	privateCapability, err := d.IssueInternalRefMutation(private, "test-authority")
	if err != nil {
		t.Fatalf("issue private capability: %v", err)
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", private.GatePath, private.Branch, private.Ref, private.OldSHA, private.NewSHA, private.Operation, InternalRefMutationScopeOrdinary, privateCapability); err == nil {
		t.Fatal("private capability crossed into ordinary scope")
	}
	private.Scope = InternalRefMutationScopeOrdinary
	if _, err := d.IssueInternalRefMutation(private, "test-authority"); err == nil {
		t.Fatal("private ref accepted ordinary scope")
	}
}

func TestInternalRefMutationCanonicalizesGatePathAliases(t *testing.T) {
	d := openTestDB(t)
	realGate := t.TempDir()
	alias := filepath.Join(t.TempDir(), "gate.git")
	if err := os.Symlink(realGate, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := d.InsertRepoWithID("repo-1", filepath.Join(t.TempDir(), "working"), "https://example.test/repo", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	spec := InternalRefMutationSpec{
		RepoID: "repo-1", GatePath: realGate, Branch: "feature",
		Ref: "refs/no-mistakes/recover/run-1", OldSHA: "old", NewSHA: "new",
		Operation: "recover-anchor", Scope: InternalRefMutationScopePrivate,
	}
	capability, err := d.IssueInternalRefMutation(spec, "test-authority")
	if err != nil {
		t.Fatalf("issue capability: %v", err)
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "prepared", alias, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err != nil {
		t.Fatalf("prepare capability through alias: %v", err)
	}
	if err := d.AdvanceInternalRefMutation("test-authority", "committed", alias, spec.Branch, spec.Ref, spec.OldSHA, spec.NewSHA, spec.Operation, spec.Scope, capability); err != nil {
		t.Fatalf("commit capability through alias: %v", err)
	}
	aliasSpec := spec
	aliasSpec.GatePath = alias
	consumed, err := d.ConsumedInternalRefMutationExists(aliasSpec, "test-authority")
	if err != nil {
		t.Fatalf("find consumed capability through alias: %v", err)
	}
	if !consumed {
		t.Fatal("consumed capability through alias was not found")
	}
}

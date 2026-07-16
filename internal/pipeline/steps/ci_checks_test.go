package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestHasPendingChecksIgnoresOnlyExplicitAdvisoryLegacyStatus(t *testing.T) {
	t.Parallel()
	if hasPendingChecks([]scm.Check{{Name: "CodeRabbit", Bucket: scm.CheckBucketPending, Source: scm.CheckSourceLegacy, BlocksPending: false}}) {
		t.Fatal("advisory legacy status must not keep CI running")
	}
	if !hasPendingChecks([]scm.Check{{Name: "CodeRabbit", Bucket: scm.CheckBucketPending, Source: scm.CheckSourceLegacy, BlocksPending: true}}) {
		t.Fatal("protected legacy status must keep CI running")
	}
	if !hasPendingChecks([]scm.Check{{Name: "unit", Bucket: scm.CheckBucketPending, Source: scm.CheckSourceNative, BlocksPending: true}}) {
		t.Fatal("native pending check must keep CI running")
	}
}

func TestPendingCheckMatchesLastFixed_SpecialCheckNames(t *testing.T) {
	t.Parallel()

	lastFixedChecks := encodeLastFixedChecks([]string{"lint,unit", "deploy+conflict"}, true)
	checks := []scm.Check{
		{Name: "lint,unit", Bucket: "pending"},
	}

	if !pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected pending check with special characters to match encoded last fixed checks %q", lastFixedChecks)
	}

	checks = []scm.Check{
		{Name: "lint", Bucket: "pending"},
	}
	if pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected unrelated pending check not to match encoded last fixed checks %q", lastFixedChecks)
	}
}

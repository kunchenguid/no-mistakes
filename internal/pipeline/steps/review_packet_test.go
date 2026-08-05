package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReviewPacket_BindsCompleteManifestAndBranchDiff(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)

	packet, err := buildReviewPacket(context.Background(), dir, base, head, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"base_commit: " + base,
		"target_commit: " + head,
		`"feature.txt"`,
		"diff --git a/feature.txt b/feature.txt",
	} {
		if !strings.Contains(packet.Text, want) {
			t.Errorf("packet missing %q:\n%s", want, packet.Text)
		}
	}
	if packet.Metrics.PacketOmittedParts != 0 || packet.Metrics.OversizeFallback {
		t.Fatalf("ordinary packet unexpectedly omitted content: %+v", packet.Metrics)
	}
	if packet.Metrics.PacketBytes != len(packet.Text) {
		t.Fatalf("packet bytes = %d, want %d", packet.Metrics.PacketBytes, len(packet.Text))
	}
}

func TestBuildReviewPacket_OversizeDiffRequiresCompleteSelfDiscovery(t *testing.T) {
	t.Parallel()
	dir, base, _ := setupGitRepo(t)
	large := strings.Repeat("this line belongs in an intentionally large review diff\n", maxReviewPacketBranchDiffBytes)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(large), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "large.txt")
	gitCmd(t, dir, "commit", "-m", "large review diff")
	head := gitCmd(t, dir, "rev-parse", "HEAD")

	packet, err := buildReviewPacket(context.Background(), dir, base, head, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Text) > maxReviewPacketBytes {
		t.Fatalf("packet has %d bytes, cap is %d", len(packet.Text), maxReviewPacketBytes)
	}
	for _, want := range []string{
		"REVIEW_PACKET_OMITTED",
		"complete branch diff yourself",
		`"large.txt"`,
	} {
		if !strings.Contains(packet.Text, want) {
			t.Errorf("oversize packet missing %q:\n%s", want, packet.Text)
		}
	}
	if !packet.Metrics.OversizeFallback || packet.Metrics.PacketOmittedParts == 0 {
		t.Fatalf("oversize packet metrics = %+v, want explicit fallback", packet.Metrics)
	}
}

func TestBuildReviewPacket_FixDeltaIsOnlyAStartingIndex(t *testing.T) {
	t.Parallel()
	dir, base, beforeFix := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "fix.txt")
	gitCmd(t, dir, "commit", "-m", "fix")
	head := gitCmd(t, dir, "rev-parse", "HEAD")

	packet, err := buildReviewPacket(context.Background(), dir, base, head, beforeFix)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Fix delta (starting index only; never a review scope limit):",
		"from_commit: " + beforeFix,
		"target_commit: " + head,
		"diff --git a/fix.txt b/fix.txt",
	} {
		if !strings.Contains(packet.Text, want) {
			t.Errorf("packet missing %q:\n%s", want, packet.Text)
		}
	}
}

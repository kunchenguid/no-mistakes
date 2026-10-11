package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestSupportRefreshRunsOnlyTrustedConfiguredCommand(t *testing.T) {
	for _, tc := range []struct {
		name, command, claim string
		wantExit             int
		wantErr              bool
	}{
		{"success", "echo refreshed > .git/owner-refreshed", "echo refreshed > .git/owner-refreshed", 0, false},
		{"failure", "exit 7", "exit 7", 7, false},
		{"mismatched selector", "echo refreshed > .git/owner-refreshed", "echo injected > .git/injected", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, receipt := completedReviewSupportFixture(t, pendingReviewTestClaim(tc.claim))
			c.Config.Commands.Test = tc.command
			outcome, err := (&TestStep{}).RefreshReviewSupport(c)
			if tc.wantErr {
				if err == nil {
					t.Fatal("mismatched command accepted")
				}
				if _, err := os.Stat(filepath.Join(c.WorkDir, ".git", "owner-refreshed")); !os.IsNotExist(err) {
					t.Fatal("trusted command ran for mismatched selector")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("owner evidence=%+v %v", findings, err)
			}
			proof := findings.Items[0].Support.OwnerResult
			if proof == nil || proof.ExitCode == nil || *proof.ExitCode != tc.wantExit || proof.HeadSHA != receipt.LocalHeadSHA || proof.TargetSHA != receipt.TargetSHA || proof.DiffDigest != receipt.DiffDigest || proof.Generation != receipt.Generation {
				t.Fatalf("proof=%+v", proof)
			}
			if tc.wantExit == 0 {
				if _, err := os.Stat(filepath.Join(c.WorkDir, ".git", "owner-refreshed")); err != nil {
					t.Fatalf("configured command not executed: %v", err)
				}
			} else if !outcome.NeedsApproval {
				t.Fatal("failed current command was certified")
			}
		})
	}
}

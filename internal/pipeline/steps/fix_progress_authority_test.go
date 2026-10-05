package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/config"
	"strings"
	"testing"
)

func TestFixProgressAuthorityRescueCannotAuthorizePush(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	p, err := sctx.DB.BeginWorkRescue(sctx.Run, "review", "", sctx.Run.HeadSHA, sctx.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	p.State = "saved"
	p.SHA = "private-wip"
	p.Ref = "refs/no-mistakes/rescue/" + p.RunID + "/review/" + p.StopID
	if err = sctx.DB.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, sctx, head)
	err = publishRunHead(sctx, sctx.Run.HeadSHA, "", nil)
	if err == nil || !strings.Contains(err.Error(), "unfinished work") {
		t.Fatalf("rescue passed publication boundary: %v", err)
	}
}

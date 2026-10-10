package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
	toON "github.com/toon-format/toon-go"
)

type reviewedRecoveryFlags struct{ head, run, local, consent string }

func (f *reviewedRecoveryFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.head, "adopt-reviewed-head", "", "preview explicit adoption of this exact verified terminal review head; ordinary containment rules remain unchanged")
	cmd.Flags().StringVar(&f.run, "run", "", "exact terminal run for --adopt-reviewed-head")
	cmd.Flags().StringVar(&f.local, "expected-local-head", "", "exact submitted or published caller commit for --adopt-reviewed-head")
	cmd.Flags().StringVar(&f.consent, "consent", "", "explicit adoption consent: exact digest from the complete read-only preview")
}

func (f reviewedRecoveryFlags) requested() bool {
	return f.head != "" || f.run != "" || f.local != "" || f.consent != ""
}

func runReviewedRecovery(cmd *cobra.Command, flags reviewedRecoveryFlags, axi bool) error {
	fail := func(code int, err error) error {
		if axi {
			return emitError(cmd, code, err.Error())
		}
		return &exitError{code: code, err: err}
	}
	if flags.head == "" || flags.run == "" || flags.local == "" {
		return fail(2, fmt.Errorf("--adopt-reviewed-head, --run and --expected-local-head are required together"))
	}
	p, err := paths.New()
	if err != nil {
		return fail(1, err)
	}
	// A preview cannot initialize or migrate local state. Apply also opens only
	// the existing schema, so evidence validation precedes its first write.
	open := db.OpenReadOnly
	if flags.consent != "" {
		open = db.OpenExisting
	}
	database, err := open(p.DB())
	if err != nil {
		return fail(1, err)
	}
	defer database.Close()
	repo, err := findRepo(database)
	if err != nil {
		return fail(1, err)
	}
	service := &branchsync.Service{DB: database, Repo: repo, WorkDir: ".", GateDir: p.RepoDir(repo.ID), Paths: p}
	request := branchsync.ReviewedRecoveryRequest{RunID: flags.run, ExpectedLocalHead: flags.local, ReviewedHead: flags.head}
	plan, err := service.PreviewReviewedRecovery(cmd.Context(), request)
	if err != nil {
		return fail(1, err)
	}
	emitDoc(cmd, toON.Field{Key: "reviewed_recovery", Value: map[string]any{
		"repository_id": plan.RepositoryID, "caller": plan.Caller, "common_dir": plan.CommonDir,
		"branch": plan.Branch, "run": plan.Request.RunID, "submitted_head": plan.SubmittedHead,
		"gate_dir":       plan.GateDir,
		"published_head": plan.PublishedHead, "expected_local_head": plan.Request.ExpectedLocalHead,
		"reviewed_head": plan.Request.ReviewedHead, "gate_head": plan.GateHead, "recovery_ref": plan.RecoveryRef,
		"source_tree": plan.SourceTree, "target_tree": plan.TargetTree, "status": plan.Status,
		"verified_at": plan.VerifiedAt, "diff": plan.Diff, "consent_digest": plan.Digest,
		"note": "Explicit adoption accepts the displayed reviewed rewrite; it is not a containment proof, push, rerun or validation result.",
	}})
	if flags.consent == "" {
		return nil
	}
	state := service.AdoptReviewedRecovery(cmd.Context(), request, flags.consent)
	if axi {
		emitDoc(cmd, branchSyncField(state))
	} else {
		printHumanSyncState(cmd, state)
	}
	if !state.Recovered {
		return fail(1, fmt.Errorf("%s", state.Error))
	}
	return nil
}

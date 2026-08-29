package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/tui"
)

// TestSettlementGateMovePromiseStaysConditional is the ONE guard on a single
// CLAIM across every surface that states it, rather than one guard per file
// family.
//
// The claim: the keep-local custody return points the gate branch at the kept
// head. It is false for a state the advertisement predicates deliberately
// admit - with the gate branch PROVEN ABSENT, recoverSettleInconsistent stamps
// custody with no compare-and-swap and no gate branch touched at all - so every
// surface must carry branchsync.SettlementGateMoveQualifier in the same
// sentence as the promise.
//
// This test's population is the claim, because the previous scoping was the
// help text: a correction purged the phrasing from the four cobra help strings
// and pinned it with TestKeepLocalHelpSurfacesStayConditional, while the
// identical promise survived on the structured branch_sync error, the TUI
// confirmation box, the agent guidance, the skill and the agents guide. Drift
// on ANY of these now fails here.
//
// Where a surface has a real interface it is executed: cobra renders its own
// help, the TUI renders its own confirmation, branchsync classifies a real
// wedged record from a real repository, and the guidance constant is the value
// the CLI emits. The generated skill and the two docs pages are read as the
// owned text contracts they already are, following the precedent in
// axi_guidance_test.go.
func TestSettlementGateMovePromiseStaysConditional(t *testing.T) {
	for name, text := range settlementClaimSurfaces(t) {
		if offender := branchsync.UnqualifiedGateMovePromise(text); offender != "" {
			t.Errorf("%s promises the settlement moves the gate branch without conditioning it on %q:\n\t%s",
				name, branchsync.SettlementGateMoveQualifier, offender)
		}
	}
}

// TestSettlementGateMovePromiseSurfacePopulationIsComplete keeps the guard
// above honest. A surface that silently stopped stating the claim - or a
// fixture that stopped producing the settlement advertisement - would make the
// invariant vacuously true there, which is exactly how the promise survived
// the last correction.
func TestSettlementGateMovePromiseSurfacePopulationIsComplete(t *testing.T) {
	for name, text := range settlementClaimSurfaces(t) {
		// Normalized, because hard-wrapped help, boxed TUI output and
		// concatenated Go strings all break the qualifier across lines.
		if !strings.Contains(strings.Join(strings.Fields(text), " "), branchsync.SettlementGateMoveQualifier) {
			t.Errorf("%s no longer states the qualified gate-move consequence at all, so the invariant is vacuous there:\n%s", name, text)
		}
	}
}

// settlementClaimSurfaces collects every surface that describes what the
// keep-local custody return does to the gate branch.
func settlementClaimSurfaces(t *testing.T) map[string]string {
	t.Helper()

	humanHelp, err := executeCmd("sync", "--help")
	if err != nil {
		t.Fatalf("sync --help: %v\n%s", err, humanHelp)
	}
	axiHelp, err := executeCmd("axi", "sync", "--help")
	if err != nil {
		t.Fatalf("axi sync --help: %v\n%s", err, axiHelp)
	}

	// The docs pages and the agents guide are read through repo-relative paths,
	// so they must be loaded BEFORE the wedged fixture changes the working
	// directory to its operator worktree.
	agentsGuide := readAgentsGuide(t)
	cliReference := readDocsPage(t, "reference", "cli.md")
	generatedSkill := skill.Markdown()
	advertisement := wedgedSettlementAdvertisement(t)

	return map[string]string{
		// Keep the per-surface isolation the help contract test established:
		// the flag line and the Long text are separate surfaces, and a shared
		// grep over the whole blob let one satisfy the check for the other.
		"sync --keep-local flag help":     keepLocalFlagUsage(t, humanHelp),
		"sync long description":           helpLongDescription(t, humanHelp),
		"axi sync --keep-local flag help": keepLocalFlagUsage(t, axiHelp),
		"axi sync long description":       helpLongDescription(t, axiHelp),
		"live branch-sync agent guidance": branchSyncAgentGuidance,
		"structured branch_sync error":    advertisement,
		"TUI settlement confirmation":     unboxed(tui.RenderSettleConfirmation(wedgedSettlementState(), 80)),
		"generated skill":                 generatedSkill,
		"agents guide":                    agentsGuide,
		"cli reference":                   cliReference,
	}
}

// wedgedSettlementAdvertisement returns the structured branch_sync error a real
// wedged custody record produces, read from classifyPipelineOwned through the
// same service the CLI surfaces build.
func wedgedSettlementAdvertisement(t *testing.T) string {
	t.Helper()
	_, p, _ := wedgedCustodyAbortFixture(t)

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer database.Close()
	repo, err := findRepo(database)
	if err != nil || repo == nil {
		t.Fatalf("find fixture repo: repo=%#v err=%v", repo, err)
	}
	service := &branchsync.Service{
		DB:      database,
		Repo:    repo,
		WorkDir: ".",
		GateDir: p.RepoDir(repo.ID),
		Paths:   p,
	}
	state := service.InspectCached(context.Background())
	// The fixture must actually reach the settlement advertisement, or the
	// message under test is not the one an operator would read.
	if state.NextAction == nil || state.NextAction.Code != "return_custody_keep_local" {
		t.Fatalf("fixture did not advertise the keep-local settlement: %#v", state.NextAction)
	}
	return state.Error
}

// wedgedSettlementState is the state shape the TUI receives for that same
// record: pipeline_owned carrying the settlement next action.
func wedgedSettlementState() branchsync.State {
	return branchsync.State{
		State:      branchsync.StatePipelineOwned,
		Safety:     "blocked_recover_preserved_head_missing",
		Local:      branchsync.LocalState{Branch: "feature", Head: strings.Repeat("a", 40), Clean: true},
		Pipeline:   branchsync.PipelineState{RunID: "run-1", Status: "failed", Phase: "pre_push", CurrentHead: strings.Repeat("c", 40)},
		NextAction: &branchsync.NextAction{Code: "return_custody_keep_local", Command: "no-mistakes axi sync --recover --keep-local"},
	}
}

// unboxed reduces a rendered TUI box to the prose inside it. The border runes
// and their padding land mid-sentence on every wrapped line, so a claim would
// otherwise be unreadable as the one sentence an operator sees.
func unboxed(rendered string) string {
	plain := ansiEscape.ReplaceAllString(rendered, "")
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("│╭╮╰╯─", r) {
			return ' '
		}
		return r
	}, plain)
}

func readDocsPage(t *testing.T, section, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "src", "content", "docs", section, name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

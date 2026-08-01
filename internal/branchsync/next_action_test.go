package branchsync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nextActionCases is the single description of the next-action vocabulary: the
// exact wire string each code ships as, the command it is paired with, and
// whether it counts as taking the pipeline's commits. Every property of a code
// is asserted from this one table, checked for completeness against
// allNextActionCodes, so a new code cannot land in some of the assertions and
// miss the others.
var nextActionCases = map[NextActionCode]struct {
	wire            string
	action          *NextAction
	command         string
	synchronization bool
}{
	NextActionSync:                        {"sync", syncAction(), "no-mistakes axi sync", true},
	NextActionCheckSync:                   {"check_sync", checkSyncAction(), "no-mistakes axi sync --check", true},
	NextActionRecoverCustody:              {"recover_custody", recoverCustodyAction(), "no-mistakes axi sync --recover", true},
	NextActionRetry:                       {"retry", retryAction(), "no-mistakes axi sync --check", true},
	NextActionRunPipeline:                 {"run_pipeline", runPipelineAction(), `no-mistakes axi run --intent "<what the user set out to accomplish>"`, false},
	NextActionInspectWorktree:             {"inspect_worktree", inspectWorktreeAction(), "git status", false},
	NextActionContinueActiveRun:           {"continue_active_run", continueActiveRunAction(), "no-mistakes axi status", false},
	NextActionInspectAndReconcileManually: {"inspect_and_reconcile_manually", reconcileManuallyAction("refs/no-mistakes/x"), "git log --oneline --left-right HEAD...refs/no-mistakes/x", false},
}

// The codes ship verbatim to agents in skills/no-mistakes/sync-recovery.md and
// in docs/src/content/docs/reference/cli.md, so renaming one breaks every agent
// and document matching on it. The wire strings, the command each code is
// paired with by its constructor, and the synchronization classification are
// all pinned here against the complete vocabulary.
func TestNextActionVocabulary(t *testing.T) {
	if len(nextActionCases) != len(allNextActionCodes) {
		t.Fatalf("described %d codes, want all %d", len(nextActionCases), len(allNextActionCodes))
	}
	for _, code := range allNextActionCodes {
		want, ok := nextActionCases[code]
		if !ok {
			t.Errorf("code %q is undescribed; pin its wire value, command, and whether it takes the pipeline's commits", code)
			continue
		}
		t.Run(string(code), func(t *testing.T) {
			if string(code) != want.wire {
				t.Errorf("code = %q, want %q", string(code), want.wire)
			}
			if want.action.Code != code {
				t.Errorf("constructed Code = %q, want %q", want.action.Code, code)
			}
			if got := want.action.Command; got != want.command {
				t.Errorf("Command = %q, want %q", got, want.command)
			}
			if got := want.action.IsSynchronization(); got != want.synchronization {
				t.Errorf("IsSynchronization() = %v, want %v", got, want.synchronization)
			}
		})
	}
	if (*NextAction)(nil).IsSynchronization() {
		t.Error("a missing next action must not read as a synchronization action")
	}
}

// allNextActionCodes is what every other assertion derives its exhaustiveness
// from, so the slice itself has to be tied to the declarations: a ninth code
// minted in the const block but forgotten here would otherwise reach agents
// unclassified and undocumented with a green suite.
func TestAllNextActionCodesCoversEveryDeclaredConstant(t *testing.T) {
	listed := make(map[string]bool, len(allNextActionCodes))
	for _, code := range allNextActionCodes {
		listed[string(code)] = true
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	declared := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typeName, ok := value.Type.(*ast.Ident)
				if !ok || typeName.Name != "NextActionCode" {
					continue
				}
				for i, ident := range value.Names {
					declared++
					if i >= len(value.Values) {
						t.Errorf("constant %s has no literal value", ident.Name)
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("constant %s is not a string literal", ident.Name)
						continue
					}
					wire, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Errorf("unquote %s: %v", ident.Name, err)
						continue
					}
					if !listed[wire] {
						t.Errorf("constant %s (%q) is not in allNextActionCodes", ident.Name, wire)
					}
				}
			}
		}
	}
	if declared != len(allNextActionCodes) {
		t.Errorf("declared %d NextActionCode constants, allNextActionCodes lists %d", declared, len(allNextActionCodes))
	}
}

package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func assertExternalPublisherPolicy(sctx *pipeline.StepContext, head string) error {
	policy := sctx.Config.Push
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("refuse publication: %w", err)
	}
	gate := policy.ExternalPublisher
	if gate == nil {
		return nil
	}
	for _, name := range gate.Paths {
		// Literal pathspecs prevent glob or pathspec magic in repository filenames
		// from widening the declaration. Read the immutable proposed tree, never
		// the mutable worktree or its untracked files.
		entry, err := stepGitRun(sctx, "ls-tree", "-z", "--full-tree", head, "--", ":(literal)"+name)
		if err != nil {
			return fmt.Errorf("refuse publication: inspect push.external_publisher path %q at %s: %w", name, head, err)
		}
		if entry != "" {
			return fmt.Errorf("refuse publication at %s: external publisher reserved by push.external_publisher path %q: %s Remedy: %s Use no-mistakes axi sync --recover to recover unpublished pipeline fixes", head, name, strings.TrimSpace(gate.Reason), strings.TrimSpace(gate.Remedy))
		}
	}
	return nil
}

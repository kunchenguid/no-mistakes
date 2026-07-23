package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cursorQuarantineDirPrefix names the private, collision-proof directory created
// under the agent CWD while a Cursor gate run suppresses project instruction
// surfaces. Cursor CLI has no --no-project-instructions flag; empirical canaries
// show it auto-loads root AGENTS.md / CLAUDE.md and .cursor/rules when present,
// and stops loading them when those paths are absent for the duration of the run.
const cursorQuarantineDirPrefix = ".no-mistakes-cursor-quarantine-"

// cursorInstructionSurfaces are the project paths Cursor auto-loads. Only
// .cursor/rules is quarantined under .cursor/ — hooks, mcp.json, and other
// sibling content must remain visible.
func cursorInstructionSurfaces() []string {
	return []string{
		"AGENTS.md",
		"CLAUDE.md",
		filepath.Join(".cursor", "rules"),
	}
}

// isCursorGateTarget reports whether this ACP agent is Cursor: the first-class
// cursor alias or an explicit acp:cursor target. Generic acp:<other> targets
// never qualify, even if a registry override happens to name cursor-agent.
func isCursorGateTarget(target string) bool {
	return target == "cursor"
}

type quarantinedItem struct {
	original string
	parked   string
}

// cursorInstructionQuarantine holds rename-backed moves of Cursor instruction
// surfaces so they can be restored byte-exact after the agent exits.
type cursorInstructionQuarantine struct {
	dir   string
	items []quarantinedItem
}

// beginCursorInstructionQuarantine renames present instruction surfaces out of
// cwd into a private quarantine directory. Missing surfaces are skipped. On any
// failure, already-moved items are restored before the error is returned.
func beginCursorInstructionQuarantine(cwd string) (*cursorInstructionQuarantine, error) {
	q := &cursorInstructionQuarantine{}
	if strings.TrimSpace(cwd) == "" {
		return q, nil
	}
	dir, err := os.MkdirTemp(cwd, cursorQuarantineDirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create quarantine dir: %w", err)
	}
	q.dir = dir

	for _, rel := range cursorInstructionSurfaces() {
		src := filepath.Join(cwd, rel)
		fi, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			_ = q.Restore()
			return nil, fmt.Errorf("stat %s: %w", rel, err)
		}
		parked := filepath.Join(dir, strconv.Itoa(len(q.items))+"-"+sanitizeQuarantineName(rel, fi.IsDir()))
		if err := os.Rename(src, parked); err != nil {
			_ = q.Restore()
			return nil, fmt.Errorf("quarantine %s: %w", rel, err)
		}
		q.items = append(q.items, quarantinedItem{original: src, parked: parked})
	}
	if len(q.items) == 0 {
		_ = os.Remove(dir)
		q.dir = ""
	}
	return q, nil
}

func sanitizeQuarantineName(rel string, isDir bool) string {
	name := strings.ReplaceAll(rel, string(filepath.Separator), "_")
	if isDir {
		return name + ".dir"
	}
	return name
}

// Restore renames every quarantined surface back to its original path and
// removes the quarantine directory only after every parked item is restored.
// It is safe to call multiple times and on a nil receiver. Partial restore
// failures are reported; failed items stay parked and remain in q.items so a
// later Restore can retry without losing the only remaining bytes.
func (q *cursorInstructionQuarantine) Restore() error {
	if q == nil {
		return nil
	}
	var firstErr error
	for i := len(q.items) - 1; i >= 0; i-- {
		item := q.items[i]
		if err := os.MkdirAll(filepath.Dir(item.original), 0o755); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore mkdir %s: %w", item.original, err)
			}
			continue
		}
		if err := os.Rename(item.parked, item.original); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore %s: %w", item.original, err)
			}
			continue
		}
		q.items = append(q.items[:i], q.items[i+1:]...)
	}
	if len(q.items) > 0 {
		return firstErr
	}
	if q.dir != "" {
		if err := os.RemoveAll(q.dir); err != nil {
			return fmt.Errorf("remove quarantine dir: %w", err)
		}
		q.dir = ""
	}
	return nil
}

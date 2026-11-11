package steps

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// readTrustedTreeFile is the one daemon-side read of a repository file's bytes
// from the pinned trusted default-branch commit (pr.template,
// test.instructions_file, test.path_instructions[].instructions_file). It never
// opens a worktree file: the daemon pins sha after a fresh default-branch
// fetch, including on recovery. It resolves the literal tree entry first
// (rejecting symlinks and submodules), then size-checks and reads that
// immutable blob, so neither pushed paths nor allow_repo_commands choose its
// bytes. setting names the config key in every error. A missing, oversize,
// empty, non-UTF-8, or NUL-bearing file fails closed.
func readTrustedTreeFile(ctx context.Context, dir, sha, setting, name string, maxBytes int) (string, error) {
	if err := config.ValidateTrustedFilePath(setting, name); err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("%s must name a file", setting)
	}
	if (len(sha) != 40 && len(sha) != 64) || !isHexObjectID(sha) {
		return "", fmt.Errorf("%s requires a pinned trusted default-branch commit", setting)
	}
	entry, err := git.RunRaw(ctx, dir, "ls-tree", "-z", sha, "--", ":(literal)"+name)
	if err != nil {
		return "", fmt.Errorf("read trusted %s tree: %w", setting, err)
	}
	metadata, entryName, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || entryName != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !isHexObjectID(fields[2]) {
		return "", fmt.Errorf("%s %q must name a regular file in the pinned trusted tree (no symlinks or submodules)", setting, name)
	}
	sizeText, err := git.Run(ctx, dir, "cat-file", "-s", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted %s size: %w", setting, err)
	}
	size, err := strconv.Atoi(sizeText)
	if err != nil || size <= 0 || size > maxBytes {
		return "", fmt.Errorf("%s must contain 1..%d bytes", setting, maxBytes)
	}
	data, err := git.RunRaw(ctx, dir, "cat-file", "blob", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted %s blob: %w", setting, err)
	}
	if len(data) != size || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%s must contain nonempty UTF-8 text without NUL bytes", setting)
	}
	return string(data), nil
}

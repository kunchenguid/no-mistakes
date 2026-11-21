package agent

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

//go:embed cursor_preflight.mjs
var cursorPreflightBridge string

const cursorCatalogTimeout = 15 * time.Second
const cursorCatalogOutputLimit = 16 * 1024

type CursorModelStatus string

const (
	CursorModelSupported     CursorModelStatus = "supported"
	CursorModelUnsupported   CursorModelStatus = "unsupported"
	CursorModelIndeterminate CursorModelStatus = "indeterminate"
)

// CursorModelError deliberately contains no backend output, catalog or context.
// The configuration owner and requested opaque ID are the correction route.
type CursorModelError struct {
	Status       CursorModelStatus
	Owner, Model string
}

func (e *CursorModelError) Error() string {
	if e.Status == CursorModelUnsupported {
		return fmt.Sprintf("Cursor model %q in %s is unsupported by the effective ACP catalog; select an exact advertised ACP model ID", e.Model, e.Owner)
	}
	return fmt.Sprintf("Cursor model %q in %s is indeterminate; verify Cursor authentication and an installed acpx public runtime/agent-registry SDK with Node >=22.13, and set acpx_path to its direct Node entrypoint; this model invocation was blocked", e.Model, e.Owner)
}

func PreflightCursorModel(ctx context.Context, name types.AgentName, bin string, options Options, run RunOpts, owner string) error {
	target, ok := types.ACPTargetFor(name)
	if !ok || target != "cursor" {
		return nil
	}
	a := &acpxAgent{bin: bin, target: target, rawCommand: types.ACPRawCommand(target, options.ACPRegistryOverrides), model: options.Profile.Model, disableProjectSettings: options.DisableProjectSettings, subprocessContext: newSubprocessContext(options.Environment)}
	return a.preflightCursor(ctx, run, owner)
}

func (a *acpxAgent) cursorModelOwner() string {
	if a.configOwner != "" {
		return a.configOwner
	}
	return "agent_config.cursor.model"
}

func (a *acpxAgent) preflightCursor(ctx context.Context, opts RunOpts, owner string) error {
	if a.target != "cursor" {
		return nil
	}
	if a.disableProjectSettings {
		return EnsureGateNeutralized(a)
	}
	if a.model == "" {
		return nil
	}
	refuse := func(status CursorModelStatus) error {
		return &CursorModelError{Status: status, Owner: owner, Model: a.model}
	}
	env := a.gitSafeEnv(opts.CWD, opts.Env)
	entry, err := cursorExecutable(a.bin, opts.CWD, env)
	if err != nil {
		return refuse(CursorModelIndeterminate)
	}
	root, node, err := cursorSDKProvenance(entry, opts.CWD, env)
	if err != nil {
		return refuse(CursorModelIndeterminate)
	}
	input, _ := json.Marshal(struct{ Entry, Root, Raw, Model string }{entry, root, a.rawCommand, a.model})
	probeCtx, cancel := context.WithTimeout(ctx, cursorCatalogTimeout+2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, node, "--input-type=module", "--eval", cursorPreflightBridge)
	cmd.Dir, cmd.Env, cmd.Stdin = opts.CWD, env, bytes.NewReader(input)
	stdout := &cursorBoundedOutput{cancel: cancel}
	stderr := &cursorBoundedOutput{cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	shellenv.ConfigureShellCommand(cmd)
	if err := shellenv.RunShellCommand(cmd); err != nil || probeCtx.Err() != nil || stdout.exceeded || stderr.exceeded {
		return refuse(CursorModelIndeterminate)
	}
	var result struct {
		Status CursorModelStatus `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return refuse(CursorModelIndeterminate)
	}
	switch result.Status {
	case CursorModelSupported:
		return nil
	case CursorModelUnsupported:
		return refuse(CursorModelUnsupported)
	default:
		return refuse(CursorModelIndeterminate)
	}
}

type cursorBoundedOutput struct {
	sync.Mutex
	bytes.Buffer
	cancel   context.CancelFunc
	exceeded bool
}

func (b *cursorBoundedOutput) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	n := len(p)
	remaining := cursorCatalogOutputLimit - b.Len()
	if n > remaining {
		b.exceeded = true
		b.cancel()
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// Resolve against precisely the child environment, never the repository's Node
// dependencies. Unknown launch wrappers are refused rather than interpreted.
func cursorExecutable(name, cwd string, env []string) (string, error) {
	var candidates []string
	if strings.ContainsAny(name, "/\\") {
		candidates = []string{name}
	} else {
		path := ""
		for _, item := range env {
			if strings.HasPrefix(item, "PATH=") {
				path = strings.TrimPrefix(item, "PATH=")
			}
		}
		for _, dir := range filepath.SplitList(path) {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cwd, candidate)
		}
		st, err := os.Stat(candidate)
		if err != nil || st.IsDir() || st.Mode()&0111 == 0 {
			continue
		}
		return filepath.EvalSymlinks(candidate)
	}
	return "", os.ErrNotExist
}

func cursorReadBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if len(b) > 65536 {
		return nil, fmt.Errorf("oversized dependency metadata")
	}
	return b, err
}
func cursorSDKProvenance(entry, cwd string, env []string) (string, string, error) {
	f, err := os.Open(entry)
	if err != nil {
		return "", "", err
	}
	line, err := bufio.NewReader(io.LimitReader(f, 1024)).ReadString('\n')
	_ = f.Close()
	if err != nil {
		return "", "", err
	}
	line = strings.TrimSuffix(line, "\n")
	interpreter := strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	var node string
	switch {
	case line == "#!/usr/bin/env node":
		node, err = cursorExecutable("node", cwd, env)
	case strings.HasPrefix(line, "#!/") && filepath.Base(interpreter) == "node":
		node, err = cursorExecutable(interpreter, cwd, env)
	default:
		return "", "", fmt.Errorf("unrecognized acpx launcher")
	}
	if err != nil {
		return "", "", err
	}
	dir := filepath.Dir(entry)
	for depth := 0; depth < 8; depth++ {
		raw, readErr := cursorReadBounded(filepath.Join(dir, "package.json"))
		if readErr == nil {
			var pkg struct {
				Name string
				Bin  json.RawMessage
			}
			if json.Unmarshal(raw, &pkg) != nil || pkg.Name != "acpx" {
				return "", "", fmt.Errorf("unrecognized package")
			}
			var bin string
			if json.Unmarshal(pkg.Bin, &bin) != nil {
				var bins map[string]string
				if json.Unmarshal(pkg.Bin, &bins) != nil {
					return "", "", fmt.Errorf("invalid package bin")
				}
				bin = bins["acpx"]
			}
			resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(dir, bin))
			if bin == "" || resolveErr != nil || resolved != entry {
				return "", "", fmt.Errorf("entrypoint mismatch")
			}
			return dir, node, nil
		}
		if !os.IsNotExist(readErr) {
			return "", "", readErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", fmt.Errorf("public acpx SDK package unavailable")
}

package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

//go:embed pi_output.ts.tmpl
var piOutputExtension string

const piOutputTool = "no_mistakes_output"

// piLifecycleOutputPath is the adapter-control lifecycle phase that names, in
// the step log, which structured-output path an invocation took.
const piLifecycleOutputPath = "structured_output"

// Pi's documented constrainedSampling strict=require contract was introduced
// in 0.82.0; earlier releases silently ignore it, so a successful tool call
// there would not prove that generation was schema-constrained.
var piStrictOutputMinVersion = [3]int{0, 82, 0}

// piStrictUnsupported is the error pi-ai raises before any request is sent
// when the selected provider/model (or this schema) cannot take a
// strict=require JSON-schema tool.
var piStrictUnsupported = fmt.Sprintf("Tool %q requires JSON-schema constrained sampling", piOutputTool)

type piOutputSupport struct {
	mu          sync.Mutex
	probed      bool
	version     string
	unsupported bool
}

// runStructured runs one invocation on the strict output-tool path when Pi and
// the provider support it, and otherwise on the prompt-inlined schema path
// whose final text is validated against the same schema.
func (a *piAgent) runStructured(ctx context.Context, opts RunOpts) (*Result, error) {
	if len(opts.JSONSchema) == 0 {
		return a.runOnce(ctx, opts, false)
	}
	strict, reason := a.structuredOutputPath(ctx, opts)
	if !strict {
		emitLifecycle(opts, LifecycleEvent{Agent: "pi", Phase: piLifecycleOutputPath, Message: "pi structured output: prompt-inlined schema (" + reason + ")"})
		return a.runOnce(ctx, opts, false)
	}
	emitLifecycle(opts, LifecycleEvent{Agent: "pi", Phase: piLifecycleOutputPath, Message: "pi structured output: strict schema-constrained " + piOutputTool + " tool"})
	result, err := a.runOnce(ctx, opts, true)
	if err == nil || !strings.Contains(err.Error(), piStrictUnsupported) {
		return result, err
	}
	a.output.mu.Lock()
	a.output.unsupported = true
	a.output.mu.Unlock()
	emitAgentControl(opts, LifecycleEvent{
		Agent:   "pi",
		Phase:   LifecyclePhaseFallback,
		Message: "pi provider/model cannot take strict JSON-schema tools; retrying with the prompt-inlined schema",
	})
	return a.runOnce(ctx, opts, false)
}

// structuredOutputPath reports whether the strict output tool is available,
// or why not. A successful version probe and a provider refusal are cached for
// the agent's lifetime; a failed probe is retried on the next invocation.
func (a *piAgent) structuredOutputPath(ctx context.Context, opts RunOpts) (bool, string) {
	a.output.mu.Lock()
	defer a.output.mu.Unlock()
	if !a.output.probed {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, a.bin, "--version")
		cmd.Dir = opts.CWD
		cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
		shellenv.ConfigureShellCommand(cmd)
		out, err := shellenv.OutputShellCommand(cmd)
		if err != nil {
			return false, fmt.Sprintf("pi --version failed: %v", err)
		}
		a.output.probed = true
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			a.output.version = fields[0]
		}
	}
	if flag := piToolRestrictionArg(a.extraArgs); flag != "" {
		return false, "configured " + flag + " keeps the output tool from the model"
	}
	if !piVersionSupportsStrictOutput(a.output.version) {
		return false, fmt.Sprintf("pi version %q predates %d.%d.%d", a.output.version, piStrictOutputMinVersion[0], piStrictOutputMinVersion[1], piStrictOutputMinVersion[2])
	}
	if a.output.unsupported {
		return false, "provider/model cannot take strict JSON-schema tools"
	}
	return true, ""
}

// piToolRestrictionArg returns the configured flag that replaces Pi's tool
// selection with an allowlist, or "". Pi applies that allowlist to extension
// tools as well and an extension cannot widen it, so under --no-tools or
// --tools the model never sees the output tool and every strict turn would end
// in text and be rejected.
func piToolRestrictionArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-tools", "-nt", "--tools", "-t":
			return args[i]
		}
		if piArgTakesValue(args[i]) {
			i++
		}
	}
	return ""
}

// piVersionSupportsStrictOutput compares a semantic version with the strict
// output minimum, ordering a prerelease before its own release.
func piVersionSupportsStrictOutput(version string) bool {
	version, _, _ = strings.Cut(strings.TrimPrefix(version, "v"), "+")
	core, _, prerelease := strings.Cut(version, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		if n != piStrictOutputMinVersion[i] {
			return n > piStrictOutputMinVersion[i]
		}
	}
	return !prerelease
}

// piStrictToolKeywords are the only JSON Schema keywords the strict tool
// declaration carries: the ones the Review schema uses, which are the ones
// measured against a real provider. A provider refuses a strict tool over a
// keyword it does not support as a plain request error, which the prompt-path
// fallback cannot recognise, so every other keyword (maxLength in the
// commit-summary schemas) stays out of the declaration and is enforced on the
// returned output instead.
var piStrictToolKeywords = map[string]bool{
	"type":        true,
	"properties":  true,
	"items":       true,
	"required":    true,
	"enum":        true,
	"description": true,
}

func piStrictToolSchema(value any) any {
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	declared := make(map[string]any, len(schema))
	for keyword, child := range schema {
		if !piStrictToolKeywords[keyword] {
			continue
		}
		switch keyword {
		case "properties":
			properties, ok := child.(map[string]any)
			if !ok {
				continue
			}
			kept := make(map[string]any, len(properties))
			for name, property := range properties {
				kept[name] = piStrictToolSchema(property)
			}
			declared[keyword] = kept
		case "items":
			declared[keyword] = piStrictToolSchema(child)
		default:
			declared[keyword] = child
		}
	}
	return declared
}

func preparePiOutput(schema json.RawMessage) (string, func(), error) {
	if len(schema) == 0 {
		return "", func() {}, nil
	}
	var declared any
	if err := json.Unmarshal(schema, &declared); err != nil {
		return "", nil, fmt.Errorf("pi output schema: %w", err)
	}
	allowOptionalSchemaNulls(declared)
	parameters, err := json.Marshal(piStrictToolSchema(declared))
	if err != nil {
		return "", nil, fmt.Errorf("pi output schema: %w", err)
	}
	f, err := os.CreateTemp("", "no-mistakes-pi-output-*.ts")
	if err != nil {
		return "", nil, fmt.Errorf("pi output extension: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	source := strings.Replace(piOutputExtension, "__NO_MISTAKES_SCHEMA__", string(parameters), 1)
	_, writeErr := f.WriteString(source)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		cleanup()
		if writeErr != nil {
			return "", nil, fmt.Errorf("pi output extension write: %w", writeErr)
		}
		return "", nil, fmt.Errorf("pi output extension close: %w", closeErr)
	}
	return f.Name(), cleanup, nil
}

func (p *piParser) rememberOutputTool(event map[string]any) {
	if event["toolName"] != piOutputTool || event["isError"] != false {
		return
	}
	result, ok := event["result"].(map[string]any)
	if !ok || result["terminate"] != true {
		return
	}
	details, ok := result["details"].(map[string]any)
	if !ok {
		return
	}
	output, exists := details["output"]
	if !exists {
		return
	}
	id, _ := event["toolCallId"].(string)
	if id == "" {
		return
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return
	}
	if p.outputTools == nil {
		p.outputTools = make(map[string]string)
	}
	p.outputTools[id] = string(encoded)
}

func (p *piParser) finalOutputToolText() string {
	if p.finalAssistant == nil || p.finalAssistant["stopReason"] != "toolUse" {
		return ""
	}
	content, _ := p.finalAssistant["content"].([]any)
	var id string
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "toolCall" {
			continue
		}
		// A mixed batch has not finished the review when it submits the result.
		// Accept only the terminating call from the final assistant message,
		// never a result emitted before another assistant turn or tool call.
		if block["name"] != piOutputTool || id != "" {
			return ""
		}
		id, _ = block["id"].(string)
		if id == "" {
			return ""
		}
	}
	return p.outputTools[id]
}

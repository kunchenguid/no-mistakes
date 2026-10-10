package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeStrictPi fakes a strict-capable Pi that saves the output extension it
// was launched with as pi-extension.ts in its working directory and submits
// output through the output tool.
func writeStrictPi(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	name, script := "pi", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 0.99.1; exit 0; fi
cat > /dev/null
prev=
for arg in "$@"; do
	if [ "$prev" = "--extension" ]; then cp "$arg" pi-extension.ts; fi
	prev=$arg
done
cat "$0.events"
`
	if runtime.GOOS == "windows" {
		name, script = "pi.cmd", strings.Join([]string{
			"@echo off",
			`if "%~1"=="--version" (echo 0.99.1& exit /b 0)`,
			"more > nul",
			":next",
			`if "%~1"=="" goto emit`,
			`if "%~1"=="--extension" copy /y "%~2" pi-extension.ts > nul`,
			"shift /1",
			"goto next",
			":emit",
			`type "%~f0.events"`,
			"",
		}, "\r\n")
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	events := `{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"final-1","name":"no_mistakes_output","arguments":{}}]}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"final-1","toolName":"no_mistakes_output","isError":false,"result":{"content":[],"details":{"output":` + output + `},"terminate":true}}` + "\n"
	if err := os.WriteFile(bin+".events", []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin
}

// strictPiDeclaration parses the tool parameters out of the extension Pi was
// launched with, the generated declaration Pi sends to the provider.
func strictPiDeclaration(t *testing.T, cwd string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(cwd, "pi-extension.ts"))
	if err != nil {
		t.Fatal(err)
	}
	_, tail, found := strings.Cut(string(content), "const parameters = ")
	if !found {
		t.Fatal("extension declares no tool parameters")
	}
	line, _, _ := strings.Cut(tail, "\n")
	var declaration map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimRight(line, "\r"), ";")), &declaration); err != nil {
		t.Fatal(err)
	}
	return declaration
}

func TestPiStrictDeclarationLeavesSummaryLengthToPostValidation(t *testing.T) {
	t.Parallel()
	overLong, err := json.Marshal(strings.Repeat("a", config.MaxFixMessageSummaryBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		schema  json.RawMessage
		output  string
		extract func(*agent.Result) error
	}{
		{
			name:   "commit summary",
			schema: commitSummarySchema,
			output: `{"summary":` + string(overLong) + `}`,
			extract: func(result *agent.Result) error {
				_, err := extractCommitSummary(result)
				return err
			},
		},
		{
			name:   "ci fix conclusion",
			schema: ciFixConclusionSchema,
			output: `{"summary":` + string(overLong) + `,"code_change_needed":true}`,
			extract: func(result *agent.Result) error {
				_, err := extractCIFixConclusion(result)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			pi, err := agent.New(types.AgentPi, writeStrictPi(t, tc.output), nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pi.Run(context.Background(), agent.RunOpts{Prompt: "fix", CWD: cwd, JSONSchema: tc.schema})
			if err != nil {
				t.Fatal(err)
			}

			declaration := strictPiDeclaration(t, cwd)
			properties, _ := declaration["properties"].(map[string]any)
			summary, _ := properties["summary"].(map[string]any)
			if summary["type"] != "string" {
				t.Fatalf("strict declaration lost the summary field: %v", declaration)
			}
			if _, declared := summary["maxLength"]; declared {
				t.Fatalf("strict declaration still carries maxLength: %v", summary)
			}

			if err := tc.extract(result); !errors.Is(err, errRejectedCommitSummary) {
				t.Fatalf("over-long summary must still be rejected, got %v", err)
			}
		})
	}
}

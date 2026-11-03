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

// strictSchemaViolations walks a strict function schema declaration the way
// pi-ai and the provider both enforce it: every object level must carry
// additionalProperties: false and a required list naming every property, and
// no keyword outside the strict subset may appear. It returns each violation
// as a JSON-path string.
func strictSchemaViolations(value any, path string) []string {
	schema, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	var violations []string
	isObject := schema["type"] == "object" || schema["properties"] != nil
	if isObject {
		if schema["additionalProperties"] != false {
			violations = append(violations, path+": additionalProperties must be false")
		}
		properties, _ := schema["properties"].(map[string]any)
		required := map[string]bool{}
		if names, ok := schema["required"].([]any); ok {
			for _, name := range names {
				if s, ok := name.(string); ok {
					required[s] = true
				}
			}
		}
		for name := range properties {
			if !required[name] {
				violations = append(violations, path+": required must name property "+name)
			}
		}
		for name := range required {
			if _, ok := properties[name]; !ok {
				violations = append(violations, path+": required names unknown property "+name)
			}
		}
	}
	for _, keyword := range []string{
		"$ref", "$defs", "definitions", "allOf", "oneOf", "patternProperties",
		"dependentSchemas", "dependencies", "unevaluatedProperties",
		"propertyNames", "contains", "prefixItems", "not", "if", "then", "else",
	} {
		if _, ok := schema[keyword]; ok {
			violations = append(violations, path+": "+keyword+" is outside the strict subset")
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, property := range properties {
			violations = append(violations, strictSchemaViolations(property, path+"/properties/"+name)...)
		}
	}
	if items, ok := schema["items"]; ok {
		violations = append(violations, strictSchemaViolations(items, path+"/items")...)
	}
	return violations
}

// The strict output tool's declared parameters must already satisfy the
// provider's strict function contract: pi-ai only started normalizing schemas
// at 0.84.4, and strict-capable releases before it send the declaration
// verbatim, which is the v1.106.0 review failure against openai-codex
// ("At schema path /, additionalProperties must be false"). Every schema the
// steps hand to RunOpts.JSONSchema must therefore declare a strict-valid tool.
func TestPiStrictDeclarationSatisfiesTheStrictFunctionContract(t *testing.T) {
	t.Parallel()
	schemas := map[string]json.RawMessage{
		"review":                reviewSchemaForFinalize(false),
		"review finalize":       reviewSchemaForFinalize(true),
		"lint findings":         findingsSchema,
		"test findings":         testFindingsSchema,
		"commit summary":        commitSummarySchema,
		"fix summary":           fixSummarySchema,
		"housekeeping findings": housekeepingFindingsSchema,
		"ci fix conclusion":     ciFixConclusionSchema,
		"pr content":            prContentSchema,
		"pr title":              prTitleSchema,
		"pr template":           templatePRContentSchema,
	}
	for name, schema := range schemas {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			pi, err := agent.New(types.AgentPi, writeStrictPi(t, `{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			// The run's output validation is irrelevant here: the fake Pi has
			// already copied the extension the adapter launched it with.
			_, _ = pi.Run(context.Background(), agent.RunOpts{Prompt: "review", CWD: cwd, JSONSchema: schema})
			declaration := strictPiDeclaration(t, cwd)
			if violations := strictSchemaViolations(declaration, "/"); len(violations) > 0 {
				t.Fatalf("strict declaration violates the function contract: %s", strings.Join(violations, "; "))
			}
		})
	}
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

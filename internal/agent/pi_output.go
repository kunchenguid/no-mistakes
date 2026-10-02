package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed pi_output.ts.tmpl
var piOutputExtension string

const piOutputTool = "no_mistakes_output"

// Pi's documented constrainedSampling strict=require contract was introduced
// in 0.82.0. The extension's VERSION guard prevents older releases from silently
// treating the final output tool as ordinary unconstrained function calling.
func preparePiOutput(schema json.RawMessage) (string, func(), error) {
	if len(schema) == 0 {
		return "", func() {}, nil
	}
	parameters, err := textValidationSchema(schema)
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

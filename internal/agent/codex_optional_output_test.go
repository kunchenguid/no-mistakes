package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The provider requires nullable placeholders on the wire, but omitted optional
// fields must still satisfy the caller's schema, just as they do for pi.
func TestCodexAgent_ValidatesCallerContractNotTransportPlaceholders(t *testing.T) {
	bin := writeFakeCodex(t, t.TempDir(), `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"live\":true}"}}'
`, strings.Join([]string{
		"@echo off",
		"more > nul",
		`echo {"type":"item.completed","item":{"type":"agent_message","text":"{\"live\":true}"}}`,
	}, "\r\n"))
	a := &codexAgent{bin: bin}
	schema := json.RawMessage(`{"type":"object","properties":{"live":{"type":"boolean"},"reason":{"type":"string"}},"required":["live"]}`)
	result, err := a.Run(context.Background(), RunOpts{Prompt: "synthetic validation", CWD: t.TempDir(), JSONSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"live":true}` {
		t.Fatalf("output changed: %s", result.Output)
	}
}

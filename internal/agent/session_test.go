package agent

import (
	"context"
	"strings"
	"testing"
)

// TestSupportsSessionResume_PerAdapter pins which adapters advertise durable
// session resume. Claude and codex have native resume (claude --resume,
// codex exec resume); every other adapter must run cold so the pipeline's
// fallback path records the cold invocation instead of assuming reuse.
func TestSupportsSessionResume_PerAdapter(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  bool
	}{
		{"claude", &claudeAgent{bin: "claude"}, true},
		{"codex", &codexAgent{bin: "codex"}, true},
		{"rovodev", &rovodevAgent{bin: "acli"}, false},
		{"opencode", &opencodeAgent{bin: "opencode"}, false},
		{"pi", &piAgent{bin: "pi"}, false},
		{"copilot", &copilotAgent{bin: "copilot"}, false},
		{"acpx", &acpxAgent{bin: "acpx", target: "gemini"}, false},
		{"noop", NewNoop(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupportsSessionResume(tc.agent); got != tc.want {
				t.Fatalf("SupportsSessionResume(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestClaudeAgent_BuildArgs_Resume(t *testing.T) {
	ca := &claudeAgent{bin: "claude"}
	args := ca.buildArgs("re-review the branch", nil, "sess-1234")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sess-1234") {
		t.Fatalf("resume args missing --resume <id>: %v", args)
	}
	if !strings.Contains(joined, "-p re-review the branch") {
		t.Fatalf("resume args must keep print-mode prompt: %v", args)
	}
	if strings.Contains(joined, "--fork-session") {
		t.Fatalf("resume must continue the same session, not fork: %v", args)
	}
}

func TestClaudeAgent_BuildArgs_NoResumeByDefault(t *testing.T) {
	ca := &claudeAgent{bin: "claude"}
	args := ca.buildArgs("review", nil, "")
	for _, a := range args {
		if a == "--resume" {
			t.Fatalf("cold invocation must not pass --resume: %v", args)
		}
	}
}

func TestParseClaudeEvents_CapturesSessionID(t *testing.T) {
	events := `{"type":"system","subtype":"init","session_id":"0dc121a5-fbc0-4e5a-ae81-5851cc03ca1b"}
{"type":"assistant","session_id":"0dc121a5-fbc0-4e5a-ae81-5851cc03ca1b","message":{"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}}
{"type":"result","subtype":"success","session_id":"0dc121a5-fbc0-4e5a-ae81-5851cc03ca1b","usage":{"input_tokens":1,"output_tokens":1}}
`
	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.sessionID != "0dc121a5-fbc0-4e5a-ae81-5851cc03ca1b" {
		t.Fatalf("sessionID = %q, want the stream session id", result.sessionID)
	}
	if result.model != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", result.model)
	}
}

func TestParseClaudeEvents_SessionIDFallsBackToLastSeen(t *testing.T) {
	// Some result events may omit session_id; the last-seen stream value wins.
	events := `{"type":"assistant","session_id":"sess-a","message":{"usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}}
{"type":"result","subtype":"success","usage":{"input_tokens":1,"output_tokens":1}}
`
	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result == nil || result.sessionID != "sess-a" {
		t.Fatalf("expected fallback session id sess-a, got %+v", result)
	}
}

func TestCodexAgent_BuildArgs_Resume(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("re-review the branch", "/tmp/schema.json", "thread-99")

	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "exec resume thread-99 ") {
		t.Fatalf("resume args must start with exec resume <id>: %v", args)
	}
	if !strings.Contains(joined, "--json") {
		t.Fatalf("resume args must keep --json: %v", args)
	}
	if !strings.Contains(joined, "--output-schema /tmp/schema.json") {
		t.Fatalf("resume args must keep --output-schema: %v", args)
	}
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("resume args must keep the sandbox bypass: %v", args)
	}
	// codex exec resume (0.144) does not accept --color; passing it fails the
	// whole invocation.
	if strings.Contains(joined, "--color") {
		t.Fatalf("resume args must not include --color: %v", args)
	}
}

func TestCodexAgent_BuildArgs_ResumeKeepsExtraArgs(t *testing.T) {
	ca := &codexAgent{bin: "codex", extraArgs: []string{"-m", "gpt-5.2-codex"}}
	args := ca.buildArgs("prompt", "", "thread-1")

	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "exec resume -m gpt-5.2-codex thread-1 prompt") {
		t.Fatalf("resume args must interleave user extraArgs before the session id: %v", args)
	}
}

func TestParseCodexEvents_CapturesThreadID(t *testing.T) {
	events := `{"type":"thread.started","thread_id":"019f4d4d-5dc0-75c1-8efe-adf4531bd733"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`
	var usage TokenUsage
	var lastMessage, codexErr, threadID string
	if err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage, &codexErr, &threadID, nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if threadID != "019f4d4d-5dc0-75c1-8efe-adf4531bd733" {
		t.Fatalf("threadID = %q, want the thread.started id", threadID)
	}
}

//go:build unix

package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSessionStubAcpx(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "acpx-session")
	script := `#!/bin/sh
# fingerprint-a
printf 'ARGV:%s\n' "$*" >> "$NM_TEST_ACPX_LOG"
case " $* " in
  *" config show "*)
    if [ -n "$NM_TEST_ACPX_CONFIG_FILE" ]; then
      cat "$NM_TEST_ACPX_CONFIG_FILE"
    elif [ -n "$NM_TEST_ACPX_CONFIG" ]; then
      printf '%s\n' "$NM_TEST_ACPX_CONFIG"
    else
      printf '{}\n'
    fi
    ;;
  *" sessions new "*)
    if [ -n "$NM_TEST_ACPX_SETUP_ERROR" ]; then
      printf '{"error":{"message":"%s"}}\n' "$NM_TEST_ACPX_SETUP_ERROR"
    else
      printf '{"action":"session_ensured","created":true,"acpxRecordId":"%s","acpxSessionId":"%s"}\n' "$NM_TEST_ACPX_SETUP_ID" "$NM_TEST_ACPX_SETUP_ID"
    fi
    if [ -n "$NM_TEST_ACPX_CONFIG_AFTER_SETUP" ]; then
      printf '%s\n' "$NM_TEST_ACPX_CONFIG_AFTER_SETUP" > "$NM_TEST_ACPX_CONFIG_FILE"
    fi
    ;;
  *" exec "*|*" prompt "*)
    cat > "$NM_TEST_ACPX_STDIN"
    printf 'PROMPT\n' >> "$NM_TEST_ACPX_LOG"
    if [ -n "$NM_TEST_ACPX_PROMPT_ERROR" ]; then
      printf '{"error":{"message":"%s"}}\n' "$NM_TEST_ACPX_PROMPT_ERROR"
    else
      printf '{"method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"agent_message_chunk","text":"done"}}}\n' "$NM_TEST_ACPX_PROMPT_ID"
    if [ -n "$NM_TEST_ACPX_CONFIG_AFTER_PROMPT" ]; then
      printf '%s\n' "$NM_TEST_ACPX_CONFIG_AFTER_PROMPT" > "$NM_TEST_ACPX_CONFIG_FILE"
    fi
    fi
    ;;
  *" sessions close "*)
    printf '{"action":"session_closed","acpxRecordId":"%s","acpxSessionId":"%s"}\n' "$NM_TEST_ACPX_CLOSE_ID" "$NM_TEST_ACPX_CLOSE_ID"
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAcpxAgent_DurableSessionCommandsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		requested string
	}{
		{name: "fresh named target"},
		{name: "named target resume", requested: "existing-acp"},
		{name: "raw target resume", raw: "fixture", requested: "existing-acp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls")
			t.Setenv("NM_TEST_ACPX_LOG", logPath)
			t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
			id := tc.requested
			if id == "" {
				id = "fresh-acp"
			}
			if tc.raw != "" {
				rawArtifact := filepath.Join(dir, "raw-agent.sh")
				if err := os.WriteFile(rawArtifact, []byte("#!/bin/sh\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				tc.raw = "/bin/sh " + rawArtifact
			}
			t.Setenv("NM_TEST_ACPX_SETUP_ID", id)
			t.Setenv("NM_TEST_ACPX_PROMPT_ID", id)
			t.Setenv("NM_TEST_ACPX_CLOSE_ID", id)
			a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini", rawCommand: tc.raw}
			result, err := a.Run(context.Background(), RunOpts{
				Prompt:  "fix it",
				CWD:     dir,
				Session: &SessionRef{ID: tc.requested, Scope: "run-a/fixer"},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.SessionID != id || result.Resumed != (tc.requested != "") {
				t.Fatalf("identity = %q resumed=%v, want %q resumed=%v", result.SessionID, result.Resumed, id, tc.requested != "")
			}
			logBytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			log := string(logBytes)
			if tc.requested == "" {
				if strings.Count(log, " exec ") != 1 || strings.Contains(log, " sessions ") {
					t.Fatalf("first durable turn must have one target exec and no session-control calls:\n%s", log)
				}
			} else {
				if !strings.Contains(log, "--resume-session "+tc.requested) ||
					strings.Index(log, " sessions new ") > strings.Index(log, " prompt ") ||
					strings.Index(log, " prompt ") > strings.Index(log, " sessions close ") {
					t.Fatalf("wrong resume command sequence:\n%s", log)
				}
				if strings.Contains(log, " sessions show ") {
					t.Fatalf("resume unexpectedly required a pre-existing bridge record:\n%s", log)
				}
			}
		})
	}
}

func TestAcpxAgent_DurableBridgeNamesIsolateScopesAndServingConfig(t *testing.T) {
	dir := t.TempDir()
	a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini", model: "pro"}
	provider := a.staticSessionProvider() + ":" + strings.Repeat("0", sha256.Size*2)
	first := a.sessionName("run-a\x00review-fixer", provider)
	second := a.sessionName("run-b\x00review-fixer", provider)
	if first == second || strings.Contains(first, "run-a") || strings.Contains(second, "run-b") {
		t.Fatalf("opaque bridge names did not isolate two runs sharing HOME: %q/%q", first, second)
	}

	changed := &acpxAgent{bin: a.bin, target: a.target, model: "flash"}
	if a.Name() != changed.Name() {
		t.Fatalf("serving config changed user-facing name: %q != %q", a.Name(), changed.Name())
	}
	if SupportsSessionProvider(changed, provider) {
		t.Fatal("changed serving identity accepted a persisted provider identity")
	}
}

func TestAcpxAgent_NamedConfigChangeRejectsStoredIdentityBeforePrompt(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	t.Setenv("NM_TEST_ACPX_LOG", logPath)
	t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("NM_TEST_ACPX_PROMPT_ID", "stable")
	t.Setenv("NM_TEST_ACPX_CONFIG", `{"agents":{"gemini":{"command":"one"}}}`)
	a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	first, err := a.Run(context.Background(), RunOpts{Prompt: "first", CWD: dir, Session: &SessionRef{Scope: "run/fixer"}})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	t.Setenv("NM_TEST_ACPX_CONFIG", `{"agents":{"gemini":{"command":"two"}}}`)
	_, err = a.Run(context.Background(), RunOpts{
		Prompt: "must not replay",
		CWD:    dir,
		Session: &SessionRef{
			ID:    first.SessionID,
			Agent: first.Provider,
			Scope: "run/fixer",
		},
	})
	if err == nil || !IsSessionSetupFailed(err) {
		t.Fatalf("changed config error = %v, want session setup failure", err)
	}
	logBytes, _ := os.ReadFile(logPath)
	if strings.Count(string(logBytes), "PROMPT\n") != 1 {
		t.Fatalf("config mismatch reached a second target prompt:\n%s", logBytes)
	}
}
func TestAcpxAgent_ConfigChangeDuringSetupStopsBeforePrompt(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"agents":{"gemini":{"command":"one"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_ACPX_LOG", logPath)
	t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("NM_TEST_ACPX_CONFIG_FILE", configPath)
	t.Setenv("NM_TEST_ACPX_CONFIG_AFTER_SETUP", `{"agents":{"gemini":{"command":"two"}}}`)
	t.Setenv("NM_TEST_ACPX_SETUP_ID", "stored-session")
	a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	provider, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
	if err != nil {
		t.Fatalf("initial provider: %v", err)
	}

	_, err = a.Run(context.Background(), RunOpts{
		Prompt: "must not be delivered",
		CWD:    dir,
		Session: &SessionRef{
			ID:    "stored-session",
			Agent: provider,
			Scope: "run/fixer",
		},
	})
	if err == nil || !IsSessionSetupFailed(err) {
		t.Fatalf("changed identity error = %v, want session setup failure", err)
	}
	logBytes, _ := os.ReadFile(logPath)
	log := string(logBytes)
	if strings.Count(log, " sessions new ") != 1 || strings.Contains(log, "PROMPT\n") {
		t.Fatalf("configuration change was not contained at setup:\n%s", log)
	}
}

func TestAcpxAgent_ConfigChangeDuringPromptStopsBeforeClose(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"agents":{"gemini":{"command":"one"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_ACPX_LOG", logPath)
	t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("NM_TEST_ACPX_CONFIG_FILE", configPath)
	t.Setenv("NM_TEST_ACPX_CONFIG_AFTER_PROMPT", `{"agents":{"gemini":{"command":"two"}}}`)
	t.Setenv("NM_TEST_ACPX_SETUP_ID", "stored-session")
	t.Setenv("NM_TEST_ACPX_PROMPT_ID", "stored-session")
	a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	provider, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
	if err != nil {
		t.Fatalf("initial provider: %v", err)
	}

	_, err = a.Run(context.Background(), RunOpts{
		Prompt: "delivered once",
		CWD:    dir,
		Session: &SessionRef{
			ID:    "stored-session",
			Agent: provider,
			Scope: "run/fixer",
		},
	})
	if err == nil || !IsPromptDelivered(err) {
		t.Fatalf("changed identity error = %v, want prompt-delivered failure", err)
	}
	logBytes, _ := os.ReadFile(logPath)
	log := string(logBytes)
	if strings.Count(log, "PROMPT\n") != 1 || strings.Contains(log, " sessions close ") {
		t.Fatalf("configuration change after prompt reached close or replayed:\n%s", log)
	}
}

func TestAcpxAgent_ExecutableEnvironmentAndRawTargetChangesRejectBeforeResume(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    bool
		change func(t *testing.T, acpxPath, rawPath string)
	}{
		{
			name: "same-path acpx replacement",
			change: func(t *testing.T, acpxPath, _ string) {
				replaceExecutableAtSamePath(t, acpxPath)
			},
		},
		{
			name: "effective environment",
			change: func(t *testing.T, _, _ string) {
				t.Setenv("NM_TEST_PROVIDER_ENV", "changed")
			},
		},
		{
			name: "same-path raw executable replacement",
			raw:  true,
			change: func(t *testing.T, _, rawPath string) {
				replaceExecutableAtSamePath(t, rawPath)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls")
			t.Setenv("NM_TEST_ACPX_LOG", logPath)
			t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
			t.Setenv("NM_TEST_ACPX_CONFIG", `{}`)
			t.Setenv("NM_TEST_PROVIDER_ENV", "original")
			acpxPath := writeSessionStubAcpx(t, dir)
			rawPath := filepath.Join(dir, "raw-agent")
			if err := os.WriteFile(rawPath, []byte("#!/bin/sh\n# fingerprint-a\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			a := &acpxAgent{bin: acpxPath, target: "gemini"}
			if tc.raw {
				a.rawCommand = rawPath + " serve"
			}
			provider, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
			if err != nil {
				t.Fatalf("initial provider: %v", err)
			}

			tc.change(t, acpxPath, rawPath)
			_, err = a.Run(context.Background(), RunOpts{
				Prompt: "must not be delivered",
				CWD:    dir,
				Session: &SessionRef{
					ID:    "stored-session",
					Agent: provider,
					Scope: "run/fixer",
				},
			})
			if err == nil || !IsSessionSetupFailed(err) {
				t.Fatalf("changed identity error = %v, want session setup failure", err)
			}
			logBytes, _ := os.ReadFile(logPath)
			if strings.Contains(string(logBytes), " sessions new ") || strings.Contains(string(logBytes), "PROMPT\n") {
				t.Fatalf("identity mismatch reached resume or prompt:\n%s", logBytes)
			}
		})
	}
}

func TestAcpxAgent_SessionProviderUsesEffectiveLastEnvironmentEntry(t *testing.T) {
	dir := t.TempDir()
	a := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	t.Setenv("NM_TEST_ACPX_LOG", filepath.Join(dir, "calls"))
	t.Setenv("NM_TEST_ACPX_CONFIG", `{}`)

	withDuplicate, err := a.resolveSessionProvider(context.Background(), RunOpts{
		CWD: dir,
		Env: []string{"SERVING_PROFILE=A", "SERVING_PROFILE=B"},
	})
	if err != nil {
		t.Fatalf("provider with duplicate environment: %v", err)
	}
	withEffectiveValue, err := a.resolveSessionProvider(context.Background(), RunOpts{
		CWD: dir,
		Env: []string{"SERVING_PROFILE=B"},
	})
	if err != nil {
		t.Fatalf("provider with effective environment: %v", err)
	}
	if withDuplicate != withEffectiveValue {
		t.Fatalf("shadowed environment entry changed provider: duplicate=%q effective=%q", withDuplicate, withEffectiveValue)
	}

	withReversedPrecedence, err := a.resolveSessionProvider(context.Background(), RunOpts{
		CWD: dir,
		Env: []string{"SERVING_PROFILE=B", "SERVING_PROFILE=A"},
	})
	if err != nil {
		t.Fatalf("provider with reversed environment: %v", err)
	}
	if withReversedPrecedence == withDuplicate {
		t.Fatal("different effective environment reused the same provider identity")
	}
}

func TestAcpxAgent_RawExecutableLookupUsesUnixPATHKeySemantics(t *testing.T) {
	dir := t.TempDir()
	trustedDir := filepath.Join(dir, "trusted")
	decoyDir := filepath.Join(dir, "decoy")
	if err := os.MkdirAll(trustedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(decoyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	trustedPath := filepath.Join(trustedDir, "raw-agent")
	decoyPath := filepath.Join(decoyDir, "raw-agent")
	if err := os.WriteFile(trustedPath, []byte("#!/bin/sh\n# fingerprint-a\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoyPath, []byte("#!/bin/sh\n# decoy\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &acpxAgent{
		bin:        writeSessionStubAcpx(t, dir),
		target:     "custom",
		rawCommand: "raw-agent serve",
	}
	opts := RunOpts{
		CWD: dir,
		Env: []string{"PATH=" + trustedDir, "Path=" + decoyDir},
	}
	before, err := a.resolveSessionProvider(context.Background(), opts)
	if err != nil {
		t.Fatalf("initial provider: %v", err)
	}
	replaceExecutableAtSamePath(t, trustedPath)
	after, err := a.resolveSessionProvider(context.Background(), opts)
	if err != nil {
		t.Fatalf("provider after target replacement: %v", err)
	}
	if before == after {
		t.Fatal("replacing the executable selected by PATH did not change provider identity")
	}
}

func TestAcpxAgent_UnresolvableRawCommandIdentityFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := &acpxAgent{
		bin:        writeSessionStubAcpx(t, dir),
		target:     "custom",
		rawCommand: `"unresolvable agent" --serve`,
	}
	_, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
	if err == nil || !strings.Contains(err.Error(), "cannot be fingerprinted exactly") {
		t.Fatalf("provider error = %v, want exact-fingerprint refusal", err)
	}
}

func TestAcpxAgent_RawCommandFileArtifactChangesProviderIdentity(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "provider.mjs")
	if err := os.WriteFile(script, []byte("// fingerprint-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &acpxAgent{
		bin:        writeSessionStubAcpx(t, dir),
		target:     "custom",
		rawCommand: "/bin/sh " + script,
	}
	before, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
	if err != nil {
		t.Fatalf("initial provider: %v", err)
	}
	replaceExecutableAtSamePath(t, script)
	after, err := a.resolveSessionProvider(context.Background(), RunOpts{CWD: dir})
	if err != nil {
		t.Fatalf("changed provider: %v", err)
	}
	if before == after {
		t.Fatal("replacing a raw command file artifact preserved provider identity")
	}
}

func replaceExecutableAtSamePath(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := strings.Replace(string(data), "fingerprint-a", "fingerprint-b", 1)
	if replacement == string(data) {
		t.Fatalf("%s has no replaceable fingerprint marker", path)
	}
	if err := os.WriteFile(path, []byte(replacement), info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestAcpxAgent_PromptErrorIsNotRetriedOrProviderFallback(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	t.Setenv("NM_TEST_ACPX_LOG", logPath)
	t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("NM_TEST_ACPX_SETUP_ID", "session")
	t.Setenv("NM_TEST_ACPX_CLOSE_ID", "session")
	t.Setenv("NM_TEST_ACPX_PROMPT_ERROR", "503 unavailable")
	first := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	second := &fallbackTestAgent{name: "codex", run: func() (*Result, error) { return &Result{Text: "wrong fallback"}, nil }}
	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{Prompt: "once", CWD: dir, Session: &SessionRef{}})
	if err == nil || !IsPromptDelivered(err) {
		t.Fatalf("Run error = %v, want prompt-delivered marker", err)
	}
	logBytes, _ := os.ReadFile(logPath)
	if strings.Count(string(logBytes), "PROMPT\n") != 1 || second.calls != 0 {
		t.Fatalf("prompt count/provider calls = %d/%d:\n%s", strings.Count(string(logBytes), "PROMPT\n"), second.calls, logBytes)
	}
}

func TestAcpxAgent_ColdPromptErrorIsReplaySafe(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	t.Setenv("NM_TEST_ACPX_LOG", logPath)
	t.Setenv("NM_TEST_ACPX_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("NM_TEST_ACPX_PROMPT_ERROR", "503 unavailable")
	first := &acpxAgent{bin: writeSessionStubAcpx(t, dir), target: "gemini"}
	second := &fallbackTestAgent{name: "codex", run: func() (*Result, error) { return &Result{Text: "wrong fallback"}, nil }}
	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{Prompt: "once", CWD: dir})
	if err == nil || !IsPromptDelivered(err) {
		t.Fatalf("Run error = %v, want prompt-delivered marker", err)
	}
	logBytes, _ := os.ReadFile(logPath)
	if strings.Count(string(logBytes), "PROMPT\n") != 1 || second.calls != 0 {
		t.Fatalf("cold prompt replayed: provider calls = %d/%d:\n%s", strings.Count(string(logBytes), "PROMPT\n"), second.calls, logBytes)
	}
}

func writeFakeACPServer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-acp.mjs")
	script := `import fs from "node:fs";
import readline from "node:readline";

const logPath = process.env.NM_TEST_ACP_REQUESTS;
const launchPath = process.env.NM_TEST_ACP_LAUNCHES;
let launch = 1;
try {
  launch = Number(fs.readFileSync(launchPath, "utf8")) + 1;
} catch {}
fs.writeFileSync(launchPath, String(launch));
const mode = process.env.NM_TEST_ACP_MODE;
const supportsLoad = !(mode === "replace" && launch === 2);
const reply = (id, result) => process.stdout.write(JSON.stringify({jsonrpc: "2.0", id, result}) + "\n");
const fail = (id, message) => process.stdout.write(JSON.stringify({jsonrpc: "2.0", id, error: {code: -32001, message}}) + "\n");

readline.createInterface({input: process.stdin}).on("line", (line) => {
  const message = JSON.parse(line);
  if (message.method) {
    fs.appendFileSync(logPath, JSON.stringify({launch, method: message.method, sessionId: message.params?.sessionId}) + "\n");
  }
  switch (message.method) {
    case "initialize":
      reply(message.id, {protocolVersion: 1, agentCapabilities: {loadSession: supportsLoad}, agentInfo: {name: "fake", version: "1"}});
      break;
    case "session/new":
      reply(message.id, {sessionId: supportsLoad ? "stable-session" : "replacement-session"});
      break;
    case "session/load":
      if (mode === "stale") {
        fail(message.id, "stale session");
      } else {
        reply(message.id, {sessionId: message.params.sessionId});
      }
      break;
    case "session/prompt":
      process.stdout.write(JSON.stringify({jsonrpc: "2.0", method: "session/update", params: {sessionId: message.params.sessionId, update: {sessionUpdate: "agent_message_chunk", text: "reply"}}}) + "\n");
      reply(message.id, {stopReason: "end_turn"});
      break;
  }
});
`
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAcpxAgent_RealBridgeSessionContract(t *testing.T) {
	acpx, err := exec.LookPath("acpx")
	if err != nil {
		t.Skip("acpx is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed")
	}

	for _, tc := range []struct {
		name        string
		named       bool
		requested   string
		mode        string
		wantID      string
		wantResumed bool
		wantError   string
	}{
		{name: "raw fresh", wantID: "stable-session"},
		{name: "named fresh", named: true, wantID: "stable-session"},
		{name: "raw resume", requested: "stable-session", wantID: "stable-session", wantResumed: true},
		{name: "setup accepts requested ID then prompt observes replacement", requested: "stable-session", mode: "replace", wantID: "replacement-session"},
		{name: "stale setup stops before prompt", requested: "stable-session", mode: "stale", wantError: "stale session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			workDir := t.TempDir()
			requests := filepath.Join(home, "requests.ndjson")
			launches := filepath.Join(home, "launches")
			t.Setenv("HOME", home)
			t.Setenv("NM_TEST_ACP_REQUESTS", requests)
			t.Setenv("NM_TEST_ACP_LAUNCHES", launches)
			t.Setenv("NM_TEST_ACP_MODE", tc.mode)
			server := writeFakeACPServer(t, home)

			targetCommand := node + " " + server
			a := &acpxAgent{bin: acpx, target: "probe", rawCommand: targetCommand}
			if tc.named {
				configDir := filepath.Join(home, ".acpx")
				if err := os.MkdirAll(configDir, 0o700); err != nil {
					t.Fatal(err)
				}
				config := fmt.Sprintf(`{"agents":{"probe":{"command":%q}},"defaultAgent":"probe"}`, targetCommand)
				if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				a.rawCommand = ""
			}
			scope := "real-bridge/fixer"
			if tc.named && tc.requested != "" {
				first, firstErr := a.Run(context.Background(), RunOpts{Prompt: "seed", CWD: workDir, Session: &SessionRef{Scope: scope}})
				if firstErr != nil || first.SessionID != tc.requested {
					t.Fatalf("seed named record: result=%+v err=%v", first, firstErr)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result, runErr := a.Run(ctx, RunOpts{Prompt: "fix", CWD: workDir, Session: &SessionRef{ID: tc.requested, Scope: scope}})
			requestLog, readErr := os.ReadFile(requests)
			if readErr != nil {
				t.Fatalf("read ACP requests: %v", readErr)
			}
			methods := string(requestLog)
			if tc.wantError != "" {
				if runErr == nil || !strings.Contains(runErr.Error(), tc.wantError) {
					t.Fatalf("Run error = %v, want %q", runErr, tc.wantError)
				}
				if strings.Contains(methods, `"method":"session/prompt"`) {
					t.Fatalf("stale setup delivered prompt:\n%s", methods)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("Run: %v\nrequests:\n%s", runErr, methods)
			}
			if result.Text != "reply" || result.SessionID != tc.wantID || result.Resumed != tc.wantResumed {
				t.Fatalf("result = text %q id %q resumed %v, want reply/%q/%v", result.Text, result.SessionID, result.Resumed, tc.wantID, tc.wantResumed)
			}
			if tc.requested == "" {
				launchCount, readErr := os.ReadFile(launches)
				if readErr != nil {
					t.Fatalf("read ACP launch count: %v", readErr)
				}
				if strings.TrimSpace(string(launchCount)) != "1" {
					t.Fatalf("first durable turn launched ACP target %s times, want 1", strings.TrimSpace(string(launchCount)))
				}
			}
			if !strings.Contains(methods, `"method":"session/prompt"`) {
				t.Fatalf("ACP prompt was not observed:\n%s", methods)
			}
			if tc.requested != "" && !strings.Contains(methods, `"method":"session/load"`) {
				t.Fatalf("ACP resume/load was not observed:\n%s", methods)
			}
			if tc.mode == "replace" && !strings.Contains(methods, `"method":"session/new"`) {
				t.Fatalf("prompt-time replacement after accepted setup was not observed:\n%s", methods)
			}
		})
	}
}

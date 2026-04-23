package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
)

// scrubFile rewrites path in place to replace PII captured during
// recording: the user's home directory, macOS tempdir paths,
// claude SessionStart hook outputs (which expose locally-installed
// axi tools by name).
//
// The substitutions don't affect anything the no-mistakes parser
// actually reads, but they keep personal paths and tool names out of
// fixtures committed to a public repo.
func scrubFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	scrubbed := scrubBytes(data)
	if bytes.Equal(scrubbed, data) {
		return nil
	}
	return os.WriteFile(path, scrubbed, 0o644)
}

func scrubBytes(data []byte) []byte {
	out := data
	out = scrubTempDir(out)
	out = scrubHomeDir(out)
	out = scrubClaudeHookEvents(out)
	return out
}

func scrubHomeDir(data []byte) []byte {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return data
	}
	const placeholder = "/private/tmp/fixture-cwd"
	out := replacePathForms(data, u.HomeDir, placeholder)
	// macOS reports /private/var/... while os.UserHomeDir reports
	// /Users/...; both forms can co-occur in transcripts.
	if resolved, err := filepath.EvalSymlinks(u.HomeDir); err == nil && resolved != u.HomeDir {
		out = replacePathForms(out, resolved, placeholder)
	}
	return out
}

// macOS allocates per-user tempdirs like /var/folders/5x/<id>/T/foo and
// references them as /private/var/folders/<id>/T/foo. The folder ID
// fingerprints the user account, so collapse it.
var macTempPattern = regexp.MustCompile(`/(?:private/)?var/folders/[^"\s/]+/[^"\s/]+/T`)

func scrubTempDir(data []byte) []byte {
	out := macTempPattern.ReplaceAll(data, []byte("/tmp"))
	return replacePathForms(out, os.TempDir(), "/tmp")
}

func replacePathForms(data []byte, path, placeholder string) []byte {
	if path == "" {
		return data
	}
	out := bytes.ReplaceAll(data, []byte(path), []byte(placeholder))
	pathJSON, err := json.Marshal(path)
	if err != nil {
		return out
	}
	placeholderJSON, err := json.Marshal(placeholder)
	if err != nil {
		return out
	}
	if len(pathJSON) >= 2 && len(placeholderJSON) >= 2 {
		out = bytes.ReplaceAll(out, pathJSON[1:len(pathJSON)-1], placeholderJSON[1:len(placeholderJSON)-1])
	}
	return out
}

// scrubClaudeHookEvents drops claude's SessionStart system events, which
// dump the user's locally-installed axi tools (terminal-axi, etc.) into
// `output`/`stdout` fields. The no-mistakes parser ignores type=system
// entirely, so removing these lines doesn't affect e2e wire-format
// coverage. The init event (subtype=init) carries information about
// available tools/skills that's also user-specific, so we drop it too.
func scrubClaudeHookEvents(data []byte) []byte {
	var out bytes.Buffer
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			continue
		}
		// Match `"type":"system",` (with optional spaces) at the
		// start of the JSON object - substring match is good enough
		// because the field always appears early in real claude
		// stream-json output.
		if bytes.Contains(line, []byte(`"type":"system"`)) || bytes.Contains(line, []byte(`"subtype":"init"`)) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	// Drop the trailing extra newline introduced by Split when the
	// input already ends with one.
	scrubbed := out.Bytes()
	if len(scrubbed) > 0 && scrubbed[len(scrubbed)-1] == '\n' && len(data) > 0 && data[len(data)-1] != '\n' {
		scrubbed = scrubbed[:len(scrubbed)-1]
	}
	return scrubbed
}

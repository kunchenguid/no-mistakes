package daemon

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func serviceDefinitionMatchesRoot(data []byte, p *paths.Paths) bool {
	if len(data) == 0 {
		return false
	}
	if p == nil {
		return true
	}
	root := p.Root()
	text := string(data)
	if strings.Contains(text, "<string>"+xmlEscaped(root)+"</string>") {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "WorkingDirectory="+systemdEscapeArg(root) {
			return true
		}
	}
	windowsRoot := quoteWindowsTaskArg(root)
	for _, suffix := range []string{
		"--root " + windowsRoot + "</Arguments>",
		"--root " + xmlEscaped(windowsRoot) + "</Arguments>",
	} {
		if strings.Contains(text, suffix) {
			return true
		}
	}
	return false
}

func xmlEscaped(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func launchAgentExecutable(data []byte) (string, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var sawProgramArguments bool
	var inProgramArguments bool
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var key string
				if err := decoder.DecodeElement(&key, &t); err != nil {
					return "", false
				}
				sawProgramArguments = strings.TrimSpace(key) == "ProgramArguments"
			case "array":
				if sawProgramArguments {
					inProgramArguments = true
					sawProgramArguments = false
				}
			case "string":
				if !inProgramArguments {
					sawProgramArguments = false
					continue
				}
				var value string
				if err := decoder.DecodeElement(&value, &t); err != nil {
					return "", false
				}
				if strings.TrimSpace(value) == "" {
					return "", false
				}
				return value, true
			default:
				if !inProgramArguments {
					sawProgramArguments = false
				}
			}
		case xml.EndElement:
			if inProgramArguments && t.Name.Local == "array" {
				return "", false
			}
		}
	}
}

func systemdUnitExecutable(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		return firstCommandArg(strings.TrimSpace(strings.TrimPrefix(line, "ExecStart=")))
	}
	return "", false
}

func firstCommandArg(command string) (string, bool) {
	if command == "" {
		return "", false
	}
	if command[0] != '"' {
		fields := strings.Fields(command)
		if len(fields) == 0 || fields[0] == "" {
			return "", false
		}
		return fields[0], true
	}
	escaped := false
	for i := 1; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			value, err := strconv.Unquote(command[:i+1])
			if err != nil || value == "" {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

func runServiceCommand(name string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", path, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

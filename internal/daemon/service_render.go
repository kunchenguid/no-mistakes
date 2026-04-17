package daemon

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os/exec"
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

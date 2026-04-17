package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestServiceDefinitionMatchesRootRejectsPrefixOnlyMatch(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "nm")
	otherRoot := root + "-prod"
	home := t.TempDir()
	p := paths.WithRoot(root)
	other := paths.WithRoot(otherRoot)

	if serviceDefinitionMatchesRoot([]byte(renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", other, home)), p) {
		t.Fatal("expected launch agent root prefix collision to be rejected")
	}
	if serviceDefinitionMatchesRoot([]byte(renderSystemdUnit("/usr/local/bin/no-mistakes", other, home)), p) {
		t.Fatal("expected systemd root prefix collision to be rejected")
	}
	if serviceDefinitionMatchesRoot([]byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>`+buildWindowsTaskCommand(`C:\nm.exe`, otherRoot)+`</Arguments></Exec></Task>`), p) {
		t.Fatal("expected windows task root prefix collision to be rejected")
	}

	if !serviceDefinitionMatchesRoot([]byte(renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", p, home)), p) {
		t.Fatal("expected exact launch agent root match")
	}
	if !serviceDefinitionMatchesRoot([]byte(renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)), p) {
		t.Fatal("expected exact systemd root match")
	}
	if !serviceDefinitionMatchesRoot([]byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>`+buildWindowsTaskCommand(`C:\nm.exe`, root)+`</Arguments></Exec></Task>`), p) {
		t.Fatal("expected exact windows task root match")
	}
}

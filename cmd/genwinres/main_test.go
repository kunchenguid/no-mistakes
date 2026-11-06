package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/winres"
)

func TestGenwinresWritesObjectsThatPassCheck(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(winres.ObjectDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(nil, root, &stdout, &stderr); code != 0 {
		t.Fatalf("write exit = %d, stderr:\n%s", code, stderr.String())
	}
	for _, target := range winres.Targets {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.ObjectPath())))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, target.Object()) {
			t.Fatalf("%s does not hold the rendered object", target.ObjectPath())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--check"}, root, &stdout, &stderr); code != 0 {
		t.Fatalf("check exit = %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is up to date") {
		t.Fatalf("check stdout = %q", stdout.String())
	}
}

func TestGenwinresCheckFailsOnDriftWithoutRewriting(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(winres.ObjectDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run(nil, root, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("write exit = %d", code)
	}
	drifted := winres.Targets[0]
	driftedPath := filepath.Join(root, filepath.FromSlash(drifted.ObjectPath()))
	stale := append(drifted.Object(), 0)
	if err := os.WriteFile(driftedPath, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := winres.Targets[1]
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(missing.ObjectPath()))); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if code := run([]string{"--check"}, root, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("check exit = %d, want 1; stderr:\n%s", code, stderr.String())
	}
	want := filepath.FromSlash(drifted.ObjectPath()) + " is stale; run `go run ./cmd/genwinres` and commit the result"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("check stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if !strings.Contains(stderr.String(), "read "+filepath.FromSlash(missing.ObjectPath())) {
		t.Fatalf("check stderr = %q, want the missing object reported", stderr.String())
	}
	got, err := os.ReadFile(driftedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stale) {
		t.Fatal("--check rewrote the stale object")
	}
}

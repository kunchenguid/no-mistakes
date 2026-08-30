package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicationMachineCommandsDoNotRunAutomaticUpdateChecks(t *testing.T) {
	originalArgs := os.Args
	originalStdin := os.Stdin
	originalCleanup := cleanupOldExecutable
	originalBackground := maybeHandleBackgroundCheck
	originalNotify := maybeNotifyAndCheck
	t.Cleanup(func() {
		os.Args = originalArgs
		os.Stdin = originalStdin
		cleanupOldExecutable = originalCleanup
		maybeHandleBackgroundCheck = originalBackground
		maybeNotifyAndCheck = originalNotify
	})

	cleanupOldExecutable = func() error { return nil }
	maybeHandleBackgroundCheck = func([]string) (bool, error) { return false, nil }
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	os.Stdin = readEnd

	for _, verb := range []string{"start", "authorize", "status"} {
		t.Run(verb, func(t *testing.T) {
			called := 0
			maybeNotifyAndCheck = func([]string, io.Writer) { called++ }
			os.Args = []string{"no-mistakes", "publication", verb}
			_ = run() // Empty stdin must fail closed; this test owns only update isolation.
			if called != 0 {
				t.Fatalf("publication %s invoked the automatic update checker %d times", verb, called)
			}
		})
	}
}

func TestPublicationConfinementCanaryDispatchIsPrivateStrictAndFullyBound(t *testing.T) {
	if _, handled, err := publicationConfinementCanaryFromArgs([]string{"publication", "start"}); err != nil || handled {
		t.Fatalf("ordinary publication command treated as canary: handled=%v err=%v", handled, err)
	}
	if publicationMachineCommand([]string{"__publication-confinement-canary"}) {
		t.Fatal("private confinement canary was exposed as a publication workflow command")
	}
	if _, handled, err := publicationConfinementCanaryFromArgs([]string{"__publication-confinement-canary"}); !handled || err == nil {
		t.Fatalf("malformed private canary handled=%v err=%v, want strict failure", handled, err)
	}

	root := t.TempDir()
	want := publicationConfinementCanaryInvocation{
		ScratchDir:  filepath.Join(root, "scratch"),
		ReadyMarker: filepath.Join(root, "ready"),
		LateMarker:  filepath.Join(root, "late"),
		Delay:       750 * time.Millisecond,
	}
	args := []string{
		"__publication-confinement-canary",
		"--scratch", want.ScratchDir,
		"--ready", want.ReadyMarker,
		"--late", want.LateMarker,
		"--delay-ms", "750",
	}
	got, handled, err := publicationConfinementCanaryFromArgs(args)
	if err != nil || !handled {
		t.Fatalf("parse bound private canary: handled=%v err=%v", handled, err)
	}
	if got != want {
		t.Fatalf("private canary invocation=%#v, want %#v", got, want)
	}
}

func TestPublicationConfinementDetachedChildDispatchIsPrivateStrictAndFullyBound(t *testing.T) {
	if _, handled, err := publicationConfinementDetachedChildFromArgs([]string{"publication", "start"}); err != nil || handled {
		t.Fatalf("ordinary publication command treated as detached child: handled=%v err=%v", handled, err)
	}
	if publicationMachineCommand([]string{"__publication-confinement-detached-child"}) {
		t.Fatal("private detached child was exposed as a publication workflow command")
	}
	if _, handled, err := publicationConfinementDetachedChildFromArgs([]string{"__publication-confinement-detached-child"}); !handled || err == nil {
		t.Fatalf("malformed detached child handled=%v err=%v, want strict failure", handled, err)
	}
	root := t.TempDir()
	want := publicationConfinementDetachedChildInvocation{
		ReadyMarker: filepath.Join(root, "ready"),
		LateMarker:  filepath.Join(root, "late"),
		Delay:       800 * time.Millisecond,
	}
	args := []string{
		"__publication-confinement-detached-child",
		"--ready", want.ReadyMarker,
		"--late", want.LateMarker,
		"--delay-ms", "800",
	}
	got, handled, err := publicationConfinementDetachedChildFromArgs(args)
	if err != nil || !handled {
		t.Fatalf("parse bound private detached child: handled=%v err=%v", handled, err)
	}
	if got != want {
		t.Fatalf("private detached child invocation=%#v, want %#v", got, want)
	}
}

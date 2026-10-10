// receive-recorder enrolls real hooks pinned to this native executable, then
// records their CLI calls without starting a daemon or pipeline.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 3 && os.Args[1] == "enroll" {
		if err := git.RefreshManagedGateHooks(os.Args[2]); err != nil {
			return err
		}
		return git.MarkGateConfigCurrent(os.Args[2])
	}
	if len(os.Args) == 3 && os.Args[1] == "current" {
		if !git.GateConfigCurrent(os.Args[2]) {
			return fmt.Errorf("gate config does not match enrolled owner")
		}
		return nil
	}
	if len(os.Args) < 3 || os.Args[1] != "daemon" || (os.Args[2] != "admit-push" && os.Args[2] != "notify-push") {
		return fmt.Errorf("unexpected CLI arguments: %q", os.Args[1:])
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(os.Getenv("NM_RECEIVE_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(struct {
		Args       []string `json:"args"`
		Home       string   `json:"home"`
		Helper     string   `json:"helper"`
		Executable string   `json:"executable"`
	}{os.Args[1:], os.Getenv("NM_HOME"), os.Getenv("NM_HOOK_HELPER"), exe})
}

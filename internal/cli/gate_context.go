package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

type olderDaemonContextKey struct{}

// guardGateControl is the one CLI policy for commands that can mutate pipeline
// ownership. It runs before command-specific resource opening, flag-driven
// auto-approval, daemon admission, or any Git/DB mutation.
func guardGateControl(cmd *cobra.Command) error {
	if !mutatesPipelineControl(cmd) {
		return nil
	}
	path := cmd.CommandPath()
	allowOlder := path == "no-mistakes daemon start" || path == "no-mistakes daemon stop" ||
		path == "no-mistakes daemon restart" || path == "no-mistakes daemon uninstall" ||
		path == "no-mistakes update"
	result, older, err := classifyGateControlCaller(cmd.Context(), allowOlder)
	if err != nil {
		return err
	}
	if !result.Nested {
		if older {
			cmd.SetContext(context.WithValue(cmd.Context(), olderDaemonContextKey{}, true))
			fmt.Fprintln(cmd.ErrOrStderr(), "Running daemon predates gate_context; checked gate execution context locally.")
		}
		return nil
	}
	return emitGateContextRefusal(cmd, result)
}

func mutatesPipelineControl(cmd *cobra.Command) bool {
	path := cmd.CommandPath()
	switch path {
	case "no-mistakes", "no-mistakes init", "no-mistakes eject", "no-mistakes rerun",
		"no-mistakes axi run", "no-mistakes axi respond", "no-mistakes axi abort", "no-mistakes axi retry",
		"no-mistakes daemon start", "no-mistakes daemon stop", "no-mistakes daemon restart",
		"no-mistakes daemon uninstall",
		"no-mistakes update":
		return true
	case "no-mistakes sync", "no-mistakes axi sync":
		check, err := cmd.Flags().GetBool("check")
		return err != nil || !check
	default:
		return false
	}
}

func classifyGateControlCaller(ctx context.Context, allowOlder bool) (gatecontext.Result, bool, error) {
	p, err := paths.New()
	if err != nil {
		return gatecontext.Result{}, false, fmt.Errorf("resolve paths: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return gatecontext.Result{}, false, fmt.Errorf("resolve current directory: %w", err)
	}
	marker := gatecontext.MarkerPresent()
	older := false
	daemonPID := 0
	alive, runningErr := daemon.IsRunning(p)
	if alive {
		if runningErr != nil {
			return gatecontext.Result{}, false, fmt.Errorf("inspect daemon for gate execution context: %w", runningErr)
		}
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			return gatecontext.Result{}, false, fmt.Errorf("connect to daemon for gate execution context: %w", err)
		}
		defer client.Close()
		var wire ipc.GateContextResult
		if err := client.Call(ipc.MethodGateContext, &ipc.GateContextParams{CWD: cwd, MarkerPresent: marker}, &wire); err != nil {
			var rpcErr *ipc.RPCError
			if !allowOlder || !errors.As(err, &rpcErr) || rpcErr.Code != ipc.ErrMethodNotFound {
				return gatecontext.Result{}, false, fmt.Errorf("classify gate execution context: %w", err)
			}
			older = true
			daemonPID, err = daemon.ReadPID(p)
			if err != nil {
				return gatecontext.Result{}, false, fmt.Errorf("classify older daemon gate execution context: read daemon PID: %w", err)
			}
			if daemonPID <= 0 {
				return gatecontext.Result{}, false, fmt.Errorf("classify older daemon gate execution context: invalid daemon PID")
			}
		} else {
			return gatecontext.Result{
				Nested:           wire.Nested,
				ManagedGit:       wire.ManagedGit,
				AgentDescendant:  wire.AgentDescendant,
				DaemonDescendant: wire.DaemonDescendant,
				MarkerPresent:    wire.MarkerPresent,
				RunID:            wire.RunID,
				Phase:            wire.Phase,
			}, false, nil
		}
	}
	// A stale/non-socket endpoint can make IsRunning return an error together
	// with alive=false. There is no authenticated peer to query in that state;
	// continue with canonical read-only topology so lifecycle repair commands
	// retain their existing compatibility.

	// An older daemon cannot classify the caller over IPC. Use the same
	// inspector locally, including our process ancestry, without migrating or
	// writing the database. When no daemon is active, only topology is needed.
	var database *db.DB
	if opened, openErr := db.OpenReadOnly(p.DB()); openErr == nil {
		database = opened
		defer database.Close()
	} else if !os.IsNotExist(openErr) {
		return gatecontext.Result{}, false, fmt.Errorf("open gate registry read-only: %w", openErr)
	}
	request := gatecontext.Request{CWD: cwd, MarkerPresent: marker}
	if older {
		request.PeerPID = os.Getpid()
		request.DaemonPID = daemonPID
	}
	result, err := (gatecontext.Inspector{DB: database, Paths: p}).Inspect(ctx, request)
	return result, older, err
}

func emitGateContextRefusal(cmd *cobra.Command, result gatecontext.Result) error {
	errorFields := []toon.Field{
		{Key: "code", Value: gatecontext.ErrorCode},
		{Key: "message", Value: "refusing pipeline control from an active no-mistakes validation step"},
	}
	if result.RunID != "" {
		errorFields = append(errorFields, toon.Field{Key: "run", Value: result.RunID})
	}
	if result.Phase != "" {
		errorFields = append(errorFields, toon.Field{Key: "phase", Value: string(result.Phase)})
	}
	allowed := []string{"no-mistakes axi status", "no-mistakes axi logs --step <phase>", "no-mistakes doctor"}
	if result.RunID != "" {
		allowed[0] = "no-mistakes axi status --run " + result.RunID
		allowed[1] = "no-mistakes axi logs --run " + result.RunID + " --step <phase>"
	}
	emitDoc(cmd,
		toon.Field{Key: "error", Value: toon.NewObject(errorFields...)},
		toon.Field{Key: "note", Value: "The enclosing executor owns validation, push, PR, and CI. This step must return only its assigned phase."},
		toon.Field{Key: "allowed", Value: allowed},
		toon.Field{Key: "help", Value: []string{"Return control to the outer executor; do not initialize, start, reattach, rerun, respond to, synchronize, abort, or eject a pipeline from this step."}},
	)
	return &exitError{code: 1}
}

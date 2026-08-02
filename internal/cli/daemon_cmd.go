package cli

import (
	"bufio"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.Stop
	daemonIsRunningFn = daemon.IsRunning
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonReceivePackCmd())
	cmd.AddCommand(newDaemonAdmitPushCmd())
	cmd.AddCommand(newDaemonReceiveTransactionCmd())
	cmd.AddCommand(newDaemonAuthorizeRefMutationCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonAuthorizeRefMutationCmd() *cobra.Command {
	var gate, authority, phase, branch, ref, oldSHA, newSHA, operation, scope string
	cmd := &cobra.Command{
		Use:    "authorize-ref-mutation",
		Short:  "Authorize one exact internal managed-gate ref transaction",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			capability := strings.TrimSpace(os.Getenv("NO_MISTAKES_INTERNAL_MUTATION_CAPABILITY"))
			if capability == "" {
				return fmt.Errorf("internal mutation capability is required")
			}
			if strings.TrimSpace(authority) == "" {
				return fmt.Errorf("live internal mutation authority is required")
			}
			return branchsync.AuthorizeInternalRefMutation(authority, branchsync.InternalRefMutationAuthorization{
				Capability: capability,
				Phase:      phase,
				GatePath:   gatePath,
				Branch:     branch,
				Ref:        ref,
				OldSHA:     oldSHA,
				NewSHA:     newSHA,
				Operation:  operation,
				Scope:      scope,
			})
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "managed bare gate path")
	cmd.Flags().StringVar(&authority, "authority", "", "live branch-lock authority endpoint")
	cmd.Flags().StringVar(&phase, "phase", "", "reference transaction phase")
	cmd.Flags().StringVar(&branch, "branch", "", "exact branch identity")
	cmd.Flags().StringVar(&ref, "ref", "", "exact ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "exact old object ID")
	cmd.Flags().StringVar(&newSHA, "new", "", "exact new object ID")
	cmd.Flags().StringVar(&operation, "operation", "", "exact internal operation")
	cmd.Flags().StringVar(&scope, "scope", "", "ordinary or private ref scope")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("authority")
	_ = cmd.MarkFlagRequired("phase")
	_ = cmd.MarkFlagRequired("branch")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")
	_ = cmd.MarkFlagRequired("operation")
	_ = cmd.MarkFlagRequired("scope")
	return cmd
}

func newDaemonReceivePackCmd() *cobra.Command {
	var gate string
	cmd := &cobra.Command{
		Use:    "receive-pack <gate> [options]",
		Short:  "Run the managed git receive-pack boundary",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runManagedReceivePack(cmd, gate, args)
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "managed gate path")
	_ = cmd.MarkFlagRequired("gate")
	return cmd
}

func runManagedReceivePack(cmd *cobra.Command, gate string, args []string) (retErr error) {
	gatePath, err := normalizeNotifyGatePath(gate)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("receive-pack repository is required")
	}
	if !sameCLIPath(args[0], gatePath) {
		return fmt.Errorf("receive-pack repository does not match managed gate")
	}
	p, err := paths.New()
	if err != nil {
		return err
	}
	repoID, err := receiveRepoID(gatePath)
	if err != nil {
		return err
	}
	if !sameCLIPath(gatePath, p.RepoDir(repoID)) {
		return fmt.Errorf("managed gate path does not match no-mistakes repository")
	}
	if !git.LooksLikeBareRepository(gatePath) || !git.GateConfigCurrent(gatePath) {
		return fmt.Errorf("managed gate fencing configuration is missing or tampered")
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return fmt.Errorf("open receive database: %w", err)
	}
	defer database.Close()
	repo, err := database.GetRepo(repoID)
	if err != nil {
		return fmt.Errorf("get receive repository: %w", err)
	}
	if repo == nil {
		return fmt.Errorf("unknown repo for gate %s", gatePath)
	}
	sessionID, capability, err := newReceiveSessionCredentials()
	if err != nil {
		return fmt.Errorf("create receive session: %w", err)
	}
	if err := database.RegisterReceiveSession(repo.ID, gatePath, sessionID, capability); err != nil {
		return err
	}
	defer func() {
		if err := database.RetireReceiveSession(sessionID); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "no-mistakes: receive session retained for reconciliation: %v\n", err)
		}
	}()
	transport, err := newReceiveCapabilityTransport(gatePath, capability)
	if err != nil {
		return fmt.Errorf("create receive capability transport: %w", err)
	}
	defer func() {
		if err := transport.Close(); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "no-mistakes: receive capability cleanup deferred: %v\n", err)
		}
	}()

	manifest, err := os.CreateTemp(gatePath, ".no-mistakes-receive-manifest.XXXXXX")
	if err != nil {
		return fmt.Errorf("create receive manifest: %w", err)
	}
	manifestPath := manifest.Name()
	defer os.Remove(manifestPath)
	if err := manifest.Chmod(0o600); err != nil {
		_ = manifest.Close()
		return fmt.Errorf("protect receive manifest: %w", err)
	}
	if err := manifest.Close(); err != nil {
		return fmt.Errorf("close receive manifest: %w", err)
	}

	receiveArgs := append([]string{gatePath}, args[1:]...)
	child := exec.CommandContext(cmd.Context(), "git-receive-pack", receiveArgs...)
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	transport.Configure(child)
	child.Env = append(os.Environ(), "NO_MISTAKES_RECEIVE_SESSION_ID="+sessionID, "NO_MISTAKES_RECEIVE_MANIFEST="+manifestPath)
	child.Env = append(child.Env, transport.Env()...)
	child.Env = git.SanitizedGateConfigEnvFrom(child.Env, gatePath)
	shellenv.ConfigureShellCommand(child)
	return shellenv.RunShellCommand(child)
}

func newReceiveSessionCredentials() (string, string, error) {
	read := func() (string, error) {
		value := make([]byte, 32)
		if _, err := cryptorand.Read(value); err != nil {
			return "", err
		}
		return hex.EncodeToString(value), nil
	}
	sessionID, err := read()
	if err != nil {
		return "", "", err
	}
	capability, err := read()
	if err != nil {
		return "", "", err
	}
	return sessionID, capability, nil
}

func receiveRepoID(gate string) (string, error) {
	base := filepath.Base(gate)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gate)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

func sameCLIPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func newDaemonReceiveTransactionCmd() *cobra.Command {
	var gate, phase, reservationID, ref, oldSHA, newSHA, receiveSessionID string
	var receiveCapabilityFD int = -1
	var receiveCapabilityHandle string
	cmd := &cobra.Command{
		Use:    "receive-transaction",
		Short:  "Record an authoritative git receive transaction phase",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			receiveCapability, err := readReceiveCapability(receiveCapabilityFD, receiveCapabilityHandle)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			allFields := reservationID != "" && ref != "" && oldSHA != "" && newSHA != ""
			anyFields := reservationID != "" || ref != "" || oldSHA != "" || newSHA != ""
			if anyFields && !allFields {
				return fmt.Errorf("receive transaction requires reservation, ref, old, and new together")
			}
			updates := make([]ipc.ReceiveTransactionUpdate, 0, 1)
			if allFields {
				updates = append(updates, ipc.ReceiveTransactionUpdate{ReservationID: reservationID, Ref: ref, Old: oldSHA, New: newSHA})
			} else {
				scanner := bufio.NewScanner(cmd.InOrStdin())
				seen := false
				for scanner.Scan() {
					fields := strings.Fields(scanner.Text())
					if len(fields) == 0 {
						continue
					}
					if len(fields) != 4 {
						return fmt.Errorf("receive transaction input must contain reservation, old, new, and ref")
					}
					seen = true
					updates = append(updates, ipc.ReceiveTransactionUpdate{ReservationID: fields[0], Ref: fields[3], Old: fields[1], New: fields[2]})
				}
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("read receive transaction input: %w", err)
				}
				if !seen && phase != "aborted" {
					return fmt.Errorf("receive transaction input is empty")
				}
			}
			return client.Call(ipc.MethodReceiveTxnBatch, &ipc.ReceiveTransactionBatchParams{Gate: gatePath, Phase: phase, Updates: updates, ReceiveSessionID: receiveSessionID, ReceiveCapability: receiveCapability}, nil)
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path for the receive transaction")
	cmd.Flags().StringVar(&phase, "phase", "", "git reference-transaction phase")
	cmd.Flags().StringVar(&reservationID, "reservation-id", "", "exact receive reservation identity")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringVar(&receiveSessionID, "receive-session-id", "", "authenticated receive session identity")
	cmd.Flags().IntVar(&receiveCapabilityFD, "receive-capability-fd", -1, "protected descriptor containing the receive capability")
	cmd.Flags().StringVar(&receiveCapabilityHandle, "receive-capability-handle", "", "protected inherited handle containing the receive capability")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("phase")
	_ = cmd.MarkFlagRequired("receive-session-id")
	return cmd
}

func newDaemonAdmitPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var receiveSessionID string
	var receiveCapabilityFD int = -1
	var receiveCapabilityHandle string
	var pushOptions []string
	cmd := &cobra.Command{
		Use:    "admit-push",
		Short:  "Authorize a managed gate ref update",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			receiveCapability, err := readReceiveCapability(receiveCapabilityFD, receiveCapabilityHandle)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			allFields := ref != "" && oldSHA != "" && newSHA != ""
			anyFields := ref != "" || oldSHA != "" || newSHA != ""
			if anyFields && !allFields {
				return fmt.Errorf("admit push requires ref, old, and new together")
			}
			updates := make([]ipc.AdmitPushUpdate, 0, 1)
			values := make([][]string, 0, 1)
			if allFields {
				updates = append(updates, ipc.AdmitPushUpdate{Ref: ref, Old: oldSHA, New: newSHA, SkipSteps: skipSteps, Intent: intent})
				values = append(values, []string{oldSHA, newSHA, ref})
			} else {
				scanner := bufio.NewScanner(cmd.InOrStdin())
				seen := false
				for scanner.Scan() {
					fields := strings.Fields(scanner.Text())
					if len(fields) == 0 {
						continue
					}
					if len(fields) != 3 {
						return fmt.Errorf("admit push input must contain old, new, and ref")
					}
					seen = true
					updates = append(updates, ipc.AdmitPushUpdate{Ref: fields[2], Old: fields[0], New: fields[1], SkipSteps: skipSteps, Intent: intent})
					values = append(values, []string{fields[0], fields[1], fields[2]})
				}
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("read admit push input: %w", err)
				}
				if !seen {
					return fmt.Errorf("admit push input is empty")
				}
			}
			var result ipc.AdmitPushBatchResult
			if err := client.Call(ipc.MethodAdmitPushBatch, &ipc.AdmitPushBatchParams{Gate: gatePath, Updates: updates, ReceiveSessionID: receiveSessionID, ReceiveCapability: receiveCapability}, &result); err != nil {
				return err
			}
			if result.Context.Nested {
				return emitGateContextRefusal(cmd, gatecontext.Result{Nested: result.Context.Nested, ManagedGit: result.Context.ManagedGit, AgentDescendant: result.Context.AgentDescendant, DaemonDescendant: result.Context.DaemonDescendant, MarkerPresent: result.Context.MarkerPresent, RunID: result.Context.RunID, Phase: result.Context.Phase})
			}
			if len(result.ReservationIDs) != len(values) {
				return fmt.Errorf("admit push returned %d reservations for %d updates", len(result.ReservationIDs), len(values))
			}
			for i, reservationID := range result.ReservationIDs {
				if reservationID == "" {
					return fmt.Errorf("admit push returned no receive reservation")
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %s\n", reservationID, values[i][0], values[i][1], values[i][2])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that is about to receive a push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringVar(&receiveSessionID, "receive-session-id", "", "authenticated receive session identity")
	cmd.Flags().IntVar(&receiveCapabilityFD, "receive-capability-fd", -1, "protected descriptor containing the receive capability")
	cmd.Flags().StringVar(&receiveCapabilityHandle, "receive-capability-handle", "", "protected inherited handle containing the receive capability")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("receive-session-id")
	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var receiveSessionID string
	var receiveCapabilityFD int = -1
	var receiveCapabilityHandle string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}

			p, err := paths.New()
			if err != nil {
				return err
			}

			receiveCapability, err := readReceiveCapability(receiveCapabilityFD, receiveCapabilityHandle)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			allFields := ref != "" && oldSHA != "" && newSHA != ""
			anyFields := ref != "" || oldSHA != "" || newSHA != ""
			if anyFields && !allFields {
				return fmt.Errorf("push notification requires ref, old, and new together")
			}
			notify := func(values []string) error {
				var result ipc.PushReceivedResult
				return client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gatePath, Ref: values[0], Old: values[1], New: values[2], SkipSteps: skipSteps, Intent: intent, ReceiveSessionID: receiveSessionID, ReceiveCapability: receiveCapability}, &result)
			}
			if allFields {
				return notify([]string{ref, oldSHA, newSHA})
			}
			scanner := bufio.NewScanner(cmd.InOrStdin())
			seen := false
			var failures []string
			for scanner.Scan() {
				fields := strings.Fields(scanner.Text())
				if len(fields) == 0 {
					continue
				}
				if len(fields) != 3 {
					return fmt.Errorf("push notification input must contain old, new, and ref")
				}
				seen = true
				if err := notify([]string{fields[2], fields[0], fields[1]}); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", fields[2], err))
				}
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read push notification input: %w", err)
			}
			if !seen {
				return fmt.Errorf("push notification input is empty")
			}
			if len(failures) > 0 {
				return fmt.Errorf("push notification batch failed: %s", strings.Join(failures, "; "))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringVar(&receiveSessionID, "receive-session-id", "", "authenticated receive session identity")
	cmd.Flags().IntVar(&receiveCapabilityFD, "receive-capability-fd", -1, "protected descriptor containing the receive capability")
	cmd.Flags().StringVar(&receiveCapabilityHandle, "receive-capability-handle", "", "protected inherited handle containing the receive capability")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("receive-session-id")

	return cmd
}

func readReceiveCapability(fd int, handle string) (string, error) {
	var file *os.File
	var err error
	if strings.TrimSpace(handle) != "" {
		file, err = readReceiveCapabilityHandle(handle)
	} else if fd >= 0 {
		file = os.NewFile(uintptr(fd), "receive-capability")
	} else {
		return "", fmt.Errorf("receive capability transport is required")
	}
	if err != nil {
		return "", err
	}
	if file == nil {
		return "", fmt.Errorf("receive capability transport is invalid")
	}
	defer file.Close()
	_, _ = file.Seek(0, 0)
	value, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read receive capability: %w", err)
	}
	capability := strings.TrimSpace(string(value))
	if capability == "" || strings.IndexFunc(capability, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return "", fmt.Errorf("receive capability is invalid")
	}
	return capability, nil
}

func normalizeNotifyGatePath(gate string) (string, error) {
	if strings.TrimSpace(gate) == "" {
		return "", fmt.Errorf("gate path is required")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return "", fmt.Errorf("resolve gate path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// intentPushOptionPrefix carries an agent-supplied intent through a git push.
// The value is base64-encoded so multi-line or special-character intents
// survive the push-option transport (which is line-oriented).
const intentPushOptionPrefix = "no-mistakes.intent="

// formatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func formatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return intentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

// parseIntentPushOptions extracts and decodes the intent push option, if any.
// The last occurrence wins.
func parseIntentPushOptions(options []string) (string, error) {
	intent := ""
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, intentPushOptionPrefix)
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode intent push option: %w", err)
		}
		intent = string(decoded)
	}
	return intent, nil
}

func formatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{"no-mistakes.skip=" + strings.Join(parts, ",")}
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force)
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when pipeline runs are active")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force)
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when pipeline runs are active")
	return cmd
}

func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, force bool) error {
	runs, err := lifecycle.ActiveRuns(p)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}
	if force {
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline runs are in progress\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.RunList(runs))
		return nil
	}
	return fmt.Errorf("refusing %s because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(runs), lifecycle.RunList(runs))
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}

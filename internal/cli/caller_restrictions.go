package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

const callerRestrictionsPushPrefix = "no-mistakes.caller-restrictions="

func bindCallerRestrictionsFlag(cmd *cobra.Command) {
	cmd.Flags().String("caller-restrictions", "", "capture caller action restrictions from a UTF-8 file as immutable run instructions (new runs only)")
}

func callerRestrictionsRequested(cmd *cobra.Command) bool {
	return cmd.Flags().Lookup("caller-restrictions") != nil && cmd.Flags().Changed("caller-restrictions")
}

func captureCallerRestrictions(cmd *cobra.Command) (*callerrestrictions.Record, error) {
	if !callerRestrictionsRequested(cmd) {
		return nil, nil
	}
	path, _ := cmd.Flags().GetString("caller-restrictions")
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--caller-restrictions requires a file path")
	}
	return callerrestrictions.Capture(path)
}

func requireDaemonCallerRestrictions(client interface{ Call(string, any, any) error }, r *callerrestrictions.Record) error {
	if r == nil {
		return nil
	}
	if err := r.Validate(); err != nil {
		return err
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Call(ipc.MethodProbeCallerRestrictions, struct{}{}, &result); err != nil {
		return fmt.Errorf("daemon cannot preserve caller restrictions; update the daemon before starting a run: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("daemon declined caller restrictions; refusing to start a run")
	}
	return nil
}

func formatCallerRestrictionsPushOptions(r *callerrestrictions.Record) []string {
	if r == nil {
		return nil
	}
	b, _ := json.Marshal(r)
	return []string{callerRestrictionsPushPrefix + base64.RawURLEncoding.EncodeToString(b)}
}

func parseCallerRestrictionsPushOptions(options []string) (*callerrestrictions.Record, error) {
	var record *callerrestrictions.Record
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, callerRestrictionsPushPrefix)
		if !ok {
			continue
		}
		if record != nil || len(encoded) > 64*1024 {
			return nil, fmt.Errorf("duplicate or oversized caller restrictions push option")
		}
		b, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid caller restrictions push option")
		}
		var r callerrestrictions.Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("invalid caller restrictions record")
		}
		if err := r.Validate(); err != nil {
			return nil, err
		}
		record = &r
	}
	return record, nil
}

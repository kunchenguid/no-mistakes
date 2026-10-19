package cli

import (
	"github.com/spf13/cobra"
	toON "github.com/toon-format/toon-go"
)

// The explicit pair of commit IDs is the operator's recovery choice on both
// surfaces. The service still proves the entire history before moving anything.
func runRewordRecovery(cmd *cobra.Command, recorded, rewritten string) error {
	service, closeFn, err := openSyncService()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer closeFn()
	state := service.RecoverReworded(cmd.Context(), recorded, rewritten)
	fields := []toON.Field{branchSyncField(state)}
	if state.Error != "" {
		fields = append(fields, toON.Field{Key: "error", Value: state.Error})
	}
	if state.NextAction != nil {
		fields = append(fields, toON.Field{Key: "help", Value: []string{"Run `" + state.NextAction.Command + "`"}})
	}
	emitDoc(cmd, fields...)
	if !state.Recovered {
		return &exitError{code: 1}
	}
	return nil
}

package cli

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// resolveAxiRespondInput shares the intent transport's bounded regular-file
// and stdin readers. Inline values retain their existing semantics.
func resolveAxiRespondInput(ctx context.Context, cmd *cobra.Command, name, inline, file string) (string, error) {
	inlineFlag, fileFlag := "--"+name, "--"+name+"-file"
	hasInline, hasFile := cmd.Flags().Changed(name), cmd.Flags().Changed(name+"-file")
	if hasInline && hasFile {
		return "", fmt.Errorf("%s and %s are mutually exclusive", inlineFlag, fileFlag)
	}
	if !hasFile && (!hasInline || inline != "-") {
		return inline, nil
	}
	var data []byte
	var err error
	source := fileFlag
	if hasFile {
		if file == "" {
			return "", fmt.Errorf("%s requires a file path", fileFlag)
		}
		data, err = readAxiIntentFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fileFlag, err)
		}
	} else {
		source = inlineFlag + " stdin"
		data, err = readAxiIntentContext(ctx, cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read %s: %w", source, err)
		}
	}
	text := string(data)
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%s must be valid UTF-8", source)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be empty or whitespace-only", source)
	}
	return text, nil
}

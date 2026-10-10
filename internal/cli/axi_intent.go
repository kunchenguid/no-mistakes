package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// Intent travels as one base64 push option. Git's pkt-line payload is at most
// 65516 bytes (https://git-scm.com/docs/gitprotocol-common#_pkt_line_format).
// Round down to complete base64 quanta after accounting for the option prefix.
const maxAxiRunIntentBytes = (65516 - len(intentPushOptionPrefix)) / 4 * 3

// resolveAxiRunIntent runs before opening run resources. Presence, not value,
// selects the transport: explicit empty input must never become inference or
// an implicit reattach. With neither flag, leave the active-run lookup to decide
// whether intent is required, without touching stdin.
func resolveAxiRunIntent(cmd *cobra.Command, intent, file string) (string, error) {
	hasIntent := cmd.Flags().Changed("intent")
	hasFile := cmd.Flags().Changed("intent-file")
	if hasIntent && hasFile {
		return "", fmt.Errorf("--intent and --intent-file are mutually exclusive")
	}
	if !hasIntent && !hasFile {
		return "", nil
	}

	source := "--intent"
	switch {
	case hasFile:
		if file == "" {
			return "", fmt.Errorf("--intent-file requires a file path")
		}
		data, err := readAxiIntentFile(file)
		if err != nil {
			return "", fmt.Errorf("read --intent-file: %w", err)
		}
		intent, source = string(data), "--intent-file"
	case intent == "-":
		data, err := readAxiIntent(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read --intent from stdin: %w", err)
		}
		intent, source = string(data), "--intent stdin"
	}
	if len(intent) > maxAxiRunIntentBytes {
		return "", fmt.Errorf("%s exceeds the %d-byte intent transport limit", source, maxAxiRunIntentBytes)
	}
	// JSON replaces malformed UTF-8, which would change the strict receipt's
	// digest between the CLI and daemon. Reject it before opening run resources.
	if !utf8.ValidString(intent) {
		return "", fmt.Errorf("%s must be valid UTF-8", source)
	}
	// Do not trim the text sent to the run or strict-launch digest,
	// especially its leading/trailing whitespace.
	if strings.TrimSpace(intent) == "" {
		return "", fmt.Errorf("%s must not be empty or whitespace-only", source)
	}
	return intent, nil
}

func readAxiIntent(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxAxiRunIntentBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAxiRunIntentBytes {
		return nil, fmt.Errorf("input exceeds the %d-byte intent transport limit", maxAxiRunIntentBytes)
	}
	return data, nil
}

func readAxiIntentFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("intent path must name a regular file")
	}
	// The nonblocking open on Unix also covers a path replaced by a FIFO
	// after Stat. Check the opened descriptor before attempting any read.
	f, err := os.OpenFile(path, os.O_RDONLY|intentFileNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("intent path must name a regular file")
	}
	return readAxiIntent(f)
}

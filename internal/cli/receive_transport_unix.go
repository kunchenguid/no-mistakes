//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
)

type receiveCapabilityTransport struct {
	files []*os.File
}

func newReceiveCapabilityTransport(_ string, capability string) (*receiveCapabilityTransport, error) {
	transport := &receiveCapabilityTransport{files: make([]*os.File, 0, 5)}
	for i := 0; i < 5; i++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			transport.Close()
			return nil, err
		}
		if _, err := writer.WriteString(capability + "\n"); err != nil {
			_ = reader.Close()
			_ = writer.Close()
			transport.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			_ = reader.Close()
			transport.Close()
			return nil, err
		}
		transport.files = append(transport.files, reader)
	}
	return transport, nil
}

func (t *receiveCapabilityTransport) Configure(cmd *exec.Cmd) {
	cmd.ExtraFiles = t.files
}

func (t *receiveCapabilityTransport) Env() []string { return nil }

func (t *receiveCapabilityTransport) Close() error {
	if t == nil {
		return nil
	}
	var first error
	for _, file := range t.files {
		if err := file.Close(); err != nil && first == nil {
			first = err
		}
	}
	t.files = nil
	if first != nil {
		return fmt.Errorf("close receive capability transport: %w", first)
	}
	return nil
}

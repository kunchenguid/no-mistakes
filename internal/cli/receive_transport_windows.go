//go:build windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

type receiveCapabilityTransport struct {
	readers []*os.File
	values  []string
}

func newReceiveCapabilityTransport(_ string, capability string) (*receiveCapabilityTransport, error) {
	transport := &receiveCapabilityTransport{}
	for i := 0; i < 5; i++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			transport.Close()
			return nil, fmt.Errorf("create inherited capability pipe: %w", err)
		}
		if _, err := writer.WriteString(capability + "\n"); err != nil {
			_ = reader.Close()
			_ = writer.Close()
			transport.Close()
			return nil, fmt.Errorf("write inherited capability pipe: %w", err)
		}
		if err := writer.Close(); err != nil {
			_ = reader.Close()
			transport.Close()
			return nil, fmt.Errorf("close inherited capability pipe: %w", err)
		}
		transport.readers = append(transport.readers, reader)
		transport.values = append(transport.values, strconv.FormatUint(uint64(reader.Fd()), 10))
	}
	return transport, nil
}

func (t *receiveCapabilityTransport) Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	additional := make([]syscall.Handle, 0, len(t.readers))
	for _, reader := range t.readers {
		additional = append(additional, syscall.Handle(reader.Fd()))
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = additional
}

func (t *receiveCapabilityTransport) Env() []string {
	if t == nil || len(t.values) != 5 {
		return nil
	}
	return []string{
		"NO_MISTAKES_RECEIVE_CAPABILITY_ADMIT_HANDLE=" + t.values[0],
		"NO_MISTAKES_RECEIVE_CAPABILITY_PREPARED_HANDLE=" + t.values[1],
		"NO_MISTAKES_RECEIVE_CAPABILITY_COMMITTED_HANDLE=" + t.values[2],
		"NO_MISTAKES_RECEIVE_CAPABILITY_ABORTED_HANDLE=" + t.values[3],
		"NO_MISTAKES_RECEIVE_CAPABILITY_NOTIFY_HANDLE=" + t.values[4],
	}
}

func (t *receiveCapabilityTransport) Close() error {
	if t == nil {
		return nil
	}
	var first error
	for _, reader := range t.readers {
		if err := reader.Close(); err != nil && first == nil {
			first = err
		}
	}
	t.readers = nil
	return first
}

func readReceiveCapabilityHandle(value string) (*os.File, error) {
	handle, err := strconv.ParseUint(value, 10, 64)
	if err != nil || handle == 0 {
		return nil, fmt.Errorf("receive capability handle is invalid")
	}
	return os.NewFile(uintptr(handle), "receive-capability"), nil
}

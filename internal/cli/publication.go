package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/spf13/cobra"
)

const maxPublicationInputBytes = 1 << 20

func newPublicationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "publication",
		Short:         "Publish an exact completed Agent Factory candidate",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newPublicationStartCmd())
	cmd.AddCommand(newPublicationAuthorizeCmd())
	cmd.AddCommand(newPublicationStatusCmd())
	return cmd
}

func newPublicationStartCmd() *cobra.Command {
	return newPublicationMachineCmd("start", ipc.MethodPublicationStart, func(raw []byte, identity ipc.PublicationIdentity) (any, error) {
		parsed, err := publication.ParseRequest(raw)
		if err != nil {
			return nil, err
		}
		if err := verifyRequestPublisher(parsed.Request.Publisher, identity); err != nil {
			return nil, err
		}
		return &ipc.PublicationStartParams{Request: json.RawMessage(bytes.Clone(raw))}, nil
	})
}

func newPublicationAuthorizeCmd() *cobra.Command {
	return newPublicationMachineCmd("authorize", ipc.MethodPublicationAuthorize, func(raw []byte, _ ipc.PublicationIdentity) (any, error) {
		if _, err := publication.ParseAuthorization(raw); err != nil {
			return nil, err
		}
		return &ipc.PublicationAuthorizeParams{Authorization: json.RawMessage(bytes.Clone(raw))}, nil
	})
}

func newPublicationStatusCmd() *cobra.Command {
	return newPublicationMachineCmd("status", ipc.MethodPublicationStatus, func(raw []byte, _ ipc.PublicationIdentity) (any, error) {
		if _, err := publication.ParseStatusQuery(raw); err != nil {
			return nil, err
		}
		return &ipc.PublicationStatusParams{Query: json.RawMessage(bytes.Clone(raw))}, nil
	})
}

type publicationParamsBuilder func(raw []byte, identity ipc.PublicationIdentity) (any, error)

func newPublicationMachineCmd(verb, method string, buildParams publicationParamsBuilder) *cobra.Command {
	var requestPath string
	cmd := &cobra.Command{
		Use:           verb,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := readPublicationInput(cmd.InOrStdin(), requestPath)
			if err != nil {
				return err
			}
			identity, err := currentPublicationIdentity()
			if err != nil {
				return err
			}
			params, err := buildParams(raw, identity)
			if err != nil {
				return err
			}

			client, err := dialPublicationDaemon()
			if err != nil {
				return err
			}
			defer client.Close()
			if err := verifyPublicationHandshake(client, identity); err != nil {
				return err
			}

			var resultRaw json.RawMessage
			if err := client.Call(method, params, &resultRaw); err != nil {
				return fmt.Errorf("publication %s: %w", verb, err)
			}
			result, err := publication.ParseResult(resultRaw)
			if err != nil {
				return fmt.Errorf("publication %s returned an invalid result: %w", verb, err)
			}
			if _, err := cmd.OutOrStdout().Write(append(bytes.Clone(resultRaw), '\n')); err != nil {
				return fmt.Errorf("write publication result: %w", err)
			}
			if code := result.ExitCode(); code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&requestPath, "request", "", "read canonical JSON from this file instead of stdin")
	return cmd
}

func readPublicationInput(stdin io.Reader, requestPath string) ([]byte, error) {
	reader := stdin
	var file *os.File
	if requestPath != "" {
		opened, err := os.Open(requestPath)
		if err != nil {
			return nil, fmt.Errorf("open publication request: %w", err)
		}
		file = opened
		defer file.Close()
		reader = file
	}
	limited := io.LimitReader(reader, maxPublicationInputBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read publication request: %w", err)
	}
	if len(raw) > maxPublicationInputBytes {
		return nil, fmt.Errorf("publication request exceeds %d bytes", maxPublicationInputBytes)
	}
	return raw, nil
}

func currentPublicationIdentity() (ipc.PublicationIdentity, error) {
	binding, err := publication.CurrentPublisherBinding(executablePath())
	if err != nil {
		return ipc.PublicationIdentity{}, err
	}
	return ipc.PublicationIdentity{
		ExecutablePath:   binding.ExecutablePath,
		ExecutableSHA256: binding.ExecutableSHA256,
		BuildSHA:         binding.BuildSHA,
		Protocol:         binding.Protocol,
	}, nil
}

func verifyRequestPublisher(binding publication.PublisherBinding, identity ipc.PublicationIdentity) error {
	if binding.ExecutablePath != identity.ExecutablePath ||
		binding.ExecutableSHA256 != identity.ExecutableSHA256 ||
		binding.BuildSHA != identity.BuildSHA ||
		binding.Protocol != identity.Protocol {
		return fmt.Errorf("publication request publisher does not match the running executable")
	}
	return nil
}

func dialPublicationDaemon() (*ipc.Client, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve publication daemon path: %w", err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil, fmt.Errorf("connect to publication daemon: %w", err)
	}
	return client, nil
}

func verifyPublicationHandshake(client *ipc.Client, identity ipc.PublicationIdentity) error {
	var result ipc.PublicationHandshakeResult
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: identity}, &result); err != nil {
		return fmt.Errorf("publication daemon compatibility handshake: %w", err)
	}
	if result.Identity != identity {
		return fmt.Errorf("publication daemon executable, build, or protocol is incompatible with the pinned CLI")
	}
	return nil
}

package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func newAxiOwnerDecisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "owner-decision",
		Short:         "Create and apply controller-signed decisions for protected runs",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newAxiOwnerDecisionKeygenCmd())
	cmd.AddCommand(newAxiOwnerDecisionChallengeCmd())
	cmd.AddCommand(newAxiOwnerDecisionSignCmd())
	cmd.AddCommand(newAxiOwnerDecisionCheckpointCmd())
	return cmd
}

func newAxiOwnerDecisionKeygenCmd() *cobra.Command {
	var privatePath, publicPath string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an Ed25519 controller key pair without exposing the private key to the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(privatePath) == "" || strings.TrimSpace(publicPath) == "" {
				return emitError(cmd, 2, "--private-key and --public-key are required")
			}
			if privatePath == publicPath {
				return emitError(cmd, 2, "private and public key paths must differ")
			}
			if fileExists(privatePath) || fileExists(publicPath) {
				return emitError(cmd, 1, "refusing to overwrite an existing owner-decision key file")
			}
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return emitError(cmd, 1, fmt.Sprintf("generate owner-decision key: %v", err))
			}
			privateEncoded, _ := ownerdecision.EncodePrivateKey(privateKey)
			publicEncoded, _ := ownerdecision.EncodePublicKey(publicKey)
			if err := writeExclusive(privatePath, []byte(privateEncoded+"\n"), 0o600); err != nil {
				return emitError(cmd, 1, fmt.Sprintf("write private key: %v", err))
			}
			if err := writeExclusive(publicPath, []byte(publicEncoded+"\n"), 0o644); err != nil {
				_ = os.Remove(privatePath)
				return emitError(cmd, 1, fmt.Sprintf("write public key: %v", err))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "private_key: %s\npublic_key: %s\n", privatePath, publicPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&privatePath, "private-key", "", "new private-key file (created mode 0600; never overwritten)")
	cmd.Flags().StringVar(&publicPath, "public-key", "", "new public-key file (never overwritten)")
	return cmd
}

func newAxiOwnerDecisionChallengeCmd() *cobra.Command {
	var runID, purpose, expectedHead, outputPath string
	cmd := &cobra.Command{
		Use:   "challenge",
		Short: "Export the exact active challenge for signing outside the workload host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == "" {
				return emitError(cmd, 2, "--run is required")
			}
			client, closeClient, err := openOwnerDecisionClient()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer closeClient()
			challenge, err := ownerChallengeForExport(client, runID, purpose, expectedHead)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			encoded, err := json.MarshalIndent(challenge, "", "  ")
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			encoded = append(encoded, '\n')
			if outputPath == "" {
				_, err = cmd.OutOrStdout().Write(encoded)
				return err
			}
			if err := writeExclusive(outputPath, encoded, 0o644); err != nil {
				return emitError(cmd, 1, fmt.Sprintf("write owner-decision challenge: %v", err))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "challenge: %s\n", outputPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "protected run id")
	cmd.Flags().StringVar(&purpose, "purpose", ownerdecision.PurposeRespond, "respond | cancel | checkpoint")
	cmd.Flags().StringVar(&expectedHead, "expected-head", "", "controller-held history head (required for checkpoint)")
	cmd.Flags().StringVar(&outputPath, "out", "", "write a new challenge file instead of stdout (never overwrites)")
	return cmd
}

func newAxiOwnerDecisionSignCmd() *cobra.Command {
	var challengePath, privatePath, action, findings, instructions, addFinding, outputPath string
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Sign an exported challenge offline, without connecting to the workload daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if challengePath == "" || privatePath == "" {
				return emitError(cmd, 2, "--challenge-file and --private-key are required")
			}
			challenge, err := readOwnerDecisionChallenge(challengePath)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			if err := validateChallengeForOfflineSigning(challenge, time.Now().UTC()); err != nil {
				return emitError(cmd, 1, err.Error())
			}
			privateKey, err := readOwnerPrivateKey(privatePath)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			response, err := ownerResponseForSign(challenge.Purpose, action, findings, instructions, addFinding)
			if err != nil {
				return emitError(cmd, 2, err.Error())
			}
			envelope, err := ownerdecision.Sign(privateKey, challenge, response)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			encoded, err := json.MarshalIndent(envelope, "", "  ")
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			encoded = append(encoded, '\n')
			if outputPath == "" {
				_, err = cmd.OutOrStdout().Write(encoded)
				return err
			}
			if err := writeExclusive(outputPath, encoded, 0o644); err != nil {
				return emitError(cmd, 1, fmt.Sprintf("write decision envelope: %v", err))
			}
			if challenge.Purpose == ownerdecision.PurposeCheckpoint {
				fmt.Fprintf(cmd.OutOrStdout(), "decision: %s\nhistory_head: %s\n", outputPath, challenge.PreviousHead)
				return nil
			}
			envelopeDigest, err := ownerdecision.EnvelopeDigest(envelope)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			nextHead, err := ownerdecision.NextHead(challenge.PreviousHead, envelopeDigest)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "decision: %s\nnext_head: %s\n", outputPath, nextHead)
			return nil
		},
	}
	cmd.Flags().StringVar(&challengePath, "challenge-file", "", "exported canonical challenge JSON")
	cmd.Flags().StringVar(&privatePath, "private-key", "", "controller private-key file")
	cmd.Flags().StringVar(&action, "action", "", "approve | fix | skip | abort (respond only)")
	cmd.Flags().StringVar(&findings, "findings", "", "comma-separated finding IDs (fix only)")
	cmd.Flags().StringVar(&instructions, "instructions", "", "guidance for selected findings (fix only)")
	cmd.Flags().StringVar(&addFinding, "add-finding", "", "JSON finding object (fix only)")
	cmd.Flags().StringVar(&outputPath, "out", "", "write a new envelope file instead of stdout (never overwrites)")
	return cmd
}

func validateChallengeForOfflineSigning(challenge ownerdecision.Challenge, now time.Time) error {
	if err := ownerdecision.ValidateChallenge(challenge); err != nil {
		return err
	}
	if now.Unix() < challenge.IssuedAt {
		return errors.New("owner decision: refusing to sign a future-issued challenge")
	}
	if now.Unix() >= challenge.ExpiresAt {
		return errors.New("owner decision: refusing to sign an expired challenge")
	}
	return nil
}

func newAxiOwnerDecisionCheckpointCmd() *cobra.Command {
	var runID, decisionPath string
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Resume a protected run after restart using a signed external-head checkpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == "" || decisionPath == "" {
				return emitError(cmd, 2, "--run and --decision-file are required")
			}
			envelope, err := readOwnerDecisionEnvelope(decisionPath)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			client, closeClient, err := openOwnerDecisionClient()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer closeClient()
			var result ipc.OwnerDecisionCheckpointResult
			if err := client.Call(ipc.MethodOwnerDecisionCheckpoint, &ipc.OwnerDecisionCheckpointParams{RunID: runID, Decision: envelope}, &result); err != nil {
				return emitError(cmd, 1, err.Error())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "checkpointed: %t\nrun: %s\n", result.OK, runID)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "protected run id")
	cmd.Flags().StringVar(&decisionPath, "decision-file", "", "signed checkpoint envelope")
	return cmd
}

func ownerChallengeForExport(client *ipc.Client, runID, purpose, expectedHead string) (ownerdecision.Challenge, error) {
	switch purpose {
	case ownerdecision.PurposeRespond, ownerdecision.PurposeCancel:
		var result ipc.OwnerDecisionChallengeResult
		if err := client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{RunID: runID, Purpose: purpose}, &result); err != nil {
			return ownerdecision.Challenge{}, err
		}
		return result.Challenge, nil
	case ownerdecision.PurposeCheckpoint:
		if expectedHead == "" {
			return ownerdecision.Challenge{}, errors.New("--expected-head is required for a checkpoint")
		}
		var result ipc.OwnerDecisionChallengeResult
		if err := client.Call(ipc.MethodOwnerDecisionChallenge, &ipc.OwnerDecisionChallengeParams{RunID: runID, Purpose: purpose, ExpectedHead: expectedHead}, &result); err != nil {
			return ownerdecision.Challenge{}, err
		}
		return result.Challenge, nil
	default:
		return ownerdecision.Challenge{}, fmt.Errorf("unknown purpose %q", purpose)
	}
}

func ownerResponseForSign(purpose, action, findings, instructions, addFinding string) (ownerdecision.Response, error) {
	response := ownerdecision.Response{}
	switch purpose {
	case ownerdecision.PurposeCancel:
		response.Action = types.ActionAbort
		return response, nil
	case ownerdecision.PurposeCheckpoint:
		response.Action = types.ActionApprove
		return response, nil
	case ownerdecision.PurposeRespond:
	default:
		return response, fmt.Errorf("unknown purpose %q", purpose)
	}
	response.Action = types.ApprovalAction(strings.TrimSpace(action))
	switch response.Action {
	case types.ActionApprove, types.ActionSkip, types.ActionAbort:
		if findings != "" || instructions != "" || addFinding != "" {
			return response, errors.New("finding fields require --action fix")
		}
		return response, nil
	case types.ActionFix:
		response.FindingIDs = splitCSV(findings)
		if note := strings.TrimSpace(instructions); note != "" {
			response.Instructions = make(map[string]string, len(response.FindingIDs))
			for _, id := range response.FindingIDs {
				response.Instructions[id] = note
			}
		}
		if addFinding != "" {
			finding, err := parseAddFinding(addFinding)
			if err != nil {
				return response, err
			}
			response.AddedFindings = []types.Finding{finding}
		}
		if len(response.FindingIDs) == 0 && len(response.AddedFindings) == 0 {
			return response, errors.New("fix requires --findings or --add-finding")
		}
		return response, nil
	default:
		return response, errors.New("--action approve|fix|skip|abort is required for respond")
	}
}

func readOwnerPrivateKey(path string) (ed25519.PrivateKey, error) {
	encoded, err := readOwnerPrivateKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("read owner-decision private key: %w", err)
	}
	return ownerdecision.ParsePrivateKey(encoded)
}

func loadOwnerRunProtection(publicPath, expectedHead, repoID, branch, initialHeadSHA string) (*ipc.OwnerDecisionRunConfig, error) {
	if publicPath == "" {
		if expectedHead != "" {
			return nil, errors.New("--expected-owner-decision-head requires --owner-decision-public-key for a new run")
		}
		return nil, nil
	}
	publicKey, err := readOwnerPublicKey(publicPath)
	if err != nil {
		return nil, err
	}
	genesisHead, err := ownerdecision.GenesisHeadForRun(publicKey, repoID, branch, initialHeadSHA)
	if err != nil {
		return nil, err
	}
	if expectedHead == "" {
		expectedHead = genesisHead
	}
	if expectedHead != genesisHead {
		return nil, errors.New("a new protected run must use the public-key-and-run-bound genesis owner-decision history head")
	}
	encoded, err := ownerdecision.EncodePublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	return &ipc.OwnerDecisionRunConfig{PublicKey: encoded, ExpectedHead: expectedHead}, nil
}

func readOwnerPublicKey(path string) (ed25519.PublicKey, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read owner-decision public key: %w", err)
	}
	return ownerdecision.ParsePublicKey(string(encoded))
}

func readOwnerDecisionEnvelope(path string) (ownerdecision.Envelope, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ownerdecision.Envelope{}, fmt.Errorf("read owner-decision envelope: %w", err)
	}
	var envelope ownerdecision.Envelope
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return ownerdecision.Envelope{}, fmt.Errorf("decode owner-decision envelope: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ownerdecision.Envelope{}, fmt.Errorf("decode owner-decision envelope: %w", err)
	}
	return envelope, nil
}

func readOwnerDecisionChallenge(path string) (ownerdecision.Challenge, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ownerdecision.Challenge{}, fmt.Errorf("read owner-decision challenge: %w", err)
	}
	var challenge ownerdecision.Challenge
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&challenge); err != nil {
		return ownerdecision.Challenge{}, fmt.Errorf("decode owner-decision challenge: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ownerdecision.Challenge{}, fmt.Errorf("decode owner-decision challenge: %w", err)
	}
	return challenge, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func openOwnerDecisionClient() (*ipc.Client, func(), error) {
	p, err := paths.New()
	if err != nil {
		return nil, func() {}, err
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil, func() {}, fmt.Errorf("connect to daemon: %w", err)
	}
	return client, func() { _ = client.Close() }, nil
}

func writeExclusive(path string, value []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

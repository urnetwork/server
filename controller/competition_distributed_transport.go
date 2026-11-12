// The coordinator owns encrypted transport, authentication and bounded transfer.
// Lost SSH acknowledgement leaves the stable remote execution running for status
// reconciliation; transport lifetime is never the evaluation lifetime.
package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// Optional seams drive deterministic lost-acknowledgement and reconnect tests.
// Each evaluator owns one transport; a coordinator shares only immutable pins.
type competitionAgentTransport interface {
	Exchange(context.Context, competitionagent.Request, string, string) (competitionagent.Response, error)
}

// Fixed root-pinned commands are independent of every submission byte. No SSH
// agent, interactive config, environment forwarding or arbitrary remote shell
// command is inherited from the coordinator's operator account.
type competitionCommandAgentTransport struct {
	profile    *competitionDistributedCoordinatorProfile
	host       CompetitionExecutorHostPolicy
	connection competitionExecutorConnection
}

// Management placement precedes unauthenticated framing for both host classes.
// Local launch retains the existing trusted-root coordinator role; SSH uses the
// independently provisioned exact forced command, never a generic sudo bridge.
func (self competitionCommandAgentTransport) commandArguments() (string, []string, error) {
	command := self.connection.AgentCommand
	arguments := []string{"--config", self.connection.AgentConfigPath, "--serve-managed"}
	switch self.host.Transport {
	case "local":
		if err := verifyPinnedExecutable(command, self.connection.AgentCommandSha256); err != nil {
			return "", nil, err
		}
		digest, _, err := hashRegularFile(self.connection.AgentConfigPath)
		if err != nil || digest != self.connection.AgentConfigSha256 {
			return "", nil, errors.New("local executor role pin changed")
		}
	case "ssh":
		settings := self.profile.Config.Settings
		command = settings.SshCommand
		arguments = []string{"-T", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
			"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "RequestTTY=no", "-o", "ConnectionAttempts=1",
			"-o", "ConnectTimeout=" + strconv.Itoa(int(settings.ConnectionTimeout/time.Second)), "-o", "UserKnownHostsFile=" + self.connection.SshKnownHostsFile,
			"-i", self.connection.SshIdentityFile, "--", self.connection.SshEndpoint,
			self.connection.AgentCommand + " --config " + self.connection.AgentConfigPath + " --serve-managed"}
	default:
		return "", nil, errors.New("unsupported executor transport")
	}
	return command, arguments, nil
}

// Both directions are authenticated before files are trusted. The caller's
// absolute job deadline bounds this request timeout, including artifact copies.
func (self competitionCommandAgentTransport) Exchange(parent context.Context, request competitionagent.Request, inputDirectory, outputDirectory string) (competitionagent.Response, error) {
	settings := self.profile.Config.Settings
	ctx, cancel := context.WithTimeout(parent, settings.RequestTimeout)
	defer cancel()
	request.Schema, request.HostId = competitionagent.Schema, self.host.HostId
	if err := competitionagent.SignRequest(&request, self.profile.PrivateKey); err != nil {
		return competitionagent.Response{}, err
	}
	command, arguments, err := self.commandArguments()
	if err != nil {
		return competitionagent.Response{}, err
	}
	writeRequest := func(stdin io.Writer) error {
		err := competitionagent.WriteDocument(stdin, request, settings.Limits.DocumentBytes)
		if err == nil && request.Operation == "start" && request.Start != nil {
			err = competitionagent.WriteFiles(stdin, inputDirectory, request.Start.Inputs, settings.Limits.InputBytes, settings.Limits)
		}
		return err
	}
	response := competitionagent.Response{}
	readResponse := func(stdout io.Reader) error {
		if err := competitionagent.ReadDocument(stdout, &response, settings.Limits.DocumentBytes); err != nil {
			return err
		}
		key, err := base64.StdEncoding.DecodeString(self.host.AgentPublicKeyBase64)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return errors.New("invalid frozen executor verification key")
		}
		if err := competitionagent.VerifyResponse(response, key); err != nil {
			return err
		}
		if response.Schema != competitionagent.Schema || response.HostId != self.host.HostId || response.RunId != request.RunId || response.FencingToken != request.FencingToken {
			return errors.Join(competitionagent.ErrIntegrity, errors.New("executor response belongs to another host, run or owner fence"))
		}
		if request.Operation == "artifacts" {
			if outputDirectory == "" {
				return errors.New("artifact request has no private destination")
			}
			if err := competitionagent.ReadFiles(stdout, outputDirectory, response.Files, settings.Limits.ArtifactBytes, settings.Limits); err != nil {
				return err
			}
		}
		return readCompetitionAgentTrailer(stdout)
	}
	if err := exchangeCompetitionAgentCommand(ctx, cancel, command, arguments, int(settings.MaximumDiagnosticBytes), writeRequest, readResponse); err != nil {
		// Agent stderr may contain hidden request paths or runtime details; it
		// is never included in a public error or echoed into service logs.
		return competitionagent.Response{}, fmt.Errorf("authenticated executor transport failed: %w", err)
	}
	return response, nil
}

// One extra byte is definitive framing corruption; an I/O error without bytes
// is still uncertain transport. EOF is the only successful frame terminator.
func readCompetitionAgentTrailer(reader io.Reader) error {
	var trailing [1]byte
	count, err := reader.Read(trailing[:])
	if count > 0 {
		return errors.Join(competitionagent.ErrIntegrity, errors.New("executor response has trailing unframed bytes"))
	}
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return io.ErrNoProgress
}

// Matching is exact on the frozen host list; extra/missing deployment routes
// cannot cause fallback to an unqualified host or the operator's local machine.
func competitionTransportForHost(settings *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
	profile := settings.distributedCoordinator
	if profile == nil || settings.DistributedEvaluationPolicy == nil || profile.Config.CoordinatorId != settings.DistributedEvaluationPolicy.CoordinatorId {
		return nil, errors.New("distributed coordinator role has not been explicitly configured")
	}
	for _, connection := range profile.Config.Hosts {
		if connection.HostId == host.HostId {
			return competitionCommandAgentTransport{profile: profile, host: host, connection: connection}, nil
		}
	}
	return nil, errors.New("approved executor has no pinned transport")
}

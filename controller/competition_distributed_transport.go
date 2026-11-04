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
	"os/exec"
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
	command := self.connection.AgentCommand
	arguments := []string{"--config", self.connection.AgentConfigPath, "--serve"}
	if self.host.Transport == "local" {
		if err := verifyPinnedExecutable(command, self.connection.AgentCommandSha256); err != nil {
			return competitionagent.Response{}, err
		}
		digest, _, err := hashRegularFile(self.connection.AgentConfigPath)
		if err != nil || digest != self.connection.AgentConfigSha256 {
			return competitionagent.Response{}, errors.New("local executor role pin changed")
		}
	}
	if self.host.Transport == "ssh" {
		command = settings.SshCommand
		arguments = []string{"-T", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
			"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "RequestTTY=no", "-o", "ConnectionAttempts=1",
			"-o", "ConnectTimeout=" + strconv.Itoa(int(settings.ConnectionTimeout/time.Second)), "-o", "UserKnownHostsFile=" + self.connection.SshKnownHostsFile,
			"-i", self.connection.SshIdentityFile, "--", self.connection.SshEndpoint,
			self.connection.AgentCommand + " --config " + self.connection.AgentConfigPath + " --serve"}
	}
	process := exec.CommandContext(ctx, command, arguments...)
	process.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	stderr := &boundedBuffer{limit: int(settings.MaximumDiagnosticBytes)}
	process.Stderr = stderr
	stdin, err := process.StdinPipe()
	if err != nil {
		return competitionagent.Response{}, err
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		stdin.Close()
		return competitionagent.Response{}, err
	}
	if err := process.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return competitionagent.Response{}, err
	}
	written := make(chan error, 1)
	go func() {
		err := competitionagent.WriteDocument(stdin, request, settings.Limits.DocumentBytes)
		if err == nil && request.Operation == "start" && request.Start != nil {
			err = competitionagent.WriteFiles(stdin, inputDirectory, request.Start.Inputs, settings.Limits.InputBytes, settings.Limits)
		}
		written <- errors.Join(err, stdin.Close())
	}()
	response := competitionagent.Response{}
	readErr := competitionagent.ReadDocument(stdout, &response, settings.Limits.DocumentBytes)
	if readErr == nil {
		key, err := base64.StdEncoding.DecodeString(self.host.AgentPublicKeyBase64)
		if err != nil || len(key) != ed25519.PublicKeySize {
			readErr = errors.New("invalid frozen executor verification key")
		} else {
			readErr = competitionagent.VerifyResponse(response, key)
		}
	}
	if readErr == nil && (response.Schema != competitionagent.Schema || response.HostId != self.host.HostId || response.RunId != request.RunId || response.FencingToken != request.FencingToken) {
		readErr = errors.Join(competitionagent.ErrIntegrity, errors.New("executor response belongs to another host, run or owner fence"))
	}
	if readErr == nil && request.Operation == "artifacts" {
		if outputDirectory == "" {
			readErr = errors.New("artifact request has no private destination")
		} else {
			readErr = competitionagent.ReadFiles(stdout, outputDirectory, response.Files, settings.Limits.ArtifactBytes, settings.Limits)
		}
	}
	if readErr == nil {
		var trailing [1]byte
		if n, err := stdout.Read(trailing[:]); n != 0 || err != io.EOF {
			readErr = errors.New("executor response has trailing unframed bytes")
		}
	}
	if readErr != nil {
		cancel()
		stdin.Close()
		stdout.Close()
	}
	writeErr := <-written
	waitErr := process.Wait()
	if err := errors.Join(readErr, writeErr, waitErr); err != nil {
		// Agent stderr may contain hidden request paths or runtime details; it
		// is never included in a public error or echoed into service logs.
		return competitionagent.Response{}, fmt.Errorf("authenticated executor transport failed: %w", err)
	}
	return response, nil
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

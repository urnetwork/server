// Command construction is pure apart from hashing private synthetic pins. No
// agent, SSH client, privilege bridge or service manager is executed here.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Only the fixture owns these non-executed files; no installed role is read.
func competitionManagedTransportFixture(t *testing.T) competitionCommandAgentTransport {
	t.Helper()
	directory := t.TempDir()
	executable, config := []byte("synthetic pinned agent\n"), []byte("synthetic protected configuration\n")
	agentPath, configPath := filepath.Join(directory, "agent"), filepath.Join(directory, "agent.json")
	if err := writeExclusiveFile(agentPath, executable, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveFile(configPath, config, 0400); err != nil {
		t.Fatal(err)
	}
	agentDigest, configDigest := sha256.Sum256(executable), sha256.Sum256(config)
	return competitionCommandAgentTransport{
		profile: &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings()}},
		host:    CompetitionExecutorHostPolicy{Transport: "local"},
		connection: competitionExecutorConnection{
			AgentCommand: agentPath, AgentCommandSha256: hex.EncodeToString(agentDigest[:]),
			AgentConfigPath: configPath, AgentConfigSha256: hex.EncodeToString(configDigest[:]),
			SshEndpoint: "urnetwork-eval-control@192.0.2.1", SshIdentityFile: "/protected/coordinator-key", SshKnownHostsFile: "/protected/known-hosts",
		},
	}
}

// Both paths select the same bounded management wrapper; neither may fall
// back to the raw framing role merely because the host is local.
func TestCompetitionAgentCommandsUseOnlyManagedRole(t *testing.T) {
	transport := competitionManagedTransportFixture(t)
	command, arguments, err := transport.commandArguments()
	expected := []string{"--config", transport.connection.AgentConfigPath, "--serve-managed"}
	if err != nil || command != transport.connection.AgentCommand || !slices.Equal(arguments, expected) {
		t.Fatal("local command bypassed managed admission", command, arguments, err)
	}
	transport.host.Transport = "ssh"
	transport.profile.Config.Settings.SshCommand = "/protected/ssh"
	command, arguments, err = transport.commandArguments()
	expected = []string{"-T", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "RequestTTY=no", "-o", "ConnectionAttempts=1",
		"-o", "ConnectTimeout=15", "-o", "UserKnownHostsFile=" + transport.connection.SshKnownHostsFile,
		"-i", transport.connection.SshIdentityFile, "--", transport.connection.SshEndpoint,
		transport.connection.AgentCommand + " --config " + transport.connection.AgentConfigPath + " --serve-managed"}
	if err != nil || command != transport.profile.Config.Settings.SshCommand || !slices.Equal(arguments, expected) {
		t.Fatal("SSH command changed the closed transport or managed role", command, arguments, err)
	}
}

// Existing local pins still gate the wrapper, before any process can run.
func TestCompetitionLocalAgentCommandsRejectChangedPins(t *testing.T) {
	for _, fault := range []string{"agent", "config", "agent-alias"} {
		transport := competitionManagedTransportFixture(t)
		switch fault {
		case "agent":
			transport.connection.AgentCommandSha256 = strings.Repeat("0", 64)
		case "config":
			transport.connection.AgentConfigSha256 = strings.Repeat("0", 64)
		case "agent-alias":
			alias := filepath.Join(filepath.Dir(transport.connection.AgentCommand), "agent-alias")
			if err := os.Symlink(transport.connection.AgentCommand, alias); err != nil {
				t.Fatal(err)
			}
			transport.connection.AgentCommand = alias
		}
		command, arguments, err := transport.commandArguments()
		if err == nil || command != "" || arguments != nil {
			t.Fatal("changed local pin retained an executable command", command, arguments, err)
		}
	}
}

// A malformed host class cannot accidentally enter an unverified local path.
func TestCompetitionAgentCommandsRejectUnknownTransport(t *testing.T) {
	for _, transportKind := range []string{"", "SSH", "shell"} {
		transport := competitionManagedTransportFixture(t)
		transport.host.Transport = transportKind
		command, arguments, err := transport.commandArguments()
		if err == nil || command != "" || arguments != nil {
			t.Fatal("unknown host class retained an executable command", transportKind, command, arguments, err)
		}
	}
}

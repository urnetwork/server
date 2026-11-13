//go:build linux

// A restricted SSH command and systemd entrypoint, independent of all server
// service configuration. Standard output is exclusively the framed protocol.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Signal ownership remains live until contained evaluator cleanup has joined.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// Errors never include a request, hidden seed, key, or evaluator output.
		fmt.Fprintln(os.Stderr, "subevaluation operation failed")
		os.Exit(competitionagent.DockerClientExitStatus(err))
	}
}

// No server.LoadSettings, production vault, database, or network listener exists
// in this process. The protected config supplies only the independent role.
func run(ctx context.Context, args []string, input io.Reader, output, diagnostic io.Writer) error {
	parsed, err := parseArguments(args, diagnostic)
	if err != nil {
		return err
	}
	if parsed.dockerClientRunId != "" {
		return competitionagent.RunDockerClient(ctx, parsed.dockerClientRunId, parsed.dockerArguments, input, output, diagnostic)
	}
	if parsed.hostOperationRunId != "" {
		return competitionagent.RunHostFilesClient(ctx, parsed.hostOperationRunId, parsed.dockerArguments, output)
	}
	if parsed.serveManaged {
		return competitionagent.ServeManagedSsh(ctx, parsed.configPath, input, output, diagnostic)
	}
	if parsed.startPrivateEngine {
		if os.Geteuid() != 0 {
			return errors.New("private engine startup requires the fixed root role")
		}
		config, err := competitionagent.LoadPrivateEngineStartupConfig(parsed.configPath)
		if err != nil {
			return err
		}
		return competitionagent.StartPrivateEngine(ctx, config)
	}
	if parsed.dockerProbe && os.Geteuid() != 0 {
		return errors.New("Docker host audit requires the fixed root role")
	}
	if parsed.dockerProbe {
		config, err := competitionagent.LoadDockerProbeConfig(parsed.configPath)
		if err != nil {
			return err
		}
		return competitionagent.RunDockerProbe(ctx, config, parsed.dockerArguments, output)
	}
	if parsed.recoverActive {
		if os.Geteuid() != 0 {
			return errors.New("independent recovery requires the fixed root role")
		}
		config, err := competitionagent.LoadRecoveryConfig(parsed.configPath)
		if err != nil {
			return err
		}
		settings := competitionagent.DefaultAgentSettings()
		agent, err := competitionagent.NewRecoveryAgent(config, competitionagent.NewSystemdRunner(settings), settings)
		if err != nil {
			return err
		}
		return agent.RecoverActive(ctx)
	}
	if parsed.closeLegacyPrelaunch {
		if os.Geteuid() != 0 {
			return errors.New("legacy prelaunch closure requires the fixed root recovery role")
		}
		return competitionagent.RecoverLegacyPrelaunch(ctx, parsed.configPath)
	}
	if parsed.serve {
		if err := competitionagent.VerifyManagedControlPlacement(ctx, parsed.configPath); err != nil {
			return err
		}
	}
	config, err := competitionagent.LoadConfig(parsed.configPath)
	if err != nil {
		return err
	}
	settings := competitionagent.DefaultAgentSettings()
	agent, err := competitionagent.NewAgent(config, competitionagent.NewSystemdRunner(settings), settings)
	if err != nil {
		return err
	}
	if parsed.serve {
		return agent.Serve(ctx, input, output)
	}
	if parsed.cleanupRunId != "" {
		return agent.Cleanup(ctx, parsed.cleanupRunId)
	}
	if parsed.dockerOwnerRunId != "" {
		return agent.RunDockerOwner(ctx, parsed.dockerOwnerRunId)
	}
	if parsed.freezeRunId != "" {
		return agent.EmergencyFreeze(ctx, parsed.freezeRunId)
	}
	return agent.Execute(ctx, parsed.executeRunId)
}

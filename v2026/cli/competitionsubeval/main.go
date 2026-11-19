//go:build linux

// A restricted SSH command and systemd entrypoint, independent of all server
// service configuration. Standard output is exclusively the framed protocol.
package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// Signal ownership remains live until contained evaluator cleanup has joined.
func main() {
	if err := runSubevaluationCommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, run, defaultSubevaluationDiagnosticSettings()); err != nil {
		os.Exit(competitionagent.DockerClientExitStatus(err))
	}
}

// No server.LoadSettings, production vault, database, or network listener exists
// in this process. The protected config supplies only the independent role.
func run(ctx context.Context, args []string, input io.Reader, output, diagnostic io.Writer) error {
	argumentDiagnostic := diagnostic
	var argumentOwner *subevaluationDiagnostic
	if file, ok := diagnostic.(*os.File); ok {
		argumentOwner = newSubevaluationDiagnostic(file, defaultSubevaluationDiagnosticSettings())
		argumentDiagnostic = argumentOwner
	} else if argumentDiagnostic == nil {
		argumentDiagnostic = io.Discard
	}
	parsed, err := parseArguments(args, argumentDiagnostic)
	if argumentOwner != nil {
		_ = argumentOwner.Close()
	}
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
	if parsed.serveExistingManaged {
		return competitionagent.ServeExistingManagedSsh(ctx, parsed.configPath, input, output, diagnostic)
	}
	if parsed.serveExisting {
		settings := competitionagent.DefaultAgentSettings()
		agent, err := competitionagent.NewExistingControlAgent(ctx, parsed.configPath, competitionagent.NewSystemdRunner(settings), settings)
		if err != nil {
			return err
		}
		return agent.ServeExisting(ctx, input, output)
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

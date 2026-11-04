// A restricted SSH command and systemd entrypoint, independent of all server
// service configuration. Standard output is exclusively the framed protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/competitionagent"
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

// Startup accepts exactly one role and no caller-selected evaluator command.
type arguments struct {
	configPath        string
	serve             bool
	executeRunId      string
	cleanupRunId      string
	dockerClientRunId string
	dockerArguments   []string
}

// Argument parsing is kept separate so invalid modes cannot load any authority.
func parseArguments(args []string, diagnostic io.Writer) (arguments, error) {
	parsed := arguments{}
	flags := flag.NewFlagSet("competitionsubeval", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	flags.StringVar(&parsed.configPath, "config", "", "Protected root-owned subevaluation-role configuration")
	flags.BoolVar(&parsed.serve, "serve", false, "Serve one signed framed operation on stdin/stdout")
	flags.StringVar(&parsed.executeRunId, "execute", "", "Systemd-only fixed persisted execution identity")
	flags.StringVar(&parsed.cleanupRunId, "cleanup", "", "Systemd-only fixed persisted stop-cleanup identity")
	flags.StringVar(&parsed.dockerClientRunId, "docker-client", "", "Non-root fixed-run Docker client; public sibling metadata only")
	if err := flags.Parse(args); err != nil {
		return arguments{}, err
	}
	roles := 0
	for _, enabled := range []bool{parsed.serve, parsed.executeRunId != "", parsed.cleanupRunId != "", parsed.dockerClientRunId != ""} {
		if enabled {
			roles++
		}
	}
	if roles != 1 {
		return arguments{}, errors.New("exactly one fixed subevaluation role and protected config are required")
	}
	if parsed.dockerClientRunId != "" {
		if parsed.configPath != "" || flags.NArg() == 0 {
			return arguments{}, errors.New("Docker client requires only its fixed run identity and command")
		}
		parsed.dockerArguments = append([]string(nil), flags.Args()...)
	} else if flags.NArg() != 0 || parsed.configPath == "" {
		return arguments{}, errors.New("systemd and SSH roles require only protected configuration")
	}
	return parsed, nil
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
	return agent.Execute(ctx, parsed.executeRunId)
}

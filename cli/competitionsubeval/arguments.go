// Fixed argument parsing is shared by supported and unsupported entrypoints.
package main

import (
	"errors"
	"flag"
	"io"
)

// Startup accepts exactly one role and no caller-selected evaluator command.
type arguments struct {
	configPath           string
	serve                bool
	serveManaged         bool
	executeRunId         string
	cleanupRunId         string
	dockerClientRunId    string
	dockerArguments      []string
	dockerOwnerRunId     string
	freezeRunId          string
	hostOperationRunId   string
	dockerProbe          bool
	recoverActive        bool
	closeLegacyPrelaunch bool
	startPrivateEngine   bool
}

// Argument parsing is kept separate so invalid modes cannot load any authority.
func parseArguments(args []string, diagnostic io.Writer) (arguments, error) {
	parsed := arguments{}
	flags := flag.NewFlagSet("competitionsubeval", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	flags.StringVar(&parsed.configPath, "config", "", "Protected root-owned subevaluation-role configuration")
	flags.BoolVar(&parsed.serve, "serve", false, "Serve one signed framed operation on stdin/stdout")
	flags.BoolVar(&parsed.serveManaged, "serve-managed", false, "Root forced SSH wrapper; place fixed control service before framing")
	flags.StringVar(&parsed.executeRunId, "execute", "", "Systemd-only fixed persisted execution identity")
	flags.StringVar(&parsed.cleanupRunId, "cleanup", "", "Systemd-only fixed persisted stop-cleanup identity")
	flags.StringVar(&parsed.dockerClientRunId, "docker-client", "", "Non-root fixed-run Docker client; public sibling metadata only")
	flags.StringVar(&parsed.dockerOwnerRunId, "docker-owner", "", "Systemd-only independent persisted Docker request owner")
	flags.StringVar(&parsed.freezeRunId, "freeze", "", "Systemd-only fixed emergency freeze/kill, with no mutual unit join")
	flags.StringVar(&parsed.hostOperationRunId, "host-operation", "", "Non-root finite fixed-run file handoff; public sibling metadata only")
	flags.BoolVar(&parsed.dockerProbe, "docker-probe", false, "Root-only closed read-only private-engine host-audit query")
	flags.BoolVar(&parsed.recoverActive, "recover-active", false, "PID1-only independent recovery of the existing physical slot")
	flags.BoolVar(&parsed.closeLegacyPrelaunch, "close-legacy-prelaunch", false, "PID1-only explicitly authorized legacy prelaunch closure; config is the protected authorization")
	flags.BoolVar(&parsed.startPrivateEngine, "start-private-engine", false, "Root-only pinned Docker launch in the private containerd mount namespace")
	if err := flags.Parse(args); err != nil {
		return arguments{}, err
	}
	roles := 0
	for _, enabled := range []bool{parsed.serve, parsed.serveManaged, parsed.executeRunId != "", parsed.cleanupRunId != "", parsed.dockerClientRunId != "", parsed.dockerOwnerRunId != "", parsed.freezeRunId != "", parsed.hostOperationRunId != "", parsed.dockerProbe, parsed.recoverActive, parsed.closeLegacyPrelaunch, parsed.startPrivateEngine} {
		if enabled {
			roles++
		}
	}
	if roles != 1 {
		return arguments{}, errors.New("exactly one fixed subevaluation role and protected config are required")
	}
	if parsed.dockerClientRunId != "" || parsed.hostOperationRunId != "" {
		if parsed.configPath != "" || flags.NArg() == 0 {
			return arguments{}, errors.New("Docker client requires only its fixed run identity and command")
		}
		parsed.dockerArguments = append([]string(nil), flags.Args()...)
	} else if parsed.dockerProbe {
		if parsed.configPath == "" || flags.NArg() == 0 {
			return arguments{}, errors.New("root-only Docker audit requires protected configuration and fixed fact arguments")
		}
		parsed.dockerArguments = append([]string(nil), flags.Args()...)
	} else if flags.NArg() != 0 || parsed.configPath == "" {
		return arguments{}, errors.New("systemd and SSH roles require only protected configuration")
	}
	return parsed, nil
}

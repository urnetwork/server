package monitor

import (
	"context"
	"strings"
)

// warpServices discovers log-producing services from the environment repo
// registry and falls back to the core set when that source is unavailable.
func warpServices(ctx context.Context, env *probeEnv) []string {
	coreServices := []string{"api", "connect", "taskworker"}
	out, err := env.runner.warpctl(ctx, "ls", "services", env.cfg.env)
	if err != nil {
		// runner.local deliberately retains command output on failure. That
		// output can contain a partial repository line followed by warpctl's
		// own panic, so it is not a trustworthy discovery snapshot.
		return coreServices
	}
	marker := "repo names "
	var repoLine string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, marker); 0 <= i {
			repoLine = line[i+len(marker):]
			break
		}
	}
	if repoLine == "" {
		return coreServices
	}
	// warpctl logs the repository list before printing its human-readable
	// service table. Parse only that one log line; consuming the rest of the
	// combined stdout/stderr buffer folds the table (or a later panic) into
	// the final repository name.
	repoNames := strings.Split(strings.TrimSpace(repoLine), ",")
	envPrefix := env.cfg.env + "-"
	services := []string{}
	for _, repoName := range repoNames {
		repoName = strings.TrimSpace(repoName)
		if !strings.HasPrefix(repoName, envPrefix) {
			continue
		}
		service := strings.TrimPrefix(repoName, envPrefix)
		if service == "" || service == "lb" || service == "config-updater" {
			continue
		}
		services = append(services, service)
	}
	if len(services) == 0 {
		return coreServices
	}
	return services
}

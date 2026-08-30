package monitor

import (
	"context"
	"strings"
)

// warpServices discovers log-producing services from the environment repo
// registry and falls back to the core set when that source is unavailable.
func warpServices(ctx context.Context, env *probeEnv) []string {
	coreServices := []string{"api", "connect", "taskworker"}
	out, _ := env.runner.warpctl(ctx, "ls", "services", env.cfg.env)
	marker := "repo names "
	i := strings.Index(out, marker)
	if i < 0 {
		return coreServices
	}
	repoNames := strings.Split(strings.TrimSpace(out[i+len(marker):]), ",")
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

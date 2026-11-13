package monitor

import (
	"context"
	"sort"
	"strings"
)

// warpServices uses the active services.yml inventory in production. The
// registry path remains a compatibility fallback for custom SignalSettings
// and synthetic callers that do not provide that inventory.
func warpServices(ctx context.Context, env *probeEnv) []string {
	if len(env.cfg.logServices) != 0 {
		services := append([]string(nil), env.cfg.logServices...)
		sort.Strings(services)
		return services
	}
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

package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SIGNALS.md §1.5 maps to signal_log_errors.go and signal_log_errors_test.go.
// The reusable probe pulls a bounded one-minute window. RunLoop may use the
// standing stream collector from tailer.go for lossless continuous coverage.
func NewLogErrorsSignal() Signal {
	return &signalAdapter{number: "1.5", key: "log-errors", name: "Log error-class rates", probe: logWindowProbe{}}
}

type logWindowProbe struct{}

func (logWindowProbe) id() string             { return "logs/error-classes" }
func (logWindowProbe) tier() string           { return tierWarn }
func (logWindowProbe) cadence() time.Duration { return time.Minute }

func (logWindowProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	services := warpServices(ctx, env)
	findings := []finding{}
	for _, service := range services {
		out, err := env.runner.warpctl(ctx, "logs", env.cfg.env, service, "--since=1m", "--limit=10000")
		if err != nil && strings.TrimSpace(out) == "" {
			return findings, fmt.Errorf("logs %s: %w", service, err)
		}
		tailer := newLogTailer(service, nil)
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) != "" {
				tailer.classify(line)
			}
		}
		findings = append(findings, tailer.drainWindow()...)
	}
	return findings, nil
}

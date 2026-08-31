package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §11.17 maps to signal_grafana_ingress.go and
// signal_grafana_ingress_test.go. It pins Grafana health to every active edge
// address so a rotating DNS answer cannot make log visibility intermittent.
func NewGrafanaIngressSignal() Signal {
	return &signalAdapter{
		number: "11.17",
		key:    "grafana-ingress",
		name:   "Grafana exact-edge ingress",
		probe:  grafanaIngressProbe{},
	}
}

type grafanaIngressProbe struct{}

func (grafanaIngressProbe) id() string             { return "observability/grafana-ingress" }
func (grafanaIngressProbe) tier() string           { return tierPage }
func (grafanaIngressProbe) cadence() time.Duration { return time.Minute }

type grafanaIngressResult struct {
	host       *host
	configured EdgeIPv6InterfaceSettings
	public     exactHTTPSResult
}

func (grafanaIngressProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	domain := strings.TrimSpace(env.cfg.publicDomain)
	environment := strings.TrimSpace(env.cfg.env)
	if domain == "" || environment == "" {
		return nil, nil
	}
	hostname := environment + "-grafana." + domain

	tasks := []grafanaIngressResult{}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			tasks = append(tasks, grafanaIngressResult{host: target, configured: configured})
		}
	}
	if len(tasks) == 0 {
		return nil, nil
	}

	results := make(chan grafanaIngressResult, len(tasks))
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, queued := range tasks {
		task := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				task.public.err = ctx.Err()
				results <- task
				return
			}
			task.public = runExactHTTPS(
				ctx,
				env.runner,
				hostname,
				task.configured.Address,
				"/api/health",
			)
			results <- task
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]grafanaIngressResult, 0, len(tasks))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].host.name != ordered[j].host.name {
			return ordered[i].host.name < ordered[j].host.name
		}
		return ordered[i].configured.Interface < ordered[j].configured.Interface
	})

	findings := []finding{}
	for _, result := range ordered {
		if finding := grafanaIngressFinding(result); finding != nil {
			findings = append(findings, *finding)
		}
	}
	return findings, nil
}

func grafanaIngressFinding(result grafanaIngressResult) *finding {
	if exactHTTPSHealthy(result.public) {
		return nil
	}
	exitCode := result.public.values["monitor_exitcode"]
	if exitCode == "" && result.public.err != nil {
		finding := cannotObserveFinding(
			result.host.name+"/"+result.configured.Interface+"/grafana-ingress",
			result.public.err,
		)
		return &finding
	}
	// The edge-ipv6 signal owns failures that never reach HTTP. Keeping that
	// transport identity singular prevents one dead interface from opening a
	// second Grafana ticket with no additional discriminator.
	if exitCode == "7" || exitCode == "28" {
		return nil
	}

	code := result.public.values["monitor_http_code"]
	class := "grafana-edge-response"
	mechanism := "The exact edge completed a public Grafana request but did not return its expected health response. TLS/SNI, routing, authentication, or the Grafana front may differ on this edge even when another DNS-selected edge is healthy."
	action := "Inspect this edge's returned status and live LB generation, then compare it with a pinned healthy edge before changing Grafana or DNS."
	if code == "502" || code == "503" || code == "504" {
		class = "grafana-edge-upstream"
		mechanism = "TLS reached this edge's LB, but the LB could not complete the Grafana upstream request. During a rollout, an unready new Grafana container plus an absent old generation can leave the per-edge service alias without a live DNAT target; rotating DNS then makes every log query depend on which edge it selects."
		action = "On the affected edge, compare Grafana generations, each front /status, the service-alias DNAT target, and child logs. If provisioning rejected an alert interval, publish a corrected image whose intervals align to Grafana's scheduler; do not restart the same invalid artifact."
	}

	target := result.host.name
	frame := result.configured.Interface + "/" + result.configured.Address
	return &finding{
		probeId: "observability/grafana-ingress", tier: tierPage,
		class: class, target: target, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("%s %s returns Grafana HTTP %s on its exact public IPv6 path", target, result.configured.Interface, code),
		mechanism: mechanism,
		baseline:  "Every enabled edge address returns HTTP 200 from main-grafana /api/health; DNS rotation must never select an edge with a broken observability upstream.",
		observed: fmt.Sprintf(
			"address=%s interface=%s http_code=%s curl_exit=%s remote_ip=%s total_seconds=%s",
			result.configured.Address,
			result.configured.Interface,
			code,
			exitCode,
			result.public.values["monitor_remote_ip"],
			result.public.values["monitor_time_total"],
		),
		evidence: strings.TrimSpace(strings.Join([]string{
			"public probe: " + strings.TrimSpace(result.public.output),
			"public probe error: " + errorString(result.public.err),
		}, "\n")),
		action:   action,
		verify:   "Require three pinned /api/health HTTP 200 responses on every enabled edge address, then run a bounded warpctl logs query successfully across multiple DNS rotations.",
		playbook: "SIGNALS.md §11.17",
	}
}

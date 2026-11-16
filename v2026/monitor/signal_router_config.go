package monitor

import (
	"context"
	"fmt"
	"time"
)

// NewRouterConfigSignal implements SIGNALS.md §18.4.
func NewRouterConfigSignal() Signal {
	return &signalAdapter{number: "18.4", key: "router-config", name: "Router running and saved configuration agreement", probe: &routerConfigProbe{}}
}

type routerConfigProbe struct{}

func (*routerConfigProbe) id() string             { return "router/config" }
func (*routerConfigProbe) tier() string           { return tierWarn }
func (*routerConfigProbe) cadence() time.Duration { return 5 * time.Minute }

func (*routerConfigProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return routerTargets(ctx, env, "config", func(h *host, observed routerObservation, err error) []finding {
		if err != nil {
			return []finding{routerUnknown(h, "config", "capture-unavailable", err)}
		}
		findings := []finding{}
		for _, layer := range []struct {
			name       string
			comparison routerComparison
		}{{name: "running", comparison: observed.summary.Running}, {name: "saved", comparison: observed.summary.Saved}} {
			comparison := layer.comparison
			protectionUnknown := comparison.Reason == "protection-unavailable"
			compared := comparison.Reason == "compared" && comparison.Complete || comparison.Reason == "concealed-values" && comparison.Unverified > 0 || protectionUnknown
			if protectionUnknown || !compared || !comparison.Complete || comparison.Unverified != 0 {
				unknown := routerUnknown(h, "config", "comparison-unverified", nil)
				unknown.frame = layer.name
				findings = append(findings, unknown)
			}
			if !compared {
				continue
			}
			if comparison.Changes == 0 && !comparison.ProtectedDelete {
				continue
			}
			findings = append(findings, finding{
				probeId: "router/config", tier: tierWarn, class: "router-config-drift", target: h.name + "/router-config", frame: layer.name, sustain: 2,
				symptom:   "A complete router configuration capture differs from the one current desired render.",
				mechanism: "The authoritative Warp parser compared this layer to the same frozen desired artifact. Running and saved layers are evaluated independently; protected deletion is drift, never healthy refusal.",
				baseline:  "Both complete, unmasked running and saved captures have zero differences from the frozen desired config.",
				observed:  fmt.Sprintf("layer=%s changes=%d deletes=%d sets=%d unverified=%d protected_delete=%t", layer.name, comparison.Changes, comparison.Deletes, comparison.Sets, comparison.Unverified, comparison.ProtectedDelete),
				evidence:  "Private configurations are discarded after fixed-count comparison. A hostname and boot bracket binds the target, but sequential reads are not an atomic router snapshot.",
				action:    "Review the exact private diff under the router rollout procedure. Check source generation, management access and protected paths; this probe never applies or saves changes.",
				verify:    "Require complete comparisons of both layers to the same current desired render on subsequent cadences; non-emission after an unknown observation does not establish recovery.",
				playbook:  "SIGNALS.md §18.4",
			})
		}
		return findings
	})
}

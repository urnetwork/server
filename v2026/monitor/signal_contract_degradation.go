package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SIGNALS.md §2.6b observes the configured authoritative valve publication,
// independently of close backlog, rates and per-process metric initialization.
func NewContractDegradationSignal() Signal {
	return &signalAdapter{number: "2.6b", key: "contract-degradation", name: "Contract degradation valve publication", probe: contractDegradationProbe{}}
}

type contractDegradationProbe struct{}

func contractDegradationReadCommand(port int) string {
	// The local context covers admission and SSH; the native timeout also
	// bounds redis-cli after a lost SSH connection. There is no retry or
	// writable fallback if timeout, redis-cli or EVAL_RO is unavailable.
	return fmt.Sprintf("timeout --kill-after=1s 15s redis-cli -p %d -c --raw EVAL_RO %s 1 %s", port, shellSingleQuote(contractDegradationReadScript), contractDegradationKey)
}

func (contractDegradationProbe) id() string               { return "redis/contract-degradation" }
func (contractDegradationProbe) tier() string             { return tierPage }
func (contractDegradationProbe) cadence() time.Duration   { return 15 * time.Minute }
func (contractDegradationProbe) runBudget() time.Duration { return 30 * time.Second }

func contractDegradationUnknown(reason string) finding {
	return finding{
		probeId: "redis/contract-degradation", tier: tierWarn,
		class: "contract-degradation-unobservable", target: "contract-degradation", frame: "publication", sustain: 2,
		symptom:   "The configured contract degradation valve publication is unknown",
		mechanism: "The desired configuration, its stable generation, or the atomic bounded Redis publication and clock/TTL validation did not establish a complete decision.",
		baseline:  "An explicit enabled configuration has one complete usable publication with a positive bounded TTL; explicit disabled intent is a separate complete configured state.",
		observed:  "scope=configured-publication reason=" + reason,
		evidence:  "Only fixed state, numeric counters and source timing are admitted. Raw Redis replies, malformed values, configuration text and command errors are omitted.",
		context:   "The consumer fails closed for missing or unusable input, but that fallback does not prove that an enabled publisher recovered. This visibility warning grants no incident recovery credit.",
		action:    "Restore the named configuration or read-only publication boundary and inspect the existing CheckContractDegradation owner. Keep backlog/rate and payment findings independent; do not write the key, toggle configuration or schedule another task from missing evidence.",
		verify:    "At a later admitted cadence, require complete stable configuration and an authoritative charging publication, or explicit disabled desired intent. Verify deployed consumer adoption, its 15-second caches and actual contract funding separately.",
		playbook:  "SIGNALS.md §2.6b",
	}
}

// A source cause observed before the owner ends remains reachable through
// errors.Is, while private source text never becomes the returned error text.
func (contractDegradationProbe) check(ctx context.Context, env *probeEnv) (findings []finding, resultErr error) {
	var sourceErr error
	defer func() {
		if ended := ctx.Err(); ended != nil {
			findings = nil
			if sourceErr != nil && sourceErr != ended {
				resultErr = &signalContextError{contextErr: ended, sourceErr: sourceErr}
			} else {
				resultErr = ended
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := func() bool {
		if ctx.Err() != nil {
			return false
		}
		if env.cfg.routerGenerationCheck == nil {
			// An embedder's explicit immutable settings are its desired authority.
			return true
		}
		var ok bool
		ok, sourceErr = env.cfg.routerGenerationCheck(ctx)
		return ok && sourceErr == nil && ctx.Err() == nil
	}
	generationUnknown := func(phase string) []finding {
		result := "not-current"
		if sourceErr != nil {
			result = "error"
		}
		unknown := contractDegradationUnknown("settings-generation")
		unknown.observed += " guard_phase=" + phase + " checker_result=" + result
		return []finding{unknown}
	}
	if !current() {
		return generationUnknown("initial"), nil
	}
	status := env.cfg.contractDegradationConfig.normalized()
	if status == ContractDegradationConfigUnknown {
		return []finding{contractDegradationUnknown("configuration")}, nil
	}
	if status == ContractDegradationConfigDisabled {
		if !current() {
			return generationUnknown("disabled-intent"), nil
		}
		return []finding{healthyFinding("redis/contract-degradation", tierPage, "contract-degradation-active", "contract-degradation")}, nil
	}
	h := env.cfg.redisEntryHost()
	if h == nil || h.redisEntryPort < 1 || h.redisEntryPort > 65535 {
		return []finding{contractDegradationUnknown("redis-inventory")}, nil
	}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		// -c may route the single key to another configured Redis node. Do not
		// use an allowed entry to evade a known excluded cluster host.
		for _, nodeHost := range env.cfg.redisNodeHosts() {
			if sourceErr = scoped.guardSshHost(ctx, nodeHost); sourceErr != nil {
				return []finding{contractDegradationUnknown("redis-scope")}, nil
			}
		}
	}
	started := env.now().UTC()
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	raw, err := env.runner.sshTimeout(readCtx, h, contractDegradationReadCommand(h.redisEntryPort), "", 20*time.Second)
	sourceErr = errors.Join(err, readCtx.Err())
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if sourceErr != nil {
		return []finding{contractDegradationUnknown("redis-read")}, nil
	}
	if !current() {
		return generationUnknown("post-read"), nil
	}
	completed := env.now().UTC()
	state, ttlMillis, reason := parseContractDegradationFrame(raw, started, completed)
	if reason != "" {
		return []finding{contractDegradationUnknown(reason)}, nil
	}
	if !state.ZeroCost {
		return []finding{healthyFinding("redis/contract-degradation", tierPage, "contract-degradation-active", "contract-degradation")}, nil
	}
	return []finding{{
		probeId: "redis/contract-degradation", tier: tierPage,
		class: "contract-degradation-active", target: "contract-degradation", frame: "publication", sustain: 1,
		symptom:   "The configured contract degradation valve authorizes zero-cost contracts for exhausted payers",
		mechanism: "A usable authoritative publication has zero_cost=true while degraded.yml explicitly enables the valve. Exhausted-payer contracts may then be admitted without escrow, and their provider payout is zero. A first healthy check may still hold the valve open while the owner waits for its second consecutive healthy check.",
		baseline:  "Configured charging uses a complete usable zero_cost=false publication, or explicit disabled desired intent. State absence and observation failure do not establish recovery.",
		observed:  fmt.Sprintf("scope=configured-publication zero_cost=true open_count=%d close_count=%d ratio=%.6g consecutive_healthy_count=%d evaluated_at=%s source_ttl_ms=%d", state.OpenCount, state.CloseCount, state.Ratio, state.ConsecutiveHealthyCount, state.EvaluatedAt.Format(time.RFC3339Nano), ttlMillis),
		evidence:  "One EVAL_RO on the fixed key atomically read its type, bounded value, PTTL and Redis TIME. The exact producer schema, hysteresis, clocks, one-hour state age and TTL were validated under unchanged effective monitor settings.",
		context:   "This is desired configuration plus authoritative publication, not proof that every deployed consumer adopted it, that its 15-second cache refreshed, or that unpaid traffic occurred. Initialized or stale per-process gauges cannot establish this state. Backlog and rate findings remain independent.",
		action:    "Inspect the existing degradation task's successful publications and the contract close backlog and escrow path in §2.6. Establish whether free contracts were actually created through their separate counters and funding evidence. Do not rewrite the key, change balances or duplicate the task to clear this page.",
		verify:    "Require two complete admitted observations of a usable charging publication or explicit disabled desired intent before notifier recovery. Confirm actual consumer configuration/cache adoption and provider accounting separately; missing or expired state remains unknown.",
		playbook:  "SIGNALS.md §2.6b and §2.6",
	}}, nil
}

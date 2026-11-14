// Canonical Proxy wake-channel subscriber presence, with native allocation,
// current slot owner, process generation and source-time authority.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// SIGNALS.md §3.6a maps to signal_redis_shard_subscriptions.go and its test.
// DebugWatchFqHost compatibility aliases are explicitly outside this census.
func NewRedisShardSubscriptionsSignal() Signal {
	return &signalAdapter{number: "3.6a", key: "redis-shard-subscriptions", name: "Canonical Proxy sharded subscribers", probe: redisShardSubscriptionsProbe{}}
}

// A one-shot presence observation never subtracts unrelated event counters.
type redisShardSubscriptionsProbe struct{}

func (redisShardSubscriptionsProbe) id() string               { return "redis/shard-subscriptions" }
func (redisShardSubscriptionsProbe) tier() string             { return tierWarn }
func (redisShardSubscriptionsProbe) cadence() time.Duration   { return 15 * time.Minute }
func (redisShardSubscriptionsProbe) runBudget() time.Duration { return 2 * time.Minute }

var redisShardLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// A placement retains its independently authoritative per-block generation.
type redisShardPlacement struct {
	host      *host
	hostNames []string
	before    redisShardFrame
	after     redisShardFrame
	beforeOk  bool
	afterOk   bool
}

// These states retain partial coverage without promoting a sibling's success.
type redisShardObservation struct {
	placement  *redisShardPlacement
	blockIndex int
	allocation redisShardRow
	channel    redisShardChannel
	count      redisShardRow
	frame      redisShardFrame
	known      bool
}

// Unknown reasons are code-owned fixed labels; native text never renders.
func redisShardUnknown(target, reason string) finding {
	return finding{
		probeId: "redis/shard-subscriptions", tier: tierWarn,
		class: "redis-shard-subscriptions-unobservable", target: target, sustain: 1,
		symptom:   "Canonical Proxy subscriber coverage is unknown for " + target,
		mechanism: "The current active allocation, its slot owner, native command completion or stable source generation was not fully established.",
		baseline:  "Every configured active Proxy block has one fresh canonical channel census on its current Redis owner, bracketed by unchanged effective settings, native allocation and slot-map generations.",
		observed:  "scope=canonical-only compatibility_aliases=not-observed reason=" + reason,
		context:   "A nonrunning block has no subscriber by design. Missing allocation evidence, failed Redis reads and an explicit dark deployment have distinct meanings; none proves service recovery.",
		action:    "Restore the named read-only observation boundary and inspect the exact current Proxy allocation or Redis slot map. Preserve independent findings only while their settings and source authority remain established; do not infer an outage or restart services from absent evidence.",
		verify:    "At the next admitted cadence, the same configured scope returns complete fresh native evidence. Consumer delivery and fully qualified compatibility aliases require their separate controls.",
		playbook:  "SIGNALS.md §3.6a",
	}
}

// The native presence result proves neither consumer handling nor why it lost
// its subscriber. Slot migration is a candidate only with matching evidence.
func redisShardMissing(observation redisShardObservation, block string) finding {
	return finding{
		probeId: "redis/shard-subscriptions", tier: tierWarn,
		class: "redis-shard-subscriber-missing", target: observation.placement.host.name + "/proxy/" + block, sustain: 1,
		symptom:   "An active Proxy block's canonical wake channel has no sharded subscriber at its current Redis slot owner",
		mechanism: "A successful native SHARDNUMSUB returned zero for the exact canonical channel while the current owner map and active Proxy allocation remained stable across the bounded census. This establishes missing subscriber presence, not the cause of its loss.",
		baseline:  "Every active Proxy block's canonical model.ProxyClientChannel(RequireHost, RequireBlock) has SHARDNUMSUB >= 1 at the current owner.",
		observed:  fmt.Sprintf("scope=canonical-only subscribers=0 slot=%d owner_configured=true source_started_at=%s source_finished_at=%s compatibility_aliases=not-observed", observation.count.slot, observation.frame.started.Format(time.RFC3339Nano), observation.frame.finished.Format(time.RFC3339Nano)),
		evidence:  "The native reply matched the requested channel and exact nonnegative integer grammar. KEYSLOT, complete owner maps, Redis run identity, Proxy allocation and effective settings generations were revalidated. Raw channels, container IDs, node IDs and command output are omitted.",
		context:   "Proxy retains a five-second database poll fallback, so this missing wake may delay new clients and does not by itself prove delivery outage. A slot move after the source window invalidates the result. Aggregate lost/resubscribed events cannot pair unrelated channels.",
		action:    "Compare the exact running Proxy source ancestry with the sharded-subscription repair, then inspect matching slot-move and consumer evidence. Any rollout or restart requires its own authorization and PgBouncer gate; do not prescribe an already-applied repair.",
		verify:    "After the last relevant allocation or slot-map boundary, require complete canonical subscriber presence at the next two admitted cadences and independently working affected-block window setups. Verify compatibility aliases separately before claiming full watch coverage.",
		playbook:  "SIGNALS.md §3.6a",
	}
}

// A complete explicit service inventory can establish a configured dark state.
// An absent map, empty observation or disabled target cannot establish it.
func redisShardConfiguredDark(cfg *monitorConfig) bool {
	if len(cfg.logServices) == 0 || cfg.logServiceHosts == nil || cfg.logServiceBlocks == nil {
		return false
	}
	present := false
	for _, service := range cfg.logServices {
		present = present || service == "proxy"
	}
	for _, h := range append(append([]*host(nil), cfg.hosts...), cfg.disabledHosts...) {
		if h.proxy != nil {
			return false
		}
	}
	if !present {
		_, hasHosts := cfg.logServiceHosts["proxy"]
		_, hasBlocks := cfg.logServiceBlocks["proxy"]
		return !hasHosts && !hasBlocks && cfg.proxyPathExpectedHosts == 0
	}
	hosts, hostsPresent := cfg.logServiceHosts["proxy"]
	_, blocksPresent := cfg.logServiceBlocks["proxy"]
	return hostsPresent && blocksPresent && len(hosts) == 0 && cfg.proxyPathExpectedHosts == 0
}

// Validate exact configured host and block sets before accepting source labels.
// Missing placements stay unknown while independently valid placements proceed.
func redisShardInventory(ctx context.Context, env *probeEnv) ([]*redisShardPlacement, []string, []finding, bool) {
	cfg := env.cfg
	hostNames, hostsPresent := cfg.logServiceHosts["proxy"]
	blocks, blocksPresent := cfg.logServiceBlocks["proxy"]
	if !hostsPresent || !blocksPresent || len(hostNames) == 0 || len(hostNames) > 64 || len(blocks) == 0 || len(blocks) > 64 || len(hostNames)*len(blocks) > 256 || cfg.proxyPathExpectedHosts != len(hostNames) || !redisShardLabelPattern.MatchString(cfg.env) {
		return nil, nil, []finding{redisShardUnknown("proxy-fleet", "inventory")}, false
	}
	blocks = append([]string(nil), blocks...)
	sort.Strings(blocks)
	for index, block := range blocks {
		if !redisShardLabelPattern.MatchString(block) || (index > 0 && blocks[index-1] == block) {
			return nil, nil, []finding{redisShardUnknown("proxy-fleet", "inventory")}, false
		}
	}
	byName := map[string]*host{}
	for _, h := range append(append([]*host(nil), cfg.hosts...), cfg.disabledHosts...) {
		if h == nil || !redisShardLabelPattern.MatchString(h.name) || byName[h.name] != nil {
			return nil, nil, []finding{redisShardUnknown("proxy-fleet", "inventory")}, false
		}
		byName[h.name] = h
	}
	hostNames = append([]string(nil), hostNames...)
	sort.Strings(hostNames)
	placements := []*redisShardPlacement{}
	findings := []finding{}
	for index, name := range hostNames {
		if !redisShardLabelPattern.MatchString(name) || (index > 0 && hostNames[index-1] == name) {
			return nil, nil, []finding{redisShardUnknown("proxy-fleet", "inventory")}, false
		}
		h := byName[name]
		// Record denied required inputs before an unavailable/disabled placement
		// is skipped, so the public scope reducer retains unknown coverage.
		if scoped, ok := env.runner.(*hostScopeRunner); ok && h != nil {
			if err := scoped.guardSshHost(ctx, h); err != nil {
				findings = append(findings, redisShardUnknown(name+"/proxy", "placement-unavailable"))
				continue
			}
		}
		if h == nil || h.proxy == nil || h.disabled {
			findings = append(findings, redisShardUnknown(name+"/proxy", "placement-unavailable"))
			continue
		}
		allowed := []string{name}
		if public := h.proxy.PublicHostname; public != "" && public != name {
			if !redisShardLabelPattern.MatchString(public) || cfg.publicDomain == "" || public != name+"."+cfg.publicDomain {
				findings = append(findings, redisShardUnknown(name+"/proxy", "host-identity"))
				continue
			}
			allowed = append(allowed, public)
		}
		placements = append(placements, &redisShardPlacement{host: h, hostNames: allowed})
	}
	for _, h := range byName {
		if h.proxy != nil && !containsString(hostNames, h.name) {
			findings = append(findings, redisShardUnknown("proxy-fleet", "unexpected-placement"))
			break
		}
	}
	return placements, blocks, findings, true
}

// Require a one-to-one finite inventory for every announced Redis endpoint.
// Missing disabled-host metadata leaves topology unknown; no disabled owner
// is contacted to complete the map.
func redisShardEndpoints(cfg *monitorConfig) ([]redisShardEndpoint, int, bool) {
	all := append(append([]*host(nil), cfg.hosts...), cfg.disabledHosts...)
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	endpoints := []redisShardEndpoint{}
	seen := map[string]bool{}
	entry := -1
	for _, h := range all {
		if !h.hasRole("redis-cluster") {
			continue
		}
		address, err := netip.ParseAddr(h.lanIp)
		ports, valid := redisClusterPorts(h.redisNodePorts())
		if err != nil || !valid {
			return nil, -1, false
		}
		for _, port := range ports {
			key := netip.AddrPortFrom(address, uint16(port)).String()
			if seen[key] || len(endpoints) == 256 {
				return nil, -1, false
			}
			seen[key] = true
			if entry < 0 && !h.disabled {
				entry = len(endpoints)
			}
			endpoints = append(endpoints, redisShardEndpoint{Address: address.String(), Port: port, host: h})
		}
	}
	return endpoints, entry, entry >= 0
}

// Look up a validated row by its requested index, independent of source order.
func redisShardAllocation(frame redisShardFrame, index int) (redisShardRow, bool) {
	for _, row := range frame.rows {
		if row.index == index {
			return row, true
		}
	}
	return redisShardRow{}, false
}

// Revalidate each independently proven allocation after the Redis observations.
func redisShardEvaluate(observations []redisShardObservation, blocks []string, mappingKnown bool) []finding {
	findings := []finding{}
	for _, observation := range observations {
		target := observation.placement.host.name + "/proxy/" + blocks[observation.blockIndex]
		after, present := redisShardAllocation(observation.placement.after, observation.blockIndex)
		if !mappingKnown || !observation.known || !observation.placement.afterOk || !present || after.state != "active" || after.generation != observation.allocation.generation || after.hostIndex != observation.allocation.hostIndex || observation.count.count < 0 {
			findings = append(findings, redisShardUnknown(target, "census-incomplete-or-generation-changed"))
			continue
		}
		if observation.count.count == 0 {
			findings = append(findings, redisShardMissing(observation, blocks[observation.blockIndex]))
		} else {
			findings = append(findings, healthyFinding("redis/shard-subscriptions", tierWarn, "redis-shard-subscriber-missing", target))
		}
	}
	return findings
}

// Changed effective settings invalidate this entire configured census, even
// independently valid native counts. Cancellation retains adapter authority.
func (redisShardSubscriptionsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings, err := redisShardObserve(ctx, env)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err == nil {
		err = redisShardCurrentSettings(ctx, env)
	}
	if errors.Is(err, errRedisShardSettings) {
		return []finding{redisShardUnknown("proxy-fleet", "settings-generation")}, nil
	}
	return findings, err
}

// Probe execution remains serial under the existing shared SSH limits. Each
// owner is contacted once for counts, with no unconfigured-host fallback.
func redisShardObserve(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := redisShardCurrentSettings(ctx, env); err != nil {
		return nil, err
	}
	if redisShardConfiguredDark(env.cfg) {
		return []finding{healthyFinding("redis/shard-subscriptions", tierWarn, "redis-shard-subscriber-missing", "proxy-fleet")}, nil
	}
	placements, blocks, findings, inventoryKnown := redisShardInventory(ctx, env)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !inventoryKnown {
		return findings, nil
	}
	observations := []redisShardObservation{}
	for _, placement := range placements {
		request := redisShardRequest{Phase: "allocations", Environment: env.cfg.env, Host: placement.host.name, HostNames: placement.hostNames, Blocks: blocks}
		frame, err := collectRedisShardFrame(ctx, env, placement.host, request)
		if errors.Is(err, errRedisShardSettings) {
			return nil, err
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if err != nil {
			findings = append(findings, redisShardUnknown(placement.host.name+"/proxy", "allocation-source"))
			continue
		}
		placement.before, placement.beforeOk = frame, true
		if !frame.complete {
			findings = append(findings, redisShardUnknown(placement.host.name+"/proxy", "allocation-census-partial"))
		}
		for index, block := range blocks {
			allocation, _ := redisShardAllocation(frame, index)
			if allocation.state != "active" {
				findings = append(findings, redisShardUnknown(placement.host.name+"/proxy/"+block, "allocation-not-active"))
				continue
			}
			channel := model.ProxyClientChannel(placement.hostNames[allocation.hostIndex], block)
			observations = append(observations, redisShardObservation{
				placement: placement, blockIndex: index, allocation: allocation,
				channel: redisShardChannel{Index: len(observations), Name: channel},
			})
		}
	}
	if len(observations) == 0 {
		return findings, nil
	}
	endpoints, entry, endpointsKnown := redisShardEndpoints(env.cfg)
	if !endpointsKnown {
		return append(findings, redisShardUnknown("proxy-fleet", "redis-endpoint-inventory")), nil
	}
	channels := make([]redisShardChannel, len(observations))
	for index := range observations {
		channels[index] = observations[index].channel
	}
	request := redisShardRequest{Phase: "resolve", Environment: env.cfg.env, Host: endpoints[entry].host.name, Blocks: blocks, Endpoints: endpoints, Endpoint: entry, Channels: channels}
	resolved, err := collectRedisShardFrame(ctx, env, endpoints[entry].host, request)
	if errors.Is(err, errRedisShardSettings) {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return append(findings, redisShardUnknown("proxy-fleet", "slot-owner-source")), nil
	}
	byOwner := map[int][]redisShardChannel{}
	for _, route := range resolved.rows {
		observation := &observations[route.index]
		observation.channel.Slot, observation.channel.Owner = route.slot, route.owner
		byOwner[route.owner] = append(byOwner[route.owner], observation.channel)
	}
	owners := make([]int, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, owner)
	}
	sort.Ints(owners)
	for _, owner := range owners {
		endpoint := endpoints[owner]
		if endpoint.host.disabled {
			continue
		}
		countRequest := redisShardRequest{Phase: "counts", Environment: env.cfg.env, Host: endpoint.host.name, Blocks: blocks, Endpoints: endpoints, Endpoint: owner, Channels: byOwner[owner], Map: resolved.mapping}
		frame, sourceErr := collectRedisShardFrame(ctx, env, endpoint.host, countRequest)
		if errors.Is(sourceErr, errRedisShardSettings) {
			return nil, sourceErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if sourceErr != nil {
			continue
		}
		for _, count := range frame.rows {
			observation := &observations[count.index]
			observation.count, observation.frame, observation.known = count, frame, true
		}
	}
	request.Channels, request.Map = nil, resolved.mapping
	_, finalErr := collectRedisShardFrame(ctx, env, endpoints[entry].host, request)
	if errors.Is(finalErr, errRedisShardSettings) {
		return nil, finalErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	for _, placement := range placements {
		if !placement.beforeOk {
			continue
		}
		request := redisShardRequest{Phase: "allocations", Environment: env.cfg.env, Host: placement.host.name, HostNames: placement.hostNames, Blocks: blocks}
		frame, sourceErr := collectRedisShardFrame(ctx, env, placement.host, request)
		if errors.Is(sourceErr, errRedisShardSettings) {
			return nil, sourceErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		placement.after, placement.afterOk = frame, sourceErr == nil
		if sourceErr == nil && !frame.complete {
			findings = append(findings, redisShardUnknown(placement.host.name+"/proxy", "allocation-census-partial"))
		}
	}
	return append(findings, redisShardEvaluate(observations, blocks, finalErr == nil)...), nil
}

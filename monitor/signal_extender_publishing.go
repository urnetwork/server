package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §27.2 maps to signal_extender_publishing.go and
// signal_extender_publishing_test.go. Two of the channels a client learns
// extenders by are publishers the operator runs (connect/EXTENDER.md C5, C6):
// the gossip service drains the publish queue that activations, probes and the
// drip fill, and the taskworker's publish tick writes the geo dns sets of the
// extender record name to Route 53. In November 2026 both had been stopped for
// days while nothing alerted: the gossip drain since 11-06 07:49Z, after a
// claim raised during a Main brownout, and dns since 10-28 18:12Z, every tick's
// batch refused for its size (factury/research/extender-health-2026-11-11.md
// §1.4). This probe judges both from series their publishers already export:
//
//   - urnetwork_stats_extender_gossip_oldest_pending_seconds, per kind: the age
//     of the oldest message written and not yet released, which every
//     taskworker's stats collector reads from the queue table;
//   - urnetwork_taskworker_extender_dns_changes_total, per record type and
//     outcome: every set change the publish tick wanted, accepted by Route 53
//     or failed.
//
// Kinds and record types are fixed enums; no extender, address or host
// reaches an alert.
func NewExtenderPublishingSignal() Signal {
	return NewExtenderPublishingSignalWithSettings(DefaultExtenderPublishingSettings())
}

// The signal with explicit thresholds.
func NewExtenderPublishingSignalWithSettings(settings *ExtenderPublishingSettings) Signal {
	return &signalAdapter{
		number: "27.2", key: "extender-publishing", name: "Extender publishing",
		probe: extenderPublishingProbe{settings: settings},
	}
}

const extenderPublishingProbeId = "mimir/extender-publishing"

// The thresholds and bounds of §27.2.
type ExtenderPublishingSettings struct {
	// a kind whose oldest unreleased message is older than GossipMaxPendingAge
	// at every evaluation of GossipHold is stalled
	GossipMaxPendingAge time.Duration
	GossipHold          time.Duration
	// a record type with a failed change in the trailing DnsFailureWindow at
	// every evaluation of DnsHold is failing
	DnsFailureWindow time.Duration
	DnsHold          time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §27.2.
func DefaultExtenderPublishingSettings() *ExtenderPublishingSettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	return &ExtenderPublishingSettings{
		GossipMaxPendingAge: 10 * time.Minute,
		GossipHold:          5 * time.Minute,
		DnsFailureWindow:    15 * time.Minute,
		DnsHold:             30 * time.Minute,
		MaxListed:           8,
		StormReadSettings:   read,
	}
}

func (self *ExtenderPublishingSettings) valid() bool {
	return self != nil && 0 < self.GossipMaxPendingAge && 0 < self.MaxListed &&
		self.DnsFailureWindow >= time.Minute && self.DnsFailureWindow%time.Minute == 0 &&
		self.StormReadSettings.valid(self.GossipHold, self.DnsHold)
}

type extenderPublishingProbe struct {
	settings *ExtenderPublishingSettings
}

func (extenderPublishingProbe) id() string   { return extenderPublishingProbeId }
func (extenderPublishingProbe) tier() string { return tierWarn }

func (self extenderPublishingProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self extenderPublishingProbe) effectiveSettings() *ExtenderPublishingSettings {
	if self.settings == nil {
		return DefaultExtenderPublishingSettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *ExtenderPublishingSettings) readSteps() int {
	return self.holdSteps(max(self.GossipHold, self.DnsHold))
}

// §27.2's expressions, each tagged with its part.
func extenderPublishingQuery(environment string, settings *ExtenderPublishingSettings) string {
	env := "env=" + strconv.Quote(environment)
	window := "[" + stormDuration(settings.DnsFailureWindow) + "]"
	changes := func(outcome string) string {
		return "sum by (record_type) (increase(urnetwork_taskworker_extender_dns_changes_total{" + env +
			",outcome=" + strconv.Quote(outcome) + "}" + window + "))"
	}
	return strings.Join([]string{
		fleetPart("gossip_oldest", "max by (kind) (urnetwork_stats_extender_gossip_oldest_pending_seconds{"+env+"})"),
		fleetPart("dns_failed", changes("failed")),
		fleetPart("dns_accepted", changes("accepted")),
	}, " or ")
}

var extenderPublishingShapes = map[string]stormShape{
	"gossip_oldest": {keys: []string{"kind"}},
	"dns_failed":    {keys: []string{"record_type"}},
	"dns_accepted":  {keys: []string{"record_type"}},
}

func (self extenderPublishingProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	stateTarget := env.cfg.env + "/extender-publishing"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(extenderPublishingProbeId, "extender-publishing-unobservable", stateTarget, "§27.2", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, extenderPublishingQuery(env.cfg.env, settings), extenderPublishingShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	at := history.at()
	findings := []finding{}
	gaps := []string{}

	judge := func(class string, target string, hold stormHold, incident func() finding) {
		switch {
		case hold.held:
			findings = append(findings, incident())
		case hold.clear:
			findings = append(findings, healthyFinding(extenderPublishingProbeId, tierWarn, class, target))
		default:
			findings = append(findings, stormClearHold(extenderPublishingProbeId, class, target))
		}
	}

	// the gossip queue, per kind
	kinds := history.keys("gossip_oldest")
	if len(kinds) == 0 {
		// no taskworker's stats collector exported the queue
		reasons = append(reasons, "gossip_state_missing")
	}
	for _, kind := range kinds {
		target := env.cfg.env + "/extender-gossip/" + kind
		oldestAt := func(step int64) stormReading {
			return history.reading("gossip_oldest", kind, step)
		}
		stalled := stormHoldOf(history.last(settings.holdSteps(settings.GossipHold)), stormLevel{threshold: settings.GossipMaxPendingAge.Seconds()}, oldestAt)
		if !stalled.complete {
			gaps = append(gaps, "gossip/"+kind+"[missing]")
		}
		judge("extender-gossip-stalled", target, stalled, func() finding {
			return finding{
				probeId: extenderPublishingProbeId, tier: tierWarn, class: "extender-gossip-stalled", target: target, sustain: 1,
				symptom:   fmt.Sprintf("The oldest unreleased extender gossip %s has waited more than %s at every evaluation for %s", kind, settings.GossipMaxPendingAge, settings.GossipHold),
				mechanism: "The gossip service drains network_extender_publish every 5 seconds, 64 rows a pass per replica (connect/EXTENDER.md C6), so a healthy queue's oldest row is seconds old. A row that waits longer means no replica is draining: the gossip processes are down or not ready, their drain is backing off a database it cannot reach, or the mesh refuses what they publish. Until it drains, a new extender reaches clients only through activation answers and dns, and a revocation not at all, so a revoked extender stays in every directory until its record expires (a day).",
				baseline:  fmt.Sprintf("The oldest unreleased message is seconds old. WARN above %s at every evaluation for %s.", settings.GossipMaxPendingAge, settings.GossipHold),
				observed:  fmt.Sprintf("kind=%s oldest_pending_seconds=%s evaluation_unix=%d", kind, stormNumber(oldestAt(at), "%.0f"), at),
				evidence:  "One bounded Mimir range read of urnetwork_stats_extender_gossip_oldest_pending_seconds, which every taskworker's stats collector computes from network_extender_publish on the replica, merged by max.",
				context:   "On 2026-11-06 a claim that raised during the Main brownout ended every replica's drain for five days. A failed pass now retries with a backoff of at most a minute, and an unready replica starts its drain once readiness passes, so a stall that lasts is a replica that is not running at all, or one whose every pass fails.",
				action:    "Read the gossip service's log for `[gossip]drain pass failed` and `[gossip]not ready`, and process_start_time_seconds{service=\"gossip\"}. Failed passes point at the replica's database path; no gossip process up, or one that logs neither line and still does not drain, needs its unit restarted (warp: docker stop the container and check docker ps).",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the oldest unreleased message under %s.", settings.GossipHold, settings.GossipMaxPendingAge),
				playbook:  "SIGNALS.md §27.2",
			}
		})
	}

	// the dns sets, per record type
	recordTypeKeys := map[string]bool{}
	for _, part := range []string{"dns_failed", "dns_accepted"} {
		for _, key := range history.keys(part) {
			recordTypeKeys[key] = true
		}
	}
	recordTypes := []string{}
	for recordType := range recordTypeKeys {
		recordTypes = append(recordTypes, recordType)
	}
	sort.Strings(recordTypes)
	if len(recordTypes) == 0 {
		// no taskworker exported the changes counter: a build that predates it
		reasons = append(reasons, "dns_state_missing")
	}
	for _, recordType := range recordTypes {
		target := env.cfg.env + "/extender-dns/" + recordType
		failedAt := func(step int64) stormReading {
			return history.reading("dns_failed", recordType, step)
		}
		failing := stormHoldOf(history.last(settings.holdSteps(settings.DnsHold)), stormLevel{threshold: 0}, failedAt)
		if !failing.complete {
			gaps = append(gaps, "dns/"+recordType+"[missing]")
		}
		judge("extender-dns-publish-failing", target, failing, func() finding {
			return finding{
				probeId: extenderPublishingProbeId, tier: tierWarn, class: "extender-dns-publish-failing", target: target, sustain: 1,
				symptom:   fmt.Sprintf("A %s set change of the extender dns tick failed in every %s window for %s", recordType, settings.DnsFailureWindow, settings.DnsHold),
				mechanism: "Every 10 minutes the taskworker's extender publish tick writes each geo dns set of the extender record name in a ChangeResourceRecordSets request of its own (connect/EXTENDER.md C5). A change that fails leaves the set as the last tick that landed it wrote it: its addresses go stale, and a TXT set's signed records expire within a day, after which a released client resolving the name verifies no extender from dns and dials none of its addresses.",
				baseline:  fmt.Sprintf("No failed change. WARN when every evaluation of a %s hold reads a failed change in its trailing %s.", settings.DnsHold, settings.DnsFailureWindow),
				observed: fmt.Sprintf("record_type=%s failed_changes_%s=%s accepted_changes_%s=%s evaluation_unix=%d",
					recordType,
					stormDuration(settings.DnsFailureWindow), stormNumber(failedAt(at), "%.1f"),
					stormDuration(settings.DnsFailureWindow), stormNumber(history.reading("dns_accepted", recordType, at), "%.1f"),
					at,
				),
				evidence: fmt.Sprintf("One bounded Mimir range read of increase(urnetwork_taskworker_extender_dns_changes_total[%s]) by record type and outcome, summed over taskworkers.", stormDuration(settings.DnsFailureWindow)),
				context:  "From 2026-10-28 18:12Z until set-by-set requests replaced the single batch, Route 53 refused every tick ('RDATA character limit of 32000 exceeded') while the dns gauges, drawn from the desired sets, read healthy. A change counts as failed when Route 53 refused it, its request errored, or it was never sent because the zone could not be resolved or listed, the publisher could not be built, or an upsert of its type failed first.",
				action:   "Read the taskworker log for `[extenderpublish]dns publish failed`, which names every failed set and Route 53's answer. InvalidChangeBatch points at a set's size or shape (the dns block's sample_count, record size); AccessDenied or a signature error at the dns block's credentials; Throttling or PriorRequestNotComplete at another writer of the hosted zone; a listing or zone error at the zone configuration.",
				verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads no failed %s change in its window.", settings.DnsHold, recordType),
				playbook: "SIGNALS.md §27.2",
			}
		})
	}

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(extenderPublishingProbeId, tierWarn, "extender-publishing-unobservable", stateTarget))
	}
	return findings, nil
}

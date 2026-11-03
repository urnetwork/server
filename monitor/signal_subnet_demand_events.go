// SIGNALS.md §22.3a maps to signal_subnet_demand_events.go and
// signal_subnet_demand_events_test.go. A partial §22.3 reader of what only the
// operator service can see: USDC that left the funding wallet with no TAO to show
// for it, and a deposit hotkey that refuses a dry run of a stake. The service records
// each as an event, because its log lines reach nobody by themselves.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const subnetDemandEventsProbeId = "subnet/demand-events"
const subnetDemandEventsPlaybook = "SIGNALS.md §22.3a"

// One deployment's newest event of each kind, and a little room for a replaced one.
const subnetDemandEventsMaximumRows = 64

// A kind the service records, and how long after its newest row it pages. Nothing
// acknowledges an event, so the one that needs a person stays up for a whole epoch.
// The others recur for as long as they last, every 10 to 15 minutes, and clear an hour
// after they stop.
var subnetDemandEventKinds = []struct {
	kind      string
	window    time.Duration
	symptom   string
	mechanism string
	baseline  string
	action    string
}{
	{
		kind:      "swap_needs_person",
		window:    7 * 24 * time.Hour,
		symptom:   "USDC left the demand deposit wallet and no TAO came for it",
		mechanism: "The funder found a swap whose USDC left the wallet with no TAO to show for it after 30 minutes. It closed the swap's journal, keeps its budget spent for the epoch and goes on converting, so nothing else will raise this again.",
		baseline:  "Every swap's USDC is matched by TAO in the wallet, or the swap is known to have failed.",
		action:    "Read the signature in the event's data (st_demand_event) and the wallet's Solana history, and find where the USDC went. If nobody withdrew it, the TAO may yet need recovering by hand. Do not reset the funder's state to clear this.",
	},
	{
		kind:      "funder_alert",
		window:    time.Hour,
		symptom:   "A swap of the demand deposit wallet's USDC is short of its TAO",
		mechanism: "The funder logged an ALERT line: a swap's TAO is still short of its floor a minute after it was signed, it paid less than its floor, or no TAO came for it at all. Its balances cannot tell a read of the TAO that lags from a withdrawal, so it buys nothing more until 30 minutes after the swap was signed.",
		baseline:  "A swap's TAO shows within a minute of its USDC leaving, at or above its floor.",
		action:    "Read the taskworker's [st]v2 ALERT demand_funder_alert line for the swap's signature, and check the wallet's Solana history. If nobody withdrew the USDC, a short read clears within the 30 minutes; swap_needs_person follows if no TAO comes.",
	},
	{
		kind:      "hotkey_not_ready",
		window:    time.Hour,
		symptom:   "The deposit hotkey refused a dry run of the smallest stake",
		mechanism: "The funder dry-runs a stake before any USDC moves, and refused to convert. It retries every 15 minutes, and priced deposits go unpaid while this lasts. An associated hotkey is not expected to refuse; a failed read during the dry run reads the same way.",
		baseline:  "The deposit hotkey is associated with the deposit signer's mirror coldkey and takes stake.",
		action:    "Compare subnet-deposit-hotkey-owner, and read the refusal in the taskworker's ALERT line. Nothing moved, so there is nothing to undo.",
	},
}

// The reader is a partial §22.3 component; the nine-family sentinel is unchanged.
func NewSubnetDemandEventsSignal() Signal {
	return &signalAdapter{
		number: "22.3a",
		key:    "subnet-demand-events",
		name:   "Demand deposit funder events",
		probe:  subnetDemandEventsProbe{},
	}
}

type subnetDemandEventsProbe struct{}

func (subnetDemandEventsProbe) id() string { return subnetDemandEventsProbeId }

func (subnetDemandEventsProbe) tier() string { return tierWarn }

func (subnetDemandEventsProbe) cadence() time.Duration {
	return DefaultSubnetReplaySettings().Cadence
}

func (subnetDemandEventsProbe) runBudget() time.Duration {
	return DefaultSubnetReplaySettings().RunBudget
}

func (subnetDemandEventsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	scope := subnetReplayScope{environment: env.cfg.env, checkpointKind: "database", completeness: "complete", source: "producer-database"}
	coverage := func(frame, reason string) finding {
		return subnetReplayCoverage(subnetDemandEventsProbeId, "subnet-demand-events/coverage", frame, scope, subnetDemandEventsPlaybook, reason, []string{"subnet-demand-event"}, 2)
	}
	deployment := subnetReplayDeploymentOf(env)
	if deployment.notDeployed || env.cfg.stConfigStatus.normalized() == STConfigurationExplicitDisabled {
		return nil, nil
	}
	if deployment.reason != "" {
		return []finding{coverage("deployment", deployment.reason)}, nil
	}
	if reason := env.cfg.subnetReplay.budgetReason(); reason != "" {
		return []finding{coverage("budget", reason)}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, env.cfg.subnetReplay.RunBudget)
	defer cancel()
	events, err := readSubnetDemandEvents(runCtx, env, subnetProcessDeploymentKey(deployment.pin))
	if err != nil {
		if reason := subnetReplayUnknownReason(err); reason != "" {
			return []finding{coverage("producer", reason)}, nil
		}
		return nil, err
	}
	findings := []finding{}
	for _, kind := range subnetDemandEventKinds {
		event, recorded := events[kind.kind]
		if !recorded || int64(kind.window/time.Second) < event.ageSeconds {
			continue
		}
		decided := subnetReplayFinding(subnetDemandEventsProbeId, "subnet-demand-event", "subnet-demand-events/"+kind.kind, "", scope, subnetDemandEventsPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: kind.kind + "-not-recorded",
			expected: "no-event-within-" + strconv.FormatInt(int64(kind.window/time.Second), 10) + "s",
			observed: fmt.Sprintf("event-age-%ds", event.ageSeconds),
			symptom:  kind.symptom, mechanism: kind.mechanism, baseline: kind.baseline, action: kind.action,
			verify: "This clears on its own once the newest such event is older than its window; it does not prove the cause is gone.",
		})
		decided.evidence = fmt.Sprintf("The newest %s event of the reviewed deployment is for epoch %d. Only its kind, epoch and age are read here; its data stays in st_demand_event.", kind.kind, event.epoch)
		findings = append(findings, decided)
	}
	return findings, nil
}

type subnetDemandEvent struct {
	epoch      int64
	ageSeconds int64
}

// Reads the deployment's newest event of each kind. The table may not exist yet, as
// the demand deposit's migration is applied before its taskworker and independently
// of this monitor, and then there is nothing to page on. As for the producer's other
// views, the query never interpolates an identity, and a failure is a gap.
func readSubnetDemandEvents(ctx context.Context, env *probeEnv, deploymentKey string) (map[string]subnetDemandEvent, error) {
	rows, err := env.runner.pg(ctx, `SELECT to_regclass('st_demand_event') IS NOT NULL`)
	if err != nil {
		return nil, subnetProcessSourceError(ctx, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 || (rows[0].str(0) != "t" && rows[0].str(0) != "f") {
		return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
	}
	if rows[0].str(0) == "f" {
		return nil, nil
	}
	rows, err = env.runner.pg(ctx, fmt.Sprintf(`
		SELECT DISTINCT ON (deployment_key, kind) deployment_key, kind, epoch,
			floor(extract(epoch FROM (now() AT TIME ZONE 'UTC') - event_time))::bigint
		FROM st_demand_event
		WHERE kind IN ('swap_needs_person', 'funder_alert', 'hotkey_not_ready')
		ORDER BY deployment_key, kind, event_time DESC
		LIMIT %d`, subnetDemandEventsMaximumRows+1))
	if err != nil {
		return nil, subnetProcessSourceError(ctx, err)
	}
	if subnetDemandEventsMaximumRows < len(rows) {
		return nil, newSubnetReplayUnknown("producer-event-bound", nil)
	}
	events := map[string]subnetDemandEvent{}
	for _, row := range rows {
		if len(row) != 4 {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		epoch, epochOk := subnetProcessInteger(row.str(2))
		age, ageOk := subnetProcessInteger(row.str(3))
		if !epochOk || !ageOk {
			return nil, newSubnetReplayUnknown("producer-row-malformed", nil)
		}
		if row.str(0) == deploymentKey {
			events[row.str(1)] = subnetDemandEvent{epoch: epoch, ageSeconds: age}
		}
	}
	return events, nil
}

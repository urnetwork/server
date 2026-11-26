package monitor

// DESIGN-COMPUTE §14.7's signals (CSRV-M12-06; SIGNALS.md §32): one signal a family of §14.7's pages, ledger,
// providers, policy, search and enclaves, each a bounded loopback Mimir query a minute over the metrics SIGNALS.md §32
// names, urnetwork_compute_*, whose labels hold opaque ids or closed sets alone (a provider's catalog id, a state, an
// outcome class, an alarm's kind), never a network, member, sidecar or request (I-13; compute-11's counts carry none).
// Each family runs only while monitor.yml's compute block enables it:
//
//	compute:
//	  families: [ledger, providers, policy, search, enclaves]
//
// so nothing pages before its service ships: without the block every family reports itself configured off, and a
// block that does not read reports each blind. An enabled family whose liveness series is not current reads
// unobservable, a visibility class that holds its pages open; a rule whose own series is absent reads unobservable
// too. A job's alarm, which sidecar/work cannot raise through the pager, reaches its page as a counter the job
// increments, urnetwork_compute_alarms_total{kind}, which the family reads over its alarm window.
//
// Every threshold is a field of ComputeMonitorSettings (QD-702), §14.7's figures its defaults.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// §14.7's families, as monitor.yml's compute block names them.
const (
	computeFamilyLedger    = "ledger"
	computeFamilyProviders = "providers"
	computeFamilyPolicy    = "policy"
	computeFamilySearch    = "search"
	computeFamilyEnclaves  = "enclaves"
)

// Every family, in SIGNALS.md §32's order.
var computeFamilies = []string{computeFamilyLedger, computeFamilyProviders, computeFamilyPolicy, computeFamilySearch, computeFamilyEnclaves}

// The settings of §32's signals: monitor.yml's compute block and §14.7's lines. DefaultComputeMonitorSettings is the
// only source of a default.
type ComputeMonitorSettings struct {
	// The families the block enables. nil settings, or no block, report every family configured off.
	Families []string
	// Why the block did not read: an unknown family, or one named twice. Set, every family reads unobservable.
	ConfigError string

	// How old a series' newest sample may be and still count as current: 3 min, several missed 15 s pushes.
	MetricFreshness time.Duration
	// The most of one Mimir answer read: 1 MiB, past which the query fails and the family reads blind.
	MimirResponseMaxByteCount int
	// How long one Mimir query may take: 15 s, inside the minute's cadence.
	MimirQueryTimeout time.Duration
	// Consecutive failing runs before a page opens: 2, riding out one lost read.
	PageSustain int
	// The window a job's alarm counter is read over: 10 min, so an alarm pages from the next run and its page clears
	// once the window passes it.
	AlarmWindow time.Duration

	// A provider failing every call for 5 minutes, judged once the window holds a call.
	ProviderFailingWindow time.Duration
	// Attestation failures above 5% of attestations for 10 minutes.
	AttestationFailureShare  float64
	AttestationFailureWindow time.Duration
	// Settlement lag over 60 s.
	SettlementLagMax time.Duration
	// A provider balance under a day of its forecast.
	ProviderBalanceMinDays float64
	// The compute policy within 14 days of its not_after (§5.4).
	PolicyExpiryWithin time.Duration
	// The newest signed ticket within 7 days of its horizon (§5.4).
	TicketHorizonWithin time.Duration
	// A reconciliation gap over 1% (§10.7).
	ReconcileGapShare float64
	// Lapsed search authorizations above 5% of authorizations for 10 minutes.
	SearchLapseShare  float64
	SearchLapseWindow time.Duration
	// The payer float under one day of searches (§21.5).
	SearchFloatMinDays float64
	// Parallel's failure rate above 5% in an hour of at least 100 searches (§21.8).
	SearchFailureShare    float64
	SearchFailureWindow   time.Duration
	SearchFailureMinCount float64
	// Disputes from at least 3 networks past 0.5% of a provider's requests in a day (§6.7).
	DisputeShare       float64
	DisputeMinNetworks float64
	DisputeWindow      time.Duration
	// A canary gap over 2% (§6.7).
	CanaryGapShare float64
	// A provider's realized margin under 5% for a week (§10.7), read from compute_reconcile's weekly gauge.
	MarginMinShare float64
	// A direct provider's costliest worst case rising: above its value this long ago (§6.6): 1 h, the price job's.
	DirectWorstCaseWindow time.Duration
	// An operation row open past 24 hours, or settled and unbooked past 60 s (§9.7).
	OpOpenMax            time.Duration
	OpSettledUnbookedMax time.Duration
	// A day's unattributed search value above $10, or 1% of its settled value (§21.8), in nano-USD.
	SearchUnattributedMaxNanoUsd float64
	SearchUnattributedShare      float64
	// The screen's lists unfetched for 24 hours (§21.5).
	SearchScreenStaleMax time.Duration
	// The signer's value not yet final past 80% of max_unsettled (§21.5).
	SignerUnsettledShare float64
	// Absorbed direct-call charges above $50 in a day across networks (§9.7), in nano-USD.
	DirectAbsorbedMaxNanoUsd float64
	DirectAbsorbedWindow     time.Duration
}

// §14.7's lines.
func DefaultComputeMonitorSettings() *ComputeMonitorSettings {
	return &ComputeMonitorSettings{
		MetricFreshness:              3 * time.Minute,
		MimirResponseMaxByteCount:    1024 * 1024,
		MimirQueryTimeout:            15 * time.Second,
		PageSustain:                  2,
		AlarmWindow:                  10 * time.Minute,
		ProviderFailingWindow:        5 * time.Minute,
		AttestationFailureShare:      0.05,
		AttestationFailureWindow:     10 * time.Minute,
		SettlementLagMax:             60 * time.Second,
		ProviderBalanceMinDays:       1,
		PolicyExpiryWithin:           14 * 24 * time.Hour,
		TicketHorizonWithin:          7 * 24 * time.Hour,
		ReconcileGapShare:            0.01,
		SearchLapseShare:             0.05,
		SearchLapseWindow:            10 * time.Minute,
		SearchFloatMinDays:           1,
		SearchFailureShare:           0.05,
		SearchFailureWindow:          1 * time.Hour,
		SearchFailureMinCount:        100,
		DisputeShare:                 0.005,
		DisputeMinNetworks:           3,
		DisputeWindow:                24 * time.Hour,
		CanaryGapShare:               0.02,
		MarginMinShare:               0.05,
		DirectWorstCaseWindow:        1 * time.Hour,
		OpOpenMax:                    24 * time.Hour,
		OpSettledUnbookedMax:         60 * time.Second,
		SearchUnattributedMaxNanoUsd: 10 * 1000 * 1000 * 1000,
		SearchUnattributedShare:      0.01,
		SearchScreenStaleMax:         24 * time.Hour,
		SignerUnsettledShare:         0.8,
		DirectAbsorbedMaxNanoUsd:     50 * 1000 * 1000 * 1000,
		DirectAbsorbedWindow:         24 * time.Hour,
	}
}

// A copy that shares nothing with settings.
func cloneComputeMonitorSettings(settings *ComputeMonitorSettings) *ComputeMonitorSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.Families = slices.Clone(settings.Families)
	return &clone
}

// monitor.yml's compute block (SIGNALS.md §32).
type computeMonitorYaml struct {
	Families []string `yaml:"families"`
}

// §32's settings from the block: nil without one, so every family reports itself configured off; with a family
// §32 does not name, or one named twice, its error, so each reports itself blind.
func loadComputeMonitorSettings(block *computeMonitorYaml) *ComputeMonitorSettings {
	if block == nil {
		return nil
	}
	settings := DefaultComputeMonitorSettings()
	seen := map[string]bool{}
	for _, family := range block.Families {
		if !slices.Contains(computeFamilies, family) {
			settings.ConfigError = fmt.Sprintf("compute names %q, which is no family of §32", family)
			return settings
		}
		if seen[family] {
			settings.ConfigError = fmt.Sprintf("compute names %q twice", family)
			return settings
		}
		seen[family] = true
		settings.Families = append(settings.Families, family)
	}
	return settings
}

// One page rule of a family: the expressions it reads, each tagged, and how it judges their values.
type computeRule struct {
	// the page class it reports
	class string
	// the expressions the rule reads, by their tag within the rule, built from the environment and the settings;
	// a value grouped by a label arrives as tag/<label value>
	expressions func(environment string, settings *ComputeMonitorSettings) map[string]string
	// groups by this label, each group its own target; "" for one target, compute
	groupBy string
	// judges one target's values: whether the rule fires, the values seen, and false where a value it needs is
	// absent, which reads unobservable
	judge func(settings *ComputeMonitorSettings, values map[string]float64) (fires bool, observed string, measured bool)
	// the finding's words: what fired, the action §14.7's runbook gives
	symptom string
	action  string
}

// One family's signal: its liveness metric and its rules.
type computeFamily struct {
	family  string
	number  string
	key     string
	name    string
	probeId string
	// the family's liveness series: an enabled family whose series is not current reads unobservable
	liveness string
	// the visibility class its blindness reports
	unobservableClass string
	rules             []*computeRule
}

// The signal over family.
func newComputeFamilySignal(family *computeFamily) Signal {
	return &signalAdapter{
		number: family.number, key: family.key, name: family.name,
		probe: &computeFamilyProbe{family: family},
	}
}

// A family's probe.
type computeFamilyProbe struct {
	family *computeFamily
	// nil queries Mimir
	query sidecarStorageQuery
}

func (self *computeFamilyProbe) id() string { return self.family.probeId }

func (self *computeFamilyProbe) tier() string { return tierPage }

func (self *computeFamilyProbe) cadence() time.Duration { return time.Minute }

// The tag of a rule's expression.
func computeRuleTag(class string, tag string) string {
	return class + "/" + tag
}

// The tag of the family's liveness.
const computeLivenessTag = "liveness"

// The family's query: its liveness, then each rule's expressions, each tagged.
func computeFamilyQuery(environment string, settings *ComputeMonitorSettings, family *computeFamily) string {
	expressions := []string{
		sidecarStorageTag("count"+sidecarStorageFresh(sidecarStorageSelector(environment, family.liveness), settings.MetricFreshness), computeLivenessTag),
	}
	for _, rule := range family.rules {
		tagExpressions := rule.expressions(environment, settings)
		tags := []string{}
		for tag := range tagExpressions {
			tags = append(tags, tag)
		}
		slices.Sort(tags)
		for _, tag := range tags {
			expressions = append(expressions, sidecarStorageTag(tagExpressions[tag], computeRuleTag(rule.class, tag)))
		}
	}
	return sidecarStorageJoin(expressions...)
}

// One run.
func (self *computeFamilyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.compute
	query := self.query
	if query == nil {
		query = computeMimirQuery
	}
	return computeFamilyFindings(ctx, env, self.family, settings, query), nil
}

// The family's findings under settings, its query run through query.
func computeFamilyFindings(ctx context.Context, env *probeEnv, family *computeFamily, settings *ComputeMonitorSettings, query sidecarStorageQuery) []finding {
	blind := func(symptom string, observed string) []finding {
		return []finding{{
			probeId: family.probeId, tier: tierPage, class: family.unobservableClass, target: "compute-" + family.family,
			sustain: 1, unobservable: true, symptom: symptom, observed: observed,
			playbook: "SIGNALS.md §" + family.number,
		}}
	}
	switch {
	case settings == nil:
		return []finding{{
			probeId: family.probeId, tier: tierPage, class: family.unobservableClass, target: "compute-" + family.family,
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no compute block",
		}}
	case settings.ConfigError != "":
		return blind("the compute block is asked for and does not read", settings.ConfigError)
	case !slices.Contains(settings.Families, family.family):
		return []finding{{
			probeId: family.probeId, tier: tierPage, class: family.unobservableClass, target: "compute-" + family.family,
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml's compute block does not enable the " + family.family + " family",
		}}
	}
	samples, err := query(ctx, env, computeFamilyQuery(env.cfg.env, settings, family))
	if err != nil {
		return blind("the family's metrics did not read from Mimir", err.Error())
	}
	return computeFamilyJudge(family, settings, samples)
}

// The family's findings from one query's samples: blind where its liveness is not current, else each rule's, by
// target.
func computeFamilyJudge(family *computeFamily, settings *ComputeMonitorSettings, samples []*sidecarStorageSample) []finding {
	liveness := 0.0
	ruleTargetValues := map[string]map[string]map[string]float64{}
	for _, sample := range samples {
		if math.IsNaN(sample.value) || math.IsInf(sample.value, 0) {
			continue
		}
		if sample.check == computeLivenessTag {
			liveness = sample.value
			continue
		}
		for _, rule := range family.rules {
			tag, ok := strings.CutPrefix(sample.check, rule.class+"/")
			if !ok {
				continue
			}
			target := "compute"
			if rule.groupBy != "" {
				target = sample.label(rule.groupBy)
				if target == "" {
					continue
				}
			}
			if ruleTargetValues[rule.class] == nil {
				ruleTargetValues[rule.class] = map[string]map[string]float64{}
			}
			if ruleTargetValues[rule.class][target] == nil {
				ruleTargetValues[rule.class][target] = map[string]float64{}
			}
			ruleTargetValues[rule.class][target][tag] = sample.value
		}
	}
	if liveness <= 0 {
		return []finding{{
			probeId: family.probeId, tier: tierPage, class: family.unobservableClass, target: "compute-" + family.family,
			sustain: 1, unobservable: true,
			symptom:  "the " + family.family + " family publishes no current " + family.liveness + " series",
			playbook: "SIGNALS.md §" + family.number,
		}}
	}
	findings := []finding{}
	for _, rule := range family.rules {
		targetValues := ruleTargetValues[rule.class]
		targets := []string{}
		for target := range targetValues {
			targets = append(targets, target)
		}
		slices.Sort(targets)
		if rule.groupBy == "" && len(targets) == 0 {
			// one target with nothing read: the rule judges what it has
			targets = []string{"compute"}
		}
		for _, target := range targets {
			values := targetValues[target]
			if values == nil {
				values = map[string]float64{}
			}
			fires, observed, measured := rule.judge(settings, values)
			switch {
			case !measured:
				findings = append(findings, finding{
					probeId: family.probeId, tier: tierPage, class: rule.class, target: target,
					sustain: 1, unobservable: true,
					symptom:  "the rule's series is absent: " + rule.symptom,
					observed: observed,
					playbook: "SIGNALS.md §" + family.number,
				})
			case fires:
				findings = append(findings, finding{
					probeId: family.probeId, tier: tierPage, class: rule.class, target: target,
					sustain:  settings.PageSustain,
					symptom:  rule.symptom,
					baseline: "DESIGN-COMPUTE §14.7's page line is not crossed.",
					verify:   "The line is no longer crossed for two runs.",
					observed: observed,
					action:   rule.action,
					playbook: "SIGNALS.md §" + family.number + "; xops' COMPUTE-OPERATIONS.md, the pages",
				})
			default:
				findings = append(findings, healthyFinding(family.probeId, tierPage, rule.class, target))
			}
		}
	}
	return findings
}

// The production query: curl to the loopback Mimir on the first services host that answers, as §30's signals query
// it, under §32's own limits.
func computeMimirQuery(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, errors.New("no services host in inventory for the loopback Mimir query")
	}
	settings := env.cfg.compute
	limit := settings.MimirResponseMaxByteCount
	command := "curl -fsS --max-time " + strconv.FormatInt(int64(settings.MimirQueryTimeout/time.Second), 10) +
		" --max-filesize " + strconv.Itoa(limit) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(env.now().Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	output, _, err := mimirShellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command, limit)
	if err != nil {
		return nil, err
	}
	return parseSidecarStorageSamples(output)
}

// A rule's helpers.

// selector's increase over span, summed, or summed by label.
func computeIncrease(environment string, metric string, span time.Duration, by string, matchers ...string) string {
	expression := "increase(" + sidecarStorageSelector(environment, metric, matchers...) + sidecarStorageRange(span) + ")"
	if by == "" {
		return "sum(" + expression + ")"
	}
	return "sum by (" + by + ") (" + expression + ")"
}

// A gauge's current value: its maximum, or its maximum by label, over its current series.
func computeGauge(environment string, settings *ComputeMonitorSettings, metric string, by string, matchers ...string) string {
	expression := sidecarStorageFresh(sidecarStorageSelector(environment, metric, matchers...), settings.MetricFreshness)
	if by == "" {
		return "max(" + expression + ")"
	}
	return "max by (" + by + ") (" + expression + ")"
}

// A gauge's least current value, or least by label.
func computeGaugeMin(environment string, settings *ComputeMonitorSettings, metric string, by string, matchers ...string) string {
	expression := sidecarStorageFresh(sidecarStorageSelector(environment, metric, matchers...), settings.MetricFreshness)
	if by == "" {
		return "min(" + expression + ")"
	}
	return "min by (" + by + ") (" + expression + ")"
}

// An alarm kind's count over the alarm window, which pages from one.
func computeAlarmRule(class string, kinds []string, symptom string, action string) *computeRule {
	return &computeRule{
		class: class,
		expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
			return map[string]string{
				"alarms": computeIncrease(environment, computeMetricAlarms, settings.AlarmWindow, "", "kind=~"+strconv.Quote(strings.Join(kinds, "|"))),
			}
		},
		judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
			// no series is no alarm: a job's counter appears at its first alarm, and the family's liveness says the
			// source is current
			alarms := values["alarms"]
			return 0 < alarms, "alarms=" + strconv.FormatFloat(alarms, 'f', 0, 64), true
		},
		symptom: symptom,
		action:  action,
	}
}

// A share's observed text.
func computeShareText(name string, part float64, whole float64) string {
	share := 0.0
	if 0 < whole {
		share = part / whole
	}
	return fmt.Sprintf("%s=%.0f of %.0f (%.2f%%)", name, part, whole, 100*share)
}

// §32's metric names, urnetwork_compute_*, as the compute block, taskworker's compute jobs and searchsigner publish
// them, each label an opaque id or a closed set. TestComputeMetricNamesAreDocumented holds each to SIGNALS.md §32.
const (
	// a job's alarm, by kind (rollup_day, rollup_total, gamma_unknown_spend, search_payer_transfer,
	// search_nonce_unknown, search_sanctions_record, search_base_disagree, search_row_retained)
	computeMetricAlarms = "urnetwork_compute_alarms_total"
	// calls by provider, model and outcome (ok, provider_error, timeout, refused, tee_unverified)
	computeMetricProviderCalls = "urnetwork_compute_provider_calls_total"
	// a call's latency by provider and model
	computeMetricCallSeconds = "urnetwork_compute_call_seconds"
	// attestations checked, by provider and result (ok, failed), and failures by provider and reason
	computeMetricAttestations        = "urnetwork_compute_attestations_total"
	computeMetricAttestationFailures = "urnetwork_compute_attestation_failures_total"
	// open holds
	computeMetricHolds = "urnetwork_compute_holds"
	// the oldest settled operation row not yet booked, in seconds
	computeMetricSettlementLag = "urnetwork_compute_settlement_lag_seconds"
	// accounts by §9.9's state
	computeMetricAccounts = "urnetwork_compute_accounts"
	// auto-renew's charges by result
	computeMetricAutoRenew = "urnetwork_compute_auto_renew_total"
	// promotion code redemptions by result and refusal code
	computeMetricPromoRedemptions = "urnetwork_compute_promo_redemptions_total"
	// a provider's balance in days of its forecast
	computeMetricProviderBalanceDays = "urnetwork_compute_provider_balance_days"
	// deposits, mints and spends, by kind
	computeMetricGammaOps = "urnetwork_compute_gamma_ops_total"
	// enclaves by state
	computeMetricEnclaves = "urnetwork_compute_enclaves"
	// disputes by provider, and the networks that disputed a provider's calls today
	computeMetricDisputes        = "urnetwork_compute_disputes_total"
	computeMetricDisputeNetworks = "urnetwork_compute_dispute_networks"
	// search authorizations by outcome (authorized, settled, lapsed), and outcome classes (ok, provider_error,
	// timeout, refused) across all networks
	computeMetricSearchAuthorizations = "urnetwork_compute_search_authorizations_total"
	computeMetricSearchOutcomes       = "urnetwork_compute_search_outcomes_total"
	// operation rows by state (open, settled, applied), and the oldest's age by state, in seconds
	computeMetricOps             = "urnetwork_compute_ops"
	computeMetricOpOldestSeconds = "urnetwork_compute_op_oldest_seconds"
	// projection rebuilds by cause, and their length
	computeMetricProjectionRebuilds       = "urnetwork_compute_projection_rebuilds_total"
	computeMetricProjectionRebuildSeconds = "urnetwork_compute_projection_rebuild_seconds"
	// direct-call charges URnetwork absorbed, by provider, in nano-USD
	computeMetricDirectAbsorbed = "urnetwork_compute_direct_absorbed_nano_usd_total"
	// today's unattributed and settled search value, in nano-USD, and whether the day before does not balance (1)
	computeMetricSearchUnattributed  = "urnetwork_compute_search_unattributed_nano_usd"
	computeMetricSearchSettled       = "urnetwork_compute_search_settled_nano_usd"
	computeMetricSearchDayUnbalanced = "urnetwork_compute_search_day_unbalanced"
	// the signer's value not yet final, and its max_unsettled, in nano-USD
	computeMetricSignerUnsettled    = "urnetwork_compute_signer_unsettled_nano_usd"
	computeMetricSignerMaxUnsettled = "urnetwork_compute_signer_max_unsettled_nano_usd"
	// the payer float in days of searches, and the screen's lists' age, in seconds
	computeMetricSearchFloatDays = "urnetwork_compute_search_float_days"
	computeMetricSearchScreenAge = "urnetwork_compute_search_screen_age_seconds"
	// seconds to the compute policy's not_after, and to the newest signed ticket's horizon
	computeMetricPolicyExpiry  = "urnetwork_compute_policy_expiry_seconds"
	computeMetricTicketHorizon = "urnetwork_compute_ticket_horizon_seconds"
	// compute_reconcile's gap, the canary's gap and the week's realized margin, by provider, as ratios
	computeMetricReconcileGap = "urnetwork_compute_reconcile_gap_ratio"
	computeMetricCanaryGap    = "urnetwork_compute_canary_gap_ratio"
	computeMetricMargin       = "urnetwork_compute_margin_ratio"
	// a direct provider's costliest worst case, in nano-USD
	computeMetricDirectWorstCase = "urnetwork_compute_direct_worst_case_nano_usd"
)

// Every metric §32 names, for the test that holds them to SIGNALS.md.
var computeMetricNames = []string{
	computeMetricAlarms, computeMetricProviderCalls, computeMetricCallSeconds, computeMetricAttestations,
	computeMetricAttestationFailures, computeMetricHolds, computeMetricSettlementLag, computeMetricAccounts,
	computeMetricAutoRenew, computeMetricPromoRedemptions, computeMetricProviderBalanceDays, computeMetricGammaOps,
	computeMetricEnclaves, computeMetricDisputes, computeMetricDisputeNetworks, computeMetricSearchAuthorizations,
	computeMetricSearchOutcomes, computeMetricOps, computeMetricOpOldestSeconds, computeMetricProjectionRebuilds,
	computeMetricProjectionRebuildSeconds, computeMetricDirectAbsorbed, computeMetricSearchUnattributed,
	computeMetricSearchSettled, computeMetricSearchDayUnbalanced, computeMetricSignerUnsettled,
	computeMetricSignerMaxUnsettled, computeMetricSearchFloatDays, computeMetricSearchScreenAge,
	computeMetricPolicyExpiry, computeMetricTicketHorizon, computeMetricReconcileGap, computeMetricCanaryGap,
	computeMetricMargin, computeMetricDirectWorstCase,
}

// §32's five signals, one a family, in its order.
func NewComputeSignals() []Signal {
	return []Signal{
		NewComputeLedgerSignal(),
		NewComputeProvidersSignal(),
		NewComputePolicySignal(),
		NewComputeSearchSignal(),
		NewComputeEnclavesSignal(),
	}
}

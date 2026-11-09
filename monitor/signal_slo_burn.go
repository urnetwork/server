package monitor

// Signal slo-burn implements SIGNALS.md §30.1 (OPS-M3-02; §19.8's SLOs and burn rows, D-OPS-8, D-OPS-9,
// owner decision 143, FS-50). Each minute one Mimir query reads, for every objective of every row
// slo_table.go holds for a service monitor.yml's sidecar_services block lists, its 30 days' events, its
// bad events over the last hour and the last 6 h, whether its series is current, and whether it reaches
// 30 days back. An objective's budget is 1 - target of its 30 days' events, and:
//
//   - 2% of that budget burned in 1 h pages §19.8's rota at any hour from M3 (slo-burn-page): roll back or
//     switch the service off (D-OPS-10);
//   - 10% of it burned in 6 h warns (slo-burn-warn), so a slow burn only warns;
//   - an objective whose service publishes no current series, whose history is under 30 days, which saw no
//     event in 30 days, or whose SLI is not published at all has no budget: it is slo-unobservable, a
//     visibility class, never healthy, which holds its burn incidents open.
//
// `monitor check-burn --service=<svc>` reads the same state for warp_rollout_gate (CheckSloBurn below):
// clear, burning when any objective crosses either line, or unknown when any objective is unobservable,
// which stops a rollout step as a burn does (plan-OPS-small-readings #11). Every query sums its labels
// away, so no finding names a network, repo, client or route.

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

func NewSloBurnSignal() Signal {
	return &signalAdapter{
		number: "30.1", key: "slo-burn", name: "Sidecar SLO budget burn",
		probe: sloBurnProbe{},
	}
}

const sloBurnProbeId = "sidecar/slo-burn"

// SIGNALS.md §30.1's classes.
const (
	sloBurnClassPage         = "slo-burn-page"
	sloBurnClassWarn         = "slo-burn-warn"
	sloBurnClassUnobservable = "slo-unobservable"
)

// One objective's partitions of the query, each tagged <service>/<objective>/<part>.
const (
	sloBurnPartTotal   = "total"
	sloBurnPartPage    = "bad_page"
	sloBurnPartWarn    = "bad_warn"
	sloBurnPartFresh   = "fresh"
	sloBurnPartHistory = "history"
)

// The settings of §30's signals (slo-burn and sidecar-service-failures): monitor.yml's sidecar_services
// block and §19.8's lines. DefaultSidecarSloSettings is the only source of a default.
type SidecarSloSettings struct {
	// The services whose SLO rows and failure warns run: monitor.yml's sidecar_services block, each a row of
	// slo_table.go. nil settings, or no block, report §30's signals configured off.
	Services []string
	// Why the block did not read: an unknown service, or one listed twice. Set, every service reads
	// unobservable. Never holds a credential.
	ConfigError string

	// The period an SLO's budget spans: §19.8's 30 days.
	BudgetPeriod time.Duration
	// §19.8's page: this share of the budget burned in PageWindow pages. 2% in 1 h.
	PageBudgetShare float64
	PageWindow      time.Duration
	// §19.8's warn: 10% in 6 h.
	WarnBudgetShare float64
	WarnWindow      time.Duration
	// The window at the start of the period an objective's series must reach into, or its history is under
	// the period and it has no budget: 1 d, so a series begun within the period's last day still counts.
	HistoryWindow time.Duration
	// How old a series' newest sample may be and still count as current: 3 min, several missed 15 s pushes.
	MetricFreshness time.Duration
	// The most of one Mimir answer read: 1 MiB, past which the query fails and the signal reads blind.
	MimirResponseMaxByteCount int
	// How long one Mimir query may take: 15 s, inside the minute's cadence.
	MimirQueryTimeout time.Duration
	// Consecutive failing runs before a page opens: 2, two minutes, riding out one lost read.
	PageSustain int
	// Consecutive failing runs before a warn opens: 2.
	WarnSustain int
	// sidecar-service-failure (§30.2): a service's 5xx share over FailureWindow past FailureShare warns,
	// judged once the window holds FailureMinRequests requests. 1% over 5 min, from 100 requests.
	FailureShare       float64
	FailureWindow      time.Duration
	FailureMinRequests float64
}

// §19.8's lines.
func DefaultSidecarSloSettings() *SidecarSloSettings {
	return &SidecarSloSettings{
		BudgetPeriod:              30 * 24 * time.Hour,
		PageBudgetShare:           0.02,
		PageWindow:                1 * time.Hour,
		WarnBudgetShare:           0.10,
		WarnWindow:                6 * time.Hour,
		HistoryWindow:             24 * time.Hour,
		MetricFreshness:           3 * time.Minute,
		MimirResponseMaxByteCount: 1024 * 1024,
		MimirQueryTimeout:         15 * time.Second,
		PageSustain:               2,
		WarnSustain:               2,
		FailureShare:              0.01,
		FailureWindow:             5 * time.Minute,
		FailureMinRequests:        100,
	}
}

// A copy that shares nothing with settings.
func cloneSidecarSloSettings(settings *SidecarSloSettings) *SidecarSloSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.Services = slices.Clone(settings.Services)
	return &clone
}

// The finding a §30 signal returns in place of its checks: configured off without a block, blind with one
// that does not read. ok is false when the signal may run.
func sidecarSloSignalGate(settings *SidecarSloSettings, probeId string, class string) (finding, bool) {
	switch {
	case settings == nil:
		return finding{
			probeId: probeId, tier: tierWarn, class: class, target: "sidecar-services",
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no sidecar_services block",
		}, true
	case settings.ConfigError != "":
		return finding{
			probeId: probeId, tier: tierWarn, class: class, target: "sidecar-services",
			sustain: 1, unobservable: true,
			symptom:  "the sidecar services are asked for and their settings do not read",
			observed: settings.ConfigError,
		}, true
	}
	return finding{}, false
}

// The production query: curl to the loopback Mimir on the first services host that answers, as the
// storage signals query it, under §30's own limits.
func sidecarSloMimirQuery(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, errors.New("no services host in inventory for the loopback Mimir query")
	}
	settings := env.cfg.sidecarSlo
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

// The check tag of one objective's partition.
func sloBurnCheck(service string, objective string, part string) string {
	return service + "/" + objective + "/" + part
}

// A selector with the environment's matcher and any others; its labels are summed away by each caller.
func sloBurnSelector(environment string, metric string, matchers ...string) string {
	return sidecarStorageSelector(environment, metric, matchers...)
}

// The partitions of one measured objective: its events over the period and the history window, its bad
// events over the page and warn windows, and whether its series is current. An unmeasured objective asks
// nothing.
func sloBurnObjectiveQueries(environment string, settings *SidecarSloSettings, service string, objective sloObjective) []string {
	historyOffset := settings.BudgetPeriod - settings.HistoryWindow
	var total, history, fresh string
	var bad func(window time.Duration) string
	switch objective.kind {
	case sloObjectiveAvailability:
		metric := "urnetwork_" + service + "_http_requests_total"
		all := sloBurnSelector(environment, metric)
		total = "sum(increase(" + all + sidecarStorageRange(settings.BudgetPeriod) + "))"
		history = "sum(increase(" + all + sidecarStorageRange(settings.HistoryWindow) + " offset " + strconv.FormatInt(int64(historyOffset/time.Second), 10) + "s))"
		fresh = "count" + sidecarStorageFresh(all, settings.MetricFreshness)
		bad = func(window time.Duration) string {
			return "sum(increase(" + sloBurnSelector(environment, metric, `code=~"5.."`) + sidecarStorageRange(window) + "))"
		}
	case sloObjectiveLatency:
		count := sloBurnSelector(environment, objective.histogram+"_count")
		good := sloBurnSelector(environment, objective.histogram+"_bucket", "le="+strconv.Quote(strconv.FormatFloat(objective.boundSeconds, 'g', -1, 64)))
		total = "sum(increase(" + count + sidecarStorageRange(settings.BudgetPeriod) + "))"
		history = "sum(increase(" + count + sidecarStorageRange(settings.HistoryWindow) + " offset " + strconv.FormatInt(int64(historyOffset/time.Second), 10) + "s))"
		fresh = "count" + sidecarStorageFresh(count, settings.MetricFreshness)
		// absent while the bound is no bucket of the histogram, which reads unobservable
		bad = func(window time.Duration) string {
			return "(sum(increase(" + count + sidecarStorageRange(window) + ")) - sum(increase(" + good + sidecarStorageRange(window) + ")))"
		}
	default:
		return nil
	}
	return []string{
		sidecarStorageTag(total, sloBurnCheck(service, objective.name, sloBurnPartTotal)),
		sidecarStorageTag(history, sloBurnCheck(service, objective.name, sloBurnPartHistory)),
		sidecarStorageTag(fresh, sloBurnCheck(service, objective.name, sloBurnPartFresh)),
		sidecarStorageTag(bad(settings.PageWindow), sloBurnCheck(service, objective.name, sloBurnPartPage)),
		sidecarStorageTag(bad(settings.WarnWindow), sloBurnCheck(service, objective.name, sloBurnPartWarn)),
	}
}

// The one query for the services' measured objectives, "" when none is measured.
func sloBurnQuery(environment string, settings *SidecarSloSettings, services []string) string {
	expressions := []string{}
	for _, service := range services {
		row, ok := sloRowOf(service)
		if !ok {
			continue
		}
		for _, objective := range row.objectives {
			expressions = append(expressions, sloBurnObjectiveQueries(environment, settings, service, objective)...)
		}
	}
	return sidecarStorageJoin(expressions...)
}

// One objective's state, as the signal and check-burn read it.
type sloObjectiveBurn struct {
	service   string
	objective string
	// the budget burned over the page and warn windows, as shares of the budget
	pageBurn float64
	warnBurn float64
	// set when the objective has no budget to burn: why
	unobservable string
	observed     string
}

// Whether it crossed the page line, the warn line, or neither.
func (self *sloObjectiveBurn) burning(settings *SidecarSloSettings) (page bool, warn bool) {
	if self.unobservable != "" {
		return false, false
	}
	return settings.PageBudgetShare*(1-sloBurnLineTolerance) <= self.pageBurn, settings.WarnBudgetShare*(1-sloBurnLineTolerance) <= self.warnBurn
}

// The relative float tolerance at a line, so a burn of exactly a line's share, which 1 - target cannot
// hold exactly in binary, reads as crossing it. A fact of float64, not a tunable.
const sloBurnLineTolerance = 1e-9

// Each objective of the services, read from one query's samples. A Mimir error leaves every measured
// objective unobservable.
func sloBurnStates(settings *SidecarSloSettings, services []string, samples []*sidecarStorageSample, queryErr error) []*sloObjectiveBurn {
	values := map[string]float64{}
	for _, sample := range samples {
		if !math.IsNaN(sample.value) && !math.IsInf(sample.value, 0) {
			values[sample.check] = sample.value
		}
	}
	burns := []*sloObjectiveBurn{}
	for _, service := range services {
		row, ok := sloRowOf(service)
		if !ok {
			burns = append(burns, &sloObjectiveBurn{service: service, objective: "slo", unobservable: "§19.8 gives the service no SLO row"})
			continue
		}
		for _, objective := range row.objectives {
			burn := &sloObjectiveBurn{service: service, objective: objective.name}
			burns = append(burns, burn)
			if objective.kind == sloObjectiveUnmeasured {
				burn.unobservable = "its SLI is not published yet: it waits for " + objective.waitsFor
				continue
			}
			if queryErr != nil {
				burn.unobservable = "its series did not read from Mimir"
				burn.observed = queryErr.Error()
				continue
			}
			value := func(part string) (float64, bool) {
				value, ok := values[sloBurnCheck(service, objective.name, part)]
				return value, ok
			}
			fresh, hasFresh := value(sloBurnPartFresh)
			total, hasTotal := value(sloBurnPartTotal)
			_, hasHistory := value(sloBurnPartHistory)
			pageBad, hasPageBad := value(sloBurnPartPage)
			warnBad, hasWarnBad := value(sloBurnPartWarn)
			if objective.kind == sloObjectiveAvailability {
				// no 5xx series is no 5xx
				hasPageBad, hasWarnBad = true, true
			}
			switch {
			case !hasFresh || fresh <= 0 || !hasTotal:
				burn.unobservable = "the service publishes no current series for it"
			case !hasHistory:
				burn.unobservable = "its series is under 30 days old, so its budget has no denominator"
			case total <= 0:
				burn.unobservable = "it saw no event in 30 days, so its budget is empty"
			case !hasPageBad || !hasWarnBad:
				burn.unobservable = "its bound is no bucket of its histogram, so a slow event cannot be told apart"
			}
			if burn.unobservable != "" {
				continue
			}
			budget := (1 - objective.target) * total
			burn.pageBurn = math.Max(pageBad, 0) / budget
			burn.warnBurn = math.Max(warnBad, 0) / budget
			burn.observed = fmt.Sprintf("events_30d=%.0f budget=%.1f bad_%s=%.0f burned_%s=%.4f bad_%s=%.0f burned_%s=%.4f",
				total, budget, sidecarStorageRange(settings.PageWindow), pageBad, sidecarStorageRange(settings.PageWindow), burn.pageBurn,
				sidecarStorageRange(settings.WarnWindow), warnBad, sidecarStorageRange(settings.WarnWindow), burn.warnBurn)
		}
	}
	return burns
}

type sloBurnProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (sloBurnProbe) id() string             { return sloBurnProbeId }
func (sloBurnProbe) tier() string           { return tierPage }
func (sloBurnProbe) cadence() time.Duration { return time.Minute }

// Reads every listed service's objectives.
func (self sloBurnProbe) burns(ctx context.Context, env *probeEnv, services []string) []*sloObjectiveBurn {
	settings := env.cfg.sidecarSlo
	query := self.query
	if query == nil {
		query = sidecarSloMimirQuery
	}
	var samples []*sidecarStorageSample
	var queryErr error
	if text := sloBurnQuery(env.cfg.env, settings, services); text != "" {
		samples, queryErr = query(ctx, env, text)
	}
	return sloBurnStates(settings, services, samples, queryErr)
}

func (self sloBurnProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarSlo
	if gate, ok := sidecarSloSignalGate(settings, sloBurnProbeId, sloBurnClassUnobservable); ok {
		return []finding{gate}, nil
	}
	findings := []finding{}
	for _, burn := range self.burns(ctx, env, settings.Services) {
		findings = append(findings, sloBurnFindings(settings, burn)...)
	}
	return findings, nil
}

// One objective's findings: its visibility while it has no budget, else each line burned or clear.
func sloBurnFindings(settings *SidecarSloSettings, burn *sloObjectiveBurn) []finding {
	target := burn.service + " " + burn.objective
	row, _ := sloRowOf(burn.service)
	if burn.unobservable != "" {
		return []finding{{
			probeId: sloBurnProbeId, tier: tierWarn, class: sloBurnClassUnobservable, target: target,
			sustain: 1, unobservable: true,
			symptom:   "the objective has no budget to burn: " + burn.unobservable,
			mechanism: "An objective without a budget shows no burn, so it can neither page nor clear.",
			baseline:  "Every listed service publishes a current series reaching back 30 days for each objective.",
			verify:    "The objective reads a budget and a burn for two runs.",
			observed:  burn.observed,
			action:    "Until it reads, nothing shows its burn, and a rollout step of the service stops unless an operator names it (warp_rollout_gate).",
			playbook:  "SIGNALS.md §30.1; §19.8's SLOs (DESIGN.md)",
		}}
	}
	page, warn := burn.burning(settings)
	findings := []finding{}
	if page {
		findings = append(findings, finding{
			probeId: sloBurnProbeId, tier: tierPage, class: sloBurnClassPage, target: target,
			sustain:   settings.PageSustain,
			symptom:   fmt.Sprintf("2%% of the objective's 30-day budget burned in an hour (%s: %s)", burn.service, row.terms),
			mechanism: "At this rate the whole 30-day budget is gone in about two days, so customers are failing now.",
			baseline:  "Under 2% of the budget burned in any hour.",
			verify:    "The hour's burn falls under 2% of the budget for two runs.",
			observed:  burn.observed,
			action:    "Roll the service back, or switch it off in config/<env>/sidecar.yml (D-OPS-10); its next rollout step waits.",
			playbook:  "SIGNALS.md §30.1; §19.8 and §19.10 (DESIGN.md)",
		})
	} else {
		findings = append(findings, healthyFinding(sloBurnProbeId, tierPage, sloBurnClassPage, target))
	}
	if warn {
		findings = append(findings, finding{
			probeId: sloBurnProbeId, tier: tierWarn, class: sloBurnClassWarn, target: target,
			sustain:   settings.WarnSustain,
			symptom:   fmt.Sprintf("10%% of the objective's 30-day budget burned in 6 hours (%s: %s)", burn.service, row.terms),
			mechanism: "A slow burn: at this rate the budget is gone in under three days.",
			baseline:  "Under 10% of the budget burned in any 6 hours.",
			verify:    "The 6 hours' burn falls under 10% of the budget for two runs.",
			observed:  burn.observed,
			action:    "Find what burns it before it pages; the service's next rollout step waits.",
			playbook:  "SIGNALS.md §30.1; §19.8 (DESIGN.md)",
		})
	} else {
		findings = append(findings, healthyFinding(sloBurnProbeId, tierWarn, sloBurnClassWarn, target))
	}
	return findings
}

// What check-burn found for one service.
type SloBurnCheck struct {
	Service string
	// SloBurnClear, SloBurnBurning or SloBurnUnknown
	State string
	// one line per objective or check read, for the operator
	Lines []string
}

// check-burn's states.
const (
	SloBurnClear   = "clear"
	SloBurnBurning = "burning"
	SloBurnUnknown = "unknown"
)

// The dns roles' stop rule beside their SLO (Q-OPS-5): the canary's classes that count as burning.
var sloBurnDnsCanaryBurningClasses = []string{dnsCanaryClassSignature, dnsCanaryClassDivergence}

// What check-burn runs for the dns canary; tests replace it.
var sloBurnDnsCanaryCheck = func(ctx context.Context, env *probeEnv) ([]finding, error) {
	return dnsCanaryProbe{}.check(ctx, env)
}

// CheckSloBurn reads one service's burn now, for `monitor check-burn` and so for warp_rollout_gate. A
// service whose row has not joined monitor.yml's sidecar_services, or has no row, is unknown. dns and
// each dns edge also run the dns canary, where an open signature or serial divergence counts as burning
// (Q-OPS-5); the edges have no SLO row of their own, so the canary is their whole check.
func CheckSloBurn(ctx context.Context, signalSettings SignalSettings, service string) (*SloBurnCheck, error) {
	signalSettings = signalSettings.withDefaults()
	if err := signalSettings.validate(); err != nil {
		return nil, err
	}
	env, err := newProbeEnv(signalSettings)
	if err != nil {
		return nil, err
	}
	return checkSloBurn(ctx, env, sloBurnProbe{}, service), nil
}

// CheckSloBurn on an env.
func checkSloBurn(ctx context.Context, env *probeEnv, probe sloBurnProbe, service string) *SloBurnCheck {
	check := &SloBurnCheck{Service: service, State: SloBurnClear}
	worsen := func(state string, line string) {
		check.Lines = append(check.Lines, line)
		switch {
		case state == SloBurnUnknown:
			check.State = SloBurnUnknown
		case state == SloBurnBurning && check.State == SloBurnClear:
			check.State = SloBurnBurning
		}
	}
	dnsEdge := strings.HasPrefix(service, "dnsedge-")
	if !dnsEdge {
		settings := env.cfg.sidecarSlo
		switch {
		case settings == nil:
			worsen(SloBurnUnknown, "monitor.yml has no sidecar_services block, so no SLO reads")
		case settings.ConfigError != "":
			worsen(SloBurnUnknown, "monitor.yml's sidecar_services block does not read: "+settings.ConfigError)
		case !slices.Contains(settings.Services, service):
			worsen(SloBurnUnknown, "its SLO row has not joined monitor.yml's sidecar_services")
		default:
			for _, burn := range probe.burns(ctx, env, []string{service}) {
				page, warn := burn.burning(settings)
				switch {
				case burn.unobservable != "":
					worsen(SloBurnUnknown, burn.objective+": unknown: "+burn.unobservable)
				case page || warn:
					worsen(SloBurnBurning, fmt.Sprintf("%s: burning: %.1f%% of its budget in %s, %.1f%% in %s", burn.objective,
						100*burn.pageBurn, settings.PageWindow, 100*burn.warnBurn, settings.WarnWindow))
				default:
					worsen(SloBurnClear, fmt.Sprintf("%s: clear: %.1f%% of its budget in %s, %.1f%% in %s", burn.objective,
						100*burn.pageBurn, settings.PageWindow, 100*burn.warnBurn, settings.WarnWindow))
				}
			}
		}
	}
	if dnsEdge || service == "dns" {
		findings, err := sloBurnDnsCanaryCheck(ctx, env)
		if err != nil {
			worsen(SloBurnUnknown, "dns canary: unknown: "+err.Error())
		}
		for _, finding := range findings {
			switch {
			case finding.configuredOff || finding.isUnobservable():
				worsen(SloBurnUnknown, "dns canary: unknown: "+finding.symptom)
			case !finding.healthy && slices.Contains(sloBurnDnsCanaryBurningClasses, finding.class):
				worsen(SloBurnBurning, "dns canary: burning: "+finding.class+" "+finding.target)
			}
		}
		if err == nil && len(findings) == 0 {
			worsen(SloBurnUnknown, "dns canary: unknown: it reported nothing")
		}
	}
	return check
}

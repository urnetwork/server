package monitor

// Signal sidecar-service-failures implements SIGNALS.md §30.2 (OPS-M3-02; §19.8's warn on each service's and
// the push sender's failures, D-OPS-8, D-OPS-9). Each minute one Mimir query reads, for each service
// monitor.yml's sidecar_services block lists, its requests and 5xx answers over the last 5 minutes from
// urnetwork_<svc>_http_requests_total, summed over every route and block:
//
//   - a 5xx share past 1%, once the window holds 100 requests, warns sidecar-service-failure;
//   - a service with no current series, or whose failures are not published as http requests yet (the
//     push sender, the edges, dns and certs, until their owners publish theirs), reads
//     sidecar-service-failure-unobservable, which holds its warn open.
//
// Its settings are §30's, SidecarSloSettings.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"
)

func NewSidecarServiceFailuresSignal() Signal {
	return &signalAdapter{
		number: "30.2", key: "sidecar-service-failures", name: "Sidecar service failures",
		probe: sidecarServiceFailuresProbe{},
	}
}

const sidecarServiceFailuresProbeId = "sidecar/service-failures"

// SIGNALS.md §30.2's classes.
const (
	sidecarServiceFailureClass             = "sidecar-service-failure"
	sidecarServiceFailureClassUnobservable = "sidecar-service-failure-unobservable"
)

// A service's partitions, each tagged <service>/<part>.
const (
	sidecarServiceFailurePartRequests = "requests"
	sidecarServiceFailurePartFailures = "failures"
	sidecarServiceFailurePartFresh    = "fresh"
)

type sidecarServiceFailuresProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (sidecarServiceFailuresProbe) id() string             { return sidecarServiceFailuresProbeId }
func (sidecarServiceFailuresProbe) tier() string           { return tierWarn }
func (sidecarServiceFailuresProbe) cadence() time.Duration { return time.Minute }

// Whether the service's row counts its requests as http requests, so its failures read from them.
func sidecarServiceFailuresMeasured(service string) bool {
	row, ok := sloRowOf(service)
	if !ok {
		return false
	}
	for _, objective := range row.objectives {
		if objective.kind == sloObjectiveAvailability {
			return true
		}
	}
	return false
}

// The query for the measured services, "" when none is.
func sidecarServiceFailuresQuery(environment string, settings *SidecarSloSettings, services []string) string {
	expressions := []string{}
	for _, service := range services {
		if !sidecarServiceFailuresMeasured(service) {
			continue
		}
		metric := "urnetwork_" + service + "_http_requests_total"
		all := sloBurnSelector(environment, metric)
		expressions = append(expressions,
			sidecarStorageTag("sum(increase("+all+sidecarStorageRange(settings.FailureWindow)+"))", service+"/"+sidecarServiceFailurePartRequests),
			sidecarStorageTag("sum(increase("+sloBurnSelector(environment, metric, `code=~"5.."`)+sidecarStorageRange(settings.FailureWindow)+"))", service+"/"+sidecarServiceFailurePartFailures),
			sidecarStorageTag("count"+sidecarStorageFresh(all, settings.MetricFreshness), service+"/"+sidecarServiceFailurePartFresh),
		)
	}
	return sidecarStorageJoin(expressions...)
}

func (self sidecarServiceFailuresProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarSlo
	if gate, ok := sidecarSloSignalGate(settings, sidecarServiceFailuresProbeId, sidecarServiceFailureClassUnobservable); ok {
		return []finding{gate}, nil
	}
	query := self.query
	if query == nil {
		query = sidecarSloMimirQuery
	}
	var samples []*sidecarStorageSample
	var queryErr error
	if text := sidecarServiceFailuresQuery(env.cfg.env, settings, settings.Services); text != "" {
		samples, queryErr = query(ctx, env, text)
	}
	return sidecarServiceFailureFindings(settings, settings.Services, samples, queryErr), nil
}

// Every listed service's finding from one query's samples.
func sidecarServiceFailureFindings(settings *SidecarSloSettings, services []string, samples []*sidecarStorageSample, queryErr error) []finding {
	values := map[string]float64{}
	for _, sample := range samples {
		if !math.IsNaN(sample.value) && !math.IsInf(sample.value, 0) {
			values[sample.check] = sample.value
		}
	}
	blind := func(service string, symptom string, observed string) finding {
		return finding{
			probeId: sidecarServiceFailuresProbeId, tier: tierWarn, class: sidecarServiceFailureClassUnobservable, target: service,
			sustain: 1, unobservable: true, symptom: symptom, observed: observed,
			playbook: "SIGNALS.md §30.2",
		}
	}
	findings := []finding{}
	for _, service := range services {
		if !sidecarServiceFailuresMeasured(service) {
			findings = append(findings, blind(service, "the service's failures are not published as http requests yet", ""))
			continue
		}
		if queryErr != nil {
			findings = append(findings, blind(service, "the service's requests did not read from Mimir", queryErr.Error()))
			continue
		}
		fresh, hasFresh := values[service+"/"+sidecarServiceFailurePartFresh]
		requests, hasRequests := values[service+"/"+sidecarServiceFailurePartRequests]
		if !hasFresh || fresh <= 0 || !hasRequests {
			findings = append(findings, blind(service, "the service publishes no current request series", ""))
			continue
		}
		// no 5xx series is no 5xx
		failures := values[service+"/"+sidecarServiceFailurePartFailures]
		share := 0.0
		if 0 < requests {
			share = failures / requests
		}
		if settings.FailureMinRequests <= requests && settings.FailureShare < share {
			findings = append(findings, finding{
				probeId: sidecarServiceFailuresProbeId, tier: tierWarn, class: sidecarServiceFailureClass, target: service,
				sustain:   settings.WarnSustain,
				symptom:   fmt.Sprintf("the service answered %.1f%% of its requests 5xx over %s", 100*share, settings.FailureWindow),
				mechanism: "Its blocks fail requests their callers retry or lose, which burns its SLO budget.",
				baseline:  "Under 1% of a service's requests answered 5xx in any 5 minutes.",
				verify:    "The 5xx share falls under 1% for two runs.",
				observed:  "requests_" + sidecarStorageRange(settings.FailureWindow) + "=" + strconv.FormatFloat(requests, 'f', 0, 64) + " failures=" + strconv.FormatFloat(failures, 'f', 0, 64),
				action:    "Read the service's logs and its blocks' readiness; a failure that burns its SLO budget pages through §30.1.",
				playbook:  "SIGNALS.md §30.2; §19.8 (DESIGN.md)",
			})
		} else {
			findings = append(findings, healthyFinding(sidecarServiceFailuresProbeId, tierWarn, sidecarServiceFailureClass, service))
		}
	}
	return findings
}

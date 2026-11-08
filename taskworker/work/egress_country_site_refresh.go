package work

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The pool refresh's country-site step: every listed country site is judged
// once a day on the week's pair tally, graduated into scoring or demoted to
// probation (model/provider_egress_country_site.go). Nothing is configured:
// the catalog lists the sites, and the evidence decides.

var egressCountrySitesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Name:      "egress_country_sites",
	Help:      "Listed country sites by state, as this taskworker's latest pool refresh left them: probation (never graduated: recorded, in the rolling quota, never scored), scored (graduated on paired evidence: counted in Quality, Speed, the index and ranking, never in the Online rule), demoted (on probation again after a demotion)",
}, []string{"state"})

var egressCountrySiteChangesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Name:      "egress_country_site_changes_total",
	Help:      "Country sites the pool refresh graduated or demoted, by change and reason: graduated/paired (no worse than the general catalog for the same providers, per network and load-weighted, measured by enough networks, no unexplained TLS failure), demoted/worse (clearly worse per network), demoted/collapsed (failing in the recent days for most loads or most networks), demoted/tls (a TLS failure on it judged a site or place fault); only networks at least two weeks old count as evidence",
}, []string{"change", "reason"})

// Registers the step's metrics with every bounded label set seeded.
func init() {
	for _, state := range model.ProviderEgressCountrySiteStates {
		egressCountrySitesGauge.WithLabelValues(state)
	}
	egressCountrySiteChangesTotal.WithLabelValues(string(model.ProviderEgressCountrySiteGraduated), model.ProviderEgressCountrySiteReasonPaired)
	for _, reason := range []string{model.ProviderEgressCountrySiteReasonWorse, model.ProviderEgressCountrySiteReasonCollapsed, model.ProviderEgressCountrySiteReasonTls} {
		egressCountrySiteChangesTotal.WithLabelValues(string(model.ProviderEgressCountrySiteDemoted), reason)
	}
	prometheus.MustRegister(egressCountrySitesGauge, egressCountrySiteChangesTotal)
}

// What the step did.
type egressCountrySiteRefreshResult struct {
	judged    int
	graduated int
	demoted   int
}

// The stored states of the listed sites at now: a site listed for the first
// time, or whose URL changed, starts over on probation.
func egressCountrySiteStates(
	listed []model.ProviderEgressCountrySiteKey,
	urls map[model.ProviderEgressCountrySiteKey]string,
	stored map[model.ProviderEgressCountrySiteKey]*model.ProviderEgressCountrySite,
	now time.Time,
) map[model.ProviderEgressCountrySiteKey]*model.ProviderEgressCountrySite {
	states := map[model.ProviderEgressCountrySiteKey]*model.ProviderEgressCountrySite{}
	for _, key := range listed {
		state, ok := stored[key]
		if !ok || state.Url != urls[key] {
			state = &model.ProviderEgressCountrySite{ProviderEgressCountrySiteKey: key, Url: urls[key], ProbationSince: now}
		}
		states[key] = state
	}
	return states
}

// Sets the state gauge from the listed sites' states.
func recordEgressCountrySiteStates(states map[model.ProviderEgressCountrySiteKey]*model.ProviderEgressCountrySite) {
	counts := map[string]int{}
	for _, state := range states {
		counts[state.State()]++
	}
	for _, state := range model.ProviderEgressCountrySiteStates {
		egressCountrySitesGauge.WithLabelValues(state).Set(float64(counts[state]))
	}
}

// Judges every listed country site at now and stores what it decided. While
// the refresh is skipped for a prober fault (skipped), which fails country and
// general sites alike, nothing is judged: the states stay as they are, a newly
// listed site is stored on probation, and the pair tally is still pruned.
func refreshEgressCountrySites(ctx context.Context, now time.Time, sites *controller.ProviderEgressSites, skipped bool) egressCountrySiteRefreshResult {
	result := egressCountrySiteRefreshResult{}
	listed, urls := model.ProviderEgressCountrySitesListed(sites.Countries)
	states := egressCountrySiteStates(listed, urls, model.GetProviderEgressCountrySites(ctx), now)
	defer model.RemoveExpiredProviderEgressCountrySiteTallies(ctx, now.Add(-model.ProviderEgressCountrySiteTallyRetention))

	write := make([]*model.ProviderEgressCountrySite, 0, len(listed))
	if skipped || len(listed) == 0 {
		for _, key := range listed {
			write = append(write, states[key])
		}
		model.SetProviderEgressCountrySites(ctx, write, listed)
		recordEgressCountrySiteStates(states)
		return result
	}

	// each site's evidence from its window start: the window's, or while on
	// probation the later start of its probation, so a demoted site graduates
	// only on evidence gathered since
	windowStart := now.Add(-model.ProviderEgressCountrySiteWindow)
	starts := map[model.ProviderEgressCountrySiteKey]time.Time{}
	names := make([]string, 0, len(listed))
	for _, key := range listed {
		start := windowStart
		if state := states[key]; !state.Scored && state.ProbationSince.After(start) {
			start = state.ProbationSince
		}
		starts[key] = start
		names = append(names, key.Name)
	}
	excluded := []server.Id{}
	for clientId := range model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx) {
		excluded = append(excluded, clientId)
	}
	// only networks two weeks old at now are evidence, for a site or against
	// it: a farm cannot make those on demand
	establishedBy := now.Add(-model.ProviderEgressCountrySiteMinNetworkAge)
	evidence := model.GetProviderEgressCountrySiteEvidence(ctx, starts, model.ProviderEgressCountrySiteRecentStart(now), establishedBy, excluded)
	findings := model.GetProviderEgressCountrySiteTlsFindings(ctx, names, windowStart, establishedBy)

	for _, key := range listed {
		judgement := model.JudgeProviderEgressCountrySite(states[key], evidence[key], findings[key.Name], now)
		states[key] = judgement.State
		write = append(write, judgement.State)
		result.judged++
		switch judgement.Change {
		case model.ProviderEgressCountrySiteGraduated:
			result.graduated++
			egressCountrySiteChangesTotal.WithLabelValues(string(judgement.Change), judgement.Reason).Inc()
			glog.Infof("[egresssites]country site %s graduated: %s\n", key, judgement.State.Judgement)
		case model.ProviderEgressCountrySiteDemoted:
			result.demoted++
			egressCountrySiteChangesTotal.WithLabelValues(string(judgement.Change), judgement.Reason).Inc()
			glog.Errorf("[egresssites]country site %s demoted to probation: %s\n", key, judgement.State.Judgement)
		default:
			if glog.V(1) {
				glog.Infof("[egresssites]country site %s: %s\n", key, judgement.State.Judgement)
			}
		}
	}
	model.SetProviderEgressCountrySites(ctx, write, listed)
	recordEgressCountrySiteStates(states)
	glog.Infof("[egresssites]country sites: %d judged, %d graduated, %d demoted\n", result.judged, result.graduated, result.demoted)
	return result
}

package controller

// Rule (a) of the SN Top board (owner decision 2026-11-20): a provider's prefix
// counts toward its network's units only if the provider passes Speed or
// Quality admission (decideProviderEgress, as the score export decided it). In
// a country whose URL probes make no UDP leg (qualityprobe.yml
// country_settings, udp_probes: false), Online membership counts instead,
// since there the probes judge HTTPS legs alone. A withheld provider shows no
// prefix (model.SnTopProvider.UnitWithheld), as a provider whose transport
// declared a proxy egress, or whose probed exit differs from its networks,
// shows none (verify_proxied_egress_model.go); the marks still apply first,
// and admission never gives a marked provider a unit back.
//
// The fold reads the newest export's decisions. With no fresh set it skips the
// tempo rather than count without the rule, as it does for a missing
// checkpoint. Without a readable probe catalog it logs the error and counts
// no country as UDP-off, so Online counts nowhere: fewer units, never more.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

const (
	// the export's admission set is missing or older than
	// model.SnTopAdmissionMaxAge at the fold
	snTopSkippedAdmissionStale = "admission_stale"
	// the admission set could not be read
	snTopSkippedAdmissionUnreadable = "admission_unreadable"
)

// The fold's rule: the export's decisions and the countries whose URL probes
// make no UDP leg.
type snTopUnitAdmission struct {
	admission         *model.SnTopAdmission
	udpProbesDisabled func(countryCode string) bool
}

// The rule from its sources at now, or the reason the fold skips the tempo.
func newSnTopUnitAdmission(
	admission *model.SnTopAdmission,
	countrySettings map[string]egresshealth.CountrySettings,
	now time.Time,
) (*snTopUnitAdmission, string) {
	if !admission.Fresh(now) {
		return nil, snTopSkippedAdmissionStale
	}
	pool := &egresshealth.Pool{CountrySettings: countrySettings}
	return &snTopUnitAdmission{
		admission:         admission,
		udpProbesDisabled: pool.UdpProbesDisabled,
	}, ""
}

// Reads the rule's sources for the fold at now.
func loadSnTopUnitAdmission(ctx context.Context, now time.Time) (*snTopUnitAdmission, string) {
	admission, err := model.ReadSnTopAdmission(ctx)
	if err != nil {
		glog.Errorf("[sntop]the export's admission set is unreadable: %s\n", err)
		return nil, snTopSkippedAdmissionUnreadable
	}
	var countrySettings map[string]egresshealth.CountrySettings
	if sites, err := LoadProviderEgressSites(); err == nil {
		countrySettings = sites.CountrySettings
	} else {
		glog.Errorf("[sntop]qualityprobe.yml is unavailable for its udp country settings; no country counts Online: %s\n", err)
	}
	return newSnTopUnitAdmission(admission, countrySettings, now)
}

// Withholds the unit of every provider the rule does not admit, a provider the
// export did not decide included, and returns how many it withheld.
func snTopApplyUnitAdmission(providers []model.SnTopProvider, unitAdmission *snTopUnitAdmission) (withheld int) {
	for i := range providers {
		provider := &providers[i]
		if unitAdmission.admission.Providers[provider.ClientId].CountsUnits(unitAdmission.udpProbesDisabled) {
			continue
		}
		provider.UnitWithheld = true
		withheld += 1
	}
	return
}

// Tier B's view of the fold's providers: a withheld provider has no prefix for
// the representatives either, as a zero pin has none.
func snTopVerifyCohortRankedProviders(providers []model.SnTopProvider) []model.VerifyCohortRankedProvider {
	ranked := make([]model.VerifyCohortRankedProvider, 0, len(providers))
	for _, provider := range providers {
		ranked = append(ranked, model.VerifyCohortRankedProvider{
			ClientId:   provider.ClientId,
			NetworkId:  provider.NetworkId,
			Eligible:   provider.Eligible,
			PrefixHash: provider.PrefixHash,
			HasPrefix:  provider.HasPrefix && !provider.UnitWithheld,
		})
	}
	return ranked
}

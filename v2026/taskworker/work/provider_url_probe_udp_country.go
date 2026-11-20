// A country whose national firewall makes UDP results say nothing about the
// provider turns UDP probes off (qualityprobe.yml country_settings
// udp_probes: false): its exits make no HTTP/3 leg, and the legs not made are
// counted here by country.
package work

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// URL loads of a URL that declares h3 whose HTTP/3 leg was not made because
// the provider's country disables UDP probes, by the country of the
// provider's claim: the place the turn drew its URL by and the ingest judges
// HTTP/3 legs by.
var urlProbeUdpCountryDisabled = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_url_probe_udp_country_disabled_total",
	Help: "Measured URL loads of acknowledged outcomes whose URL declares h3 and whose HTTP/3 (UDP) leg was not made because the provider's country sets udp_probes: false in qualityprobe.yml country_settings, by the country of the provider's claim. Their UDP is not measured, never a failure or a pass: the load is judged on its HTTPS leg alone, adds no HTTP/3 place evidence and stays out of the HTTP/3 common-mode window",
}, []string{"country"})

func init() {
	prometheus.MustRegister(urlProbeUdpCountryDisabled)
}

// Counts each measured load of an acknowledged turn whose HTTP/3 legs the
// provider's country turned off, once whichever family's URL declared h3 (the
// load's own, or the stand-in its IPv6 legs load), under country(), which is
// asked only when there is one. A canary, and a load whose HTTPS leg was not
// measured, says nothing about either transport and is not counted, as the
// transport series do not count it.
func observeUrlProbeUdpCountryDisabled(result *egresshealth.Result, country func() string) {
	if result == nil {
		return
	}
	for _, check := range result.Checks {
		if (check.UdpProbesDisabled || check.Ipv6UdpProbesDisabled) && !check.Canary && !check.NotMeasured {
			urlProbeUdpCountryDisabled.WithLabelValues(country()).Inc()
		}
	}
}

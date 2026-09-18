package work

// What the extender dns tick actually published.
//
// The sampled sets are recomputed every tick and never stored, so the database
// can only say what was ELIGIBLE -- the active address pool. Only the publisher
// knows what it wrote. These gauges are therefore the sole source for "what is
// in dns right now", which is what makes them worth having rather than a
// derived view of the pool.
//
// Every label is a bounded enum, so the series count is fixed no matter how
// large the extender population grows. Registered with the default prometheus
// registry, which `server.StartStatsPusher` pushes to mimir automatically.
//
// Note the series are per process and keyed by instance, so a dashboard must
// aggregate (`max`, `sum`) rather than select a bare series: during a warp
// redeploy the draining and starting taskworkers both push.

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var extenderDnsSetsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_dns_sets",
	Help:      "Extender dns record sets published by the last tick, per record type",
}, []string{"record_type"})

var extenderDnsAddressesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_dns_addresses",
	Help:      "Extender addresses published by the last tick, per ip version",
}, []string{"ip_version"})

var extenderDnsTxtRecordsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_dns_txt_records",
	Help:      "Signed extender records published across all TXT sets by the last tick",
})

var extenderDnsExtendersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_dns_extenders",
	Help:      "Distinct extenders reachable through the addresses the last tick published",
})

func init() {
	prometheus.MustRegister(
		extenderDnsSetsGauge,
		extenderDnsAddressesGauge,
		extenderDnsTxtRecordsGauge,
		extenderDnsExtendersGauge,
	)
}

// observeExtenderDnsSets records what one tick published.
//
// Called with the desired sets rather than the apply result because the desired
// sets are what the zone converges to; a set that failed to apply is an error
// the publisher already reports, and zeroing the gauges on it would read as "no
// extenders in dns" rather than "one tick did not land".
//
// Every label value is published on every tick, including zeros, so a record
// type or family that goes empty is a zero rather than a series that silently
// stops updating and keeps showing its last value.
func observeExtenderDnsSets(
	desiredSets []*extenderDnsRecordSet,
	addresses []*model.NetworkExtenderDnsAddress,
) {
	ipExtenderIds := map[string]server.Id{}
	for _, address := range addresses {
		ipExtenderIds[address.Ip.String()] = address.ExtenderId
	}

	setsByType := map[string]int{"A": 0, "AAAA": 0, "TXT": 0}
	addressesByVersion := map[string]int{"4": 0, "6": 0}
	txtRecords := 0
	extenders := map[server.Id]bool{}

	for _, desiredSet := range desiredSets {
		switch {
		case 0 < len(desiredSet.records):
			setsByType["TXT"] += 1
			txtRecords += len(desiredSet.records)
		case desiredSet.ipVersion == 4:
			setsByType["A"] += 1
			addressesByVersion["4"] += len(desiredSet.ips)
		case desiredSet.ipVersion == 6:
			setsByType["AAAA"] += 1
			addressesByVersion["6"] += len(desiredSet.ips)
		}
		for _, ip := range desiredSet.ips {
			if extenderId, ok := ipExtenderIds[ip]; ok {
				extenders[extenderId] = true
			}
		}
	}

	for recordType, count := range setsByType {
		extenderDnsSetsGauge.WithLabelValues(recordType).Set(float64(count))
	}
	for ipVersion, count := range addressesByVersion {
		extenderDnsAddressesGauge.WithLabelValues(ipVersion).Set(float64(count))
	}
	extenderDnsTxtRecordsGauge.Set(float64(txtRecords))
	extenderDnsExtendersGauge.Set(float64(len(extenders)))
}

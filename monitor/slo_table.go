package monitor

// §19.8's SLOs (D-OPS-9), copied row by row, each over 30 days at all hours (owner decision 143). A row
// joins when its service does: monitor.yml's sidecar_services block lists the services whose rows run
// (SidecarSloSettings), and an unlisted service's row reads nothing.
//
// Each row holds its objectives. An objective's budget is the share of its 30 days' events that may be
// bad, 1 - target: a 5xx answer for an availability objective, an event slower than its bound for a
// latency one, whose target is the percentile's own share (p99 allows 1%). Only an objective whose
// service publishes the series it needs is measured; the others are written here with what they wait
// for, and read unobservable, never healthy, until their owner publishes it. Labels stay §19.8's: route
// template, result, code, cluster and tier, every query summing them away, so no SLO series, finding or
// page carries a network, repo or client (D-OPS-8, I-13).

// How an objective counts its events.
type sloObjectiveKind int

const (
	// urnetwork_<svc>_http_requests_total{route,code} (sidecarhost/route_metrics.go): a 5xx is bad
	sloObjectiveAvailability sloObjectiveKind = iota
	// a latency histogram: an event over the bound is bad, read from its le bucket
	sloObjectiveLatency
	// the SLI is not published yet: the objective reads unobservable
	sloObjectiveUnmeasured
)

// One objective of a row.
type sloObjective struct {
	// what §19.8 calls it, which names the finding's target with the service
	name string
	kind sloObjectiveKind
	// the share of the 30 days' events that must be good
	target float64
	// sloObjectiveLatency: the histogram's base name, without _bucket or _count
	histogram string
	// sloObjectiveLatency: the bound in seconds, which must be one of the histogram's buckets
	boundSeconds float64
	// sloObjectiveUnmeasured: what the objective waits for
	waitsFor string
}

// One §19.8 row: a service and its objectives.
type sloRow struct {
	// as warp and urnetwork_<svc>_* name it
	service string
	// the row as §19.8 writes it
	terms      string
	objectives []sloObjective
}

// §19.8's SLO table, in its order. mxedge and smtpedge share a row there and are a row each here, since
// each joins on its own.
var sloTable = []sloRow{
	{
		service: "evs",
		terms:   "99.9% non-5xx; head -> hint p99 <= 2 s",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveAvailability, target: 0.999},
			{name: "head-to-hint", kind: sloObjectiveUnmeasured, target: 0.99, waitsFor: "evs's head-to-hint histogram (EVS1), with a bucket at 2 s"},
		},
	},
	{
		service: "gkm",
		terms:   "99.9%; commit p99 <= 300 ms",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveAvailability, target: 0.999},
			{name: "commit", kind: sloObjectiveUnmeasured, target: 0.99, waitsFor: "gkm's commit histogram (KEYS), with a bucket at 0.3 s; urnetwork_gkm_request_seconds has none"},
		},
	},
	{
		service: "sockets",
		terms:   "99.95%; Find p99 <= 50 ms",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveAvailability, target: 0.9995},
			{name: "find", kind: sloObjectiveLatency, target: 0.99, histogram: "urnetwork_sockets_find_seconds", boundSeconds: 0.05},
		},
	},
	{
		service: "tlsedge",
		terms:   "99.9% of connections reach a listener; warm setup p95 <= +300 ms",
		objectives: []sloObjective{
			{name: "listener-reached", kind: sloObjectiveUnmeasured, target: 0.999, waitsFor: "tlsedge's connection outcomes (TLS)"},
			{name: "warm-setup", kind: sloObjectiveUnmeasured, target: 0.95, waitsFor: "tlsedge's setup latency against a direct connection (TLS)"},
		},
	},
	{
		service: "mxedge",
		terms:   "99.9% of splices, 99.5% of preambles",
		objectives: []sloObjective{
			{name: "splices", kind: sloObjectiveUnmeasured, target: 0.999, waitsFor: "mxedge's splice outcomes (MAIL)"},
			{name: "preambles", kind: sloObjectiveUnmeasured, target: 0.995, waitsFor: "mxedge's preamble outcomes (MAIL)"},
		},
	},
	{
		service: "smtpedge",
		terms:   "99.9% of splices, 99.5% of preambles",
		objectives: []sloObjective{
			{name: "splices", kind: sloObjectiveUnmeasured, target: 0.999, waitsFor: "smtpedge's splice outcomes (SMTP)"},
			{name: "preambles", kind: sloObjectiveUnmeasured, target: 0.995, waitsFor: "smtpedge's preamble outcomes (SMTP)"},
		},
	},
	{
		service: "dns",
		terms:   "write -> served at both NS p99 <= 60 s (R-DNS-7)",
		objectives: []sloObjective{
			{name: "write-to-served", kind: sloObjectiveUnmeasured, target: 0.99, waitsFor: "dns's write-to-served histogram at both NS (DNS), with a bucket at 60 s"},
		},
	},
	{
		service: "certs",
		terms:   "renewed with 1/3 of their life left",
		objectives: []sloObjective{
			{name: "renewal", kind: sloObjectiveUnmeasured, target: 1, waitsFor: "certs' renewal margins (TLS)"},
		},
	},
	{
		service: "wake",
		terms:   "wakes p99 <= 2 s after their trigger",
		objectives: []sloObjective{
			{name: "wake-latency", kind: sloObjectiveUnmeasured, target: 0.99, waitsFor: "the push sender's trigger-to-wake histogram (SDK), with a bucket at 2 s"},
		},
	},
	{
		service: "projections",
		terms:   "per §8.9",
		objectives: []sloObjective{
			{name: "projection", kind: sloObjectiveUnmeasured, target: 1, waitsFor: "§8.9's projection freshness (ACL)"},
		},
	},
	{
		service: "file-cluster",
		terms:   "99.95%",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveUnmeasured, target: 0.9995, waitsFor: "the clusters' request outcomes in Mimir (EVSB: no scrape brings MinIO's node metrics in yet)"},
		},
	},
}

// The row of a service, false for a service §19.8 gives no SLO.
func sloRowOf(service string) (sloRow, bool) {
	for _, row := range sloTable {
		if row.service == service {
			return row, true
		}
	}
	return sloRow{}, false
}

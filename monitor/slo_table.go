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
	// A result-labelled counter. Its good and bad outcomes define the eligible events.
	sloObjectiveOutcome
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
	metric       string
	goodResults  string
	badResults   string
	// Recurring monitor observations also need evidence that the probe ran recently.
	probed bool
	// sloObjectiveUnmeasured: what the objective waits for
	waitsFor string
}

func sloOutcome(name string, target float64, metric string, good string, bad string) sloObjective {
	return sloObjective{name: name, kind: sloObjectiveOutcome, target: target, metric: metric, goodResults: good, badResults: bad}
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
			sloOutcome("head-to-hint", 0.99, "urnetwork_evs_head_hints_total", "on_time", "late|failed"),
		},
	},
	{
		service: "gkm",
		terms:   "99.9%; commit p99 <= 300 ms",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveAvailability, target: 0.999},
			{name: "commit", kind: sloObjectiveLatency, target: 0.99, histogram: "urnetwork_gkm_commit_seconds", boundSeconds: 0.3},
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
			sloOutcome("listener-reached", 0.999, "urnetwork_tlsedge_connections_total", "spliced", "process_cap|not_ready|busy|no_listener|negative_no_listener|unavailable|setup_failed"),
			{name: "warm-setup", kind: sloObjectiveLatency, target: 0.95, histogram: "urnetwork_monitor_tlsedge_warm_setup_overhead_seconds", boundSeconds: 0.3, probed: true},
		},
	},
	{
		service: "mxedge",
		terms:   "99.9% of splices, 99.5% of preambles",
		objectives: []sloObjective{
			sloOutcome("splices", 0.999, "urnetwork_mxedge_sessions_total", "splice_closed", "not_ready|route_unavailable|no_receiver|receiver_busy|sockets_unavailable|setup_failed|splice_(receiver_reset|sender_reset|idle|slow|session_timeout|drain)"),
			sloOutcome("preambles", 0.995, "urnetwork_mxedge_preambles_total", "router_ready", "router_ended"),
		},
	},
	{
		service: "mxedge-platform",
		terms:   "99.9% of eligible messages taken, 99.5% of preambles",
		objectives: []sloObjective{
			sloOutcome("intake", 0.999, "urnetwork_mxedge_platform_messages_total", "taken", "failed|buffer_full"),
			sloOutcome("preambles", 0.995, "urnetwork_mxedge_preambles_total", "platform_ready", "platform_ended"),
		},
	},
	{
		service: "smtpedge",
		terms:   "99.9% of splices, 99.5% of preambles",
		objectives: []sloObjective{
			sloOutcome("splices", 0.999, "urnetwork_smtpedge_connections_total", "sent", "refused|timeout|ended"),
			sloOutcome("preambles", 0.995, "urnetwork_smtpedge_preambles_total", "ok", "no_mx|no_tls|try_later|keys"),
		},
	},
	{
		service: "dns",
		terms:   "write -> served at both NS p99 <= 60 s (R-DNS-7)",
		objectives: []sloObjective{
			{name: "write-to-served", kind: sloObjectiveOutcome, target: 0.99, metric: "urnetwork_monitor_dns_propagation_total", goodResults: "on_time", badResults: "late", probed: true},
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
	// DESIGN-COMPUTE §14.2's two services (CSRV-M12-06), which the compute design gives no SLO figure: the row is a
	// reading, evs's availability line, until a design-records fold gives theirs. compute's block serves through
	// sidecarhost.Run, which publishes its route metrics; searchsigner's serves on a port of its own (CSRV-M13-01).
	{
		service: "compute",
		terms:   "99.9% non-5xx (a reading)",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveAvailability, target: 0.999},
		},
	},
	{
		service: "searchsigner",
		terms:   "99.9% non-5xx (a reading)",
		objectives: []sloObjective{
			{name: "availability", kind: sloObjectiveUnmeasured, target: 0.999, waitsFor: "searchsigner's route metrics on its own port (CSRV-M13-01)"},
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

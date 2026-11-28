package server

// The Go collector of the default prometheus registry, which every service's
// stats push gathers (grafana.go). client_golang registers its default Go
// collector there at its own init: the go_memstats_* metrics,
// go_gc_duration_seconds, go_goroutines, go_threads, go_info, and three
// runtime/metrics gauges, go_gc_gogc_percent, go_gc_gomemlimit_bytes and
// go_sched_gomaxprocs_threads. This package's init replaces it with one that
// exports all of those and adds the CPU time of each runtime class and the
// scheduler latency (LBALTOPT1.md R5), so the GC's share of a process's CPU
// and its goroutines' wait to run can be read before GOGC or GOMAXPROCS is
// tuned. Every process that links this package exports them.
//
// With Go 1.27 that is 21 series a process, none labeled:
//   - go_cpu_classes_<class>_cpu_seconds_total: 11 counters, gc_total,
//     gc_mark_assist, gc_mark_dedicated, gc_mark_idle, gc_pause, idle,
//     scavenge_total, scavenge_assist, scavenge_background, total and user;
//   - go_sched_latencies_seconds: a histogram of 7 buckets, client_golang's
//     powers of ten from 64 ns to 117 ms, and +Inf, with its sum and count.
//
// Reading them:
//   - the runtime updates the CPU classes at each GC cycle's start and end,
//     not continuously, so a rate needs a window of several GC periods;
//   - total is GOMAXPROCS times the process's wall time, not the CPU it used,
//     so the GC's share of the CPU the process used is gc_total over total
//     less idle;
//   - the histogram's sum is client_golang's estimate from each bucket's lower
//     bound, an underestimate: read quantiles, not sum over count.

import (
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// The runtime/metrics exported beyond client_golang's default collector, by
// runtime/metrics name. A rule adds to client_golang's default rule, which
// keeps go_gc_gomemlimit_bytes, and to its memstats-style metrics.
func goRuntimeMetricsRules() []collectors.GoRuntimeMetricsRule {
	return []collectors.GoRuntimeMetricsRule{
		// the CPU time of each class: the GC's, by phase, the scavenger's,
		// user Go code's, idle, and the total available
		{Matcher: regexp.MustCompile(`^/cpu/classes/`)},
		// how long runnable goroutines wait for a P
		{Matcher: regexp.MustCompile(`^/sched/latencies:seconds$`)},
		// also in client_golang's default rule, kept here so the export does
		// not depend on it
		{Matcher: regexp.MustCompile(`^/gc/gogc:percent$`)},
		{Matcher: regexp.MustCompile(`^/sched/gomaxprocs:threads$`)},
	}
}

// The Go collector of the default registry.
func newGoRuntimeCollector() prometheus.Collector {
	return collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(goRuntimeMetricsRules()...))
}

func init() {
	// client_golang's init, which runs before this package's, registered its
	// default Go collector. Unregister finds a collector by the metrics it
	// describes, so a new default collector finds the registered one. The
	// replacement describes the same metrics and more, and registering it
	// beside the default would be refused as a duplicate.
	prometheus.Unregister(collectors.NewGoCollector())
	prometheus.MustRegister(newGoRuntimeCollector())
}

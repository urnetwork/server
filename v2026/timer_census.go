// A sampled census of the changes a connect process makes to Go runtime
// timers, by the code that makes them (the site) and how far ahead each
// changed timer is due (the horizon): LBALTOPT2.md R14(a), CPUOPT7.md §11.
//
// Why the horizon. A timer changed while it is in a P's heap, pending or as a
// zombie a stop or an abandoned select left there, is only marked, and the
// heap keeps its earliest modified deadline (runtime/time.go, modify). Once
// that deadline passes, the P's next timer check walks its whole heap
// (timers.adjust), whatever the change cost. So a P walks about once per wait
// for its next modification plus that modification's horizon, and the
// frequent modifications with the shortest horizons set the walk rate, which
// a CPU profile can't name. A socket deadline set while the descriptor's
// timer is pending or a zombie is such a modification
// (poll_runtime_pollSetDeadline). A deadline cleared, or set at or before
// now, stops the descriptor's timer instead, which leaves a zombie and sets
// off nothing, and a new timer is an insert.
//
// What is counted. Every call at a fixed set of sites that may move a pending
// or zombie runtime timer, in one of six horizon buckets, and every stop or
// clear there in a seventh, "stop". A sampled call counts as SamplePeriod
// calls. Sites at different layers overlap: front_* counts every deadline set
// on a public TCP socket, the h1 transport's (h1_*) among them. The connect
// library and quic-go count their own sites through their TimerCensus hooks,
// which the connect service installs with this census, mapped to the client_*,
// quic_* and http3_* sites (connect/timer_census.go). New timers are not
// counted: an insert never sets off a walk.
//
// The cost. Off, which it is in every process but connect, an atomic load a
// call. On, the calling thread's random number (math/rand/v2's per-M source)
// and a mask, with no lock and no allocation; only a sampled call reads the
// clock and adds to a counter.
package server

import (
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// A place that changes a runtime timer on a connect hot path. The set is
// fixed, and so is the metric's site label: timerCensusSiteNames.
type TimerCensusSite int

const (
	// the exchange link's per-message read deadline (exchangeDeadline.refresh)
	TimerCensusSiteExchangeReadDeadline TimerCensusSite = iota
	// the exchange link's per-write deadline (exchangeDeadline.refresh)
	TimerCensusSiteExchangeWriteDeadline
	// any read deadline on a public TCP socket of the lb_alt front
	// (frontTrackedConn): net/http's, h2's, TLS's and the h1 transport's
	TimerCensusSiteFrontReadDeadline
	// any owner's write deadline on a public TCP socket of the front
	TimerCensusSiteFrontWriteDeadline
	// the front's send_timeout bound of a write its owner left without a
	// deadline, set before the write and cleared after it
	TimerCensusSiteFrontWriteBound
	// the nginx front's http.ResponseController deadlines: a response write's
	// send_timeout, a request body's read deadline, the lingering read. On h1
	// the socket's too (front_*); on h2 they move the stream's timer inside
	// net/http; on h3 they're a stream field
	TimerCensusSiteFrontResponseDeadline
	// the nginx front's deadlines on its pooled connections to the api: each
	// upstream read and write, and a tunnel's
	TimerCensusSiteFrontUpstreamDeadline
	// the h1 transport's read deadlines (readConnectH1PooledWithDeadline)
	TimerCensusSiteH1ReadDeadline
	// the h1 transport's write deadlines (writeConnectH1Batch)
	TimerCensusSiteH1WriteDeadline
	// the h1 writer's heartbeat timer, reset as its queue goes idle
	TimerCensusSiteH1PingTimer
	// the h3 writer's heartbeat timer
	TimerCensusSiteH3PingTimer
	// the write timer of an exchange send into a full queue (sendPooledMessage):
	// a forward's, and a resident transport's
	TimerCensusSiteForwardWriteTimer
	// go-redis's read deadlines, on every pooled connection
	TimerCensusSiteRedisReadDeadline
	// go-redis's write deadlines
	TimerCensusSiteRedisWriteDeadline
	// pgx's deadlines, on every pooled connection
	TimerCensusSitePgDeadline
	// the connect library's reused wait timers (resetOrCreateTimer) at
	// callers without a site of their own
	TimerCensusSiteClientTimer
	// the connect library's route write wait
	TimerCensusSiteClientRouteWriteTimer
	// a receive sequence's Pack handoff wait
	TimerCensusSiteClientReceiveHandoffTimer
	// a receive sequence's delivery wait
	TimerCensusSiteClientReceiveDeliveryTimer
	// a receive sequence's ACK compression wait
	TimerCensusSiteClientAckCompressTimer
	// a send Pack call's admission waits
	TimerCensusSiteClientPackWaitTimer
	// a send sequence's window pacing wait
	TimerCensusSiteClientWindowPacingTimer
	// a sequence loop's deadline timer, which only ever stops a pending timer
	TimerCensusSiteClientSequenceDeadlineTimer
	// quic-go's timer of a stream Read or Peek waiting under a deadline
	TimerCensusSiteQuicStreamReadTimer
	// quic-go's timer of a stream Write waiting under a deadline
	TimerCensusSiteQuicStreamWriteTimer
	// a QUIC connection's run loop timer, which only ever stops a pending timer
	TimerCensusSiteQuicConnTimer
	// an http3 server connection's idle timer
	TimerCensusSiteHttp3IdleTimer

	timerCensusSiteCount
)

// The site label of each site.
var timerCensusSiteNames = [timerCensusSiteCount]string{
	TimerCensusSiteExchangeReadDeadline:        "exchange_read_deadline",
	TimerCensusSiteExchangeWriteDeadline:       "exchange_write_deadline",
	TimerCensusSiteFrontReadDeadline:           "front_read_deadline",
	TimerCensusSiteFrontWriteDeadline:          "front_write_deadline",
	TimerCensusSiteFrontWriteBound:             "front_write_bound",
	TimerCensusSiteFrontResponseDeadline:       "front_response_deadline",
	TimerCensusSiteFrontUpstreamDeadline:       "front_upstream_deadline",
	TimerCensusSiteH1ReadDeadline:              "h1_read_deadline",
	TimerCensusSiteH1WriteDeadline:             "h1_write_deadline",
	TimerCensusSiteH1PingTimer:                 "h1_ping_timer",
	TimerCensusSiteH3PingTimer:                 "h3_ping_timer",
	TimerCensusSiteForwardWriteTimer:           "forward_write_timer",
	TimerCensusSiteRedisReadDeadline:           "redis_read_deadline",
	TimerCensusSiteRedisWriteDeadline:          "redis_write_deadline",
	TimerCensusSitePgDeadline:                  "pg_deadline",
	TimerCensusSiteClientTimer:                 "client_timer",
	TimerCensusSiteClientRouteWriteTimer:       "client_route_write_timer",
	TimerCensusSiteClientReceiveHandoffTimer:   "client_receive_handoff_timer",
	TimerCensusSiteClientReceiveDeliveryTimer:  "client_receive_delivery_timer",
	TimerCensusSiteClientAckCompressTimer:      "client_ack_compress_timer",
	TimerCensusSiteClientPackWaitTimer:         "client_pack_wait_timer",
	TimerCensusSiteClientWindowPacingTimer:     "client_window_pacing_timer",
	TimerCensusSiteClientSequenceDeadlineTimer: "client_sequence_deadline_timer",
	TimerCensusSiteQuicStreamReadTimer:         "quic_stream_read_timer",
	TimerCensusSiteQuicStreamWriteTimer:        "quic_stream_write_timer",
	TimerCensusSiteQuicConnTimer:               "quic_conn_timer",
	TimerCensusSiteHttp3IdleTimer:              "http3_idle_timer",
}

// The site label.
func (self TimerCensusSite) String() string {
	if self < 0 || timerCensusSiteCount <= self {
		return fmt.Sprintf("TimerCensusSite(%d)", int(self))
	}
	return timerCensusSiteNames[self]
}

// How far ahead a changed timer is due. The buckets below 100 ms are finer
// than R14(a)'s first, since pacing, ACK compression, loss recovery and short
// operation timeouts live there.
type timerCensusHorizon int

const (
	timerCensusHorizonUnder1Ms timerCensusHorizon = iota
	timerCensusHorizon1MsTo10Ms
	timerCensusHorizon10MsTo100Ms
	timerCensusHorizon100MsTo1S
	timerCensusHorizon1STo10S
	timerCensusHorizonFrom10S
	// a timer stopped or a deadline cleared, which leaves a zombie and sets
	// off no walk
	timerCensusHorizonStop

	timerCensusHorizonCount
)

// The horizon label of each bucket.
var timerCensusHorizonNames = [timerCensusHorizonCount]string{
	timerCensusHorizonUnder1Ms:    "lt_1ms",
	timerCensusHorizon1MsTo10Ms:   "1ms_10ms",
	timerCensusHorizon10MsTo100Ms: "10ms_100ms",
	timerCensusHorizon100MsTo1S:   "100ms_1s",
	timerCensusHorizon1STo10S:     "1s_10s",
	timerCensusHorizonFrom10S:     "ge_10s",
	timerCensusHorizonStop:        "stop",
}

// The bucket of a timer set to fire d from now. A nonpositive d is due at once.
func timerCensusHorizonOf(d time.Duration) timerCensusHorizon {
	switch {
	case d < time.Millisecond:
		return timerCensusHorizonUnder1Ms
	case d < 10*time.Millisecond:
		return timerCensusHorizon1MsTo10Ms
	case d < 100*time.Millisecond:
		return timerCensusHorizon10MsTo100Ms
	case d < time.Second:
		return timerCensusHorizon100MsTo1S
	case d < 10*time.Second:
		return timerCensusHorizon1STo10S
	default:
		return timerCensusHorizonFrom10S
	}
}

var timerCensusModifications = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "urnetwork_connect_timer_modifications_total",
		Help: "Sampled changes to Go runtime timers at connect's hot sites (CPUOPT7 §11, LBALTOPT2 R14(a)): each call that may move a pending or zombie timer, by how far ahead the timer is then due (horizon), and each stop or deadline clear (horizon=stop). A sampled call counts as the sample period. A P walks its whole timer heap when the earliest moved deadline arrives, so the shortest horizons among frequent changes set the walk rate",
	},
	[]string{"site", "horizon"},
)

func init() {
	prometheus.MustRegister(timerCensusModifications)
}

type TimerCensusSettings struct {
	// One call in SamplePeriod is counted, as SamplePeriod calls. A power of
	// two: 64 keeps a counted call to a few ns, and 1 counts every call, as
	// tests do. Non-positive leaves the census off.
	SamplePeriod int
}

func DefaultTimerCensusSettings() *TimerCensusSettings {
	return &TimerCensusSettings{
		SamplePeriod: 64,
	}
}

// A running census: the sampling and a counter for every site and horizon.
// Immutable once installed.
type timerCensus struct {
	// a sampled call has these bits of a random number clear
	mask     uint32
	weight   float64
	counters [timerCensusSiteCount][timerCensusHorizonCount]prometheus.Counter
}

// The census the counted calls read. Process-wide, as the runtime's timer
// heaps are; nil while no census runs.
var activeTimerCensus atomic.Pointer[timerCensus]

// Makes a census whose counters come from counterVec, with every site and
// horizon's series made up front, so the label set is fixed from the start.
func newTimerCensus(settings *TimerCensusSettings, counterVec *prometheus.CounterVec) (*timerCensus, error) {
	period := settings.SamplePeriod
	if period <= 0 || period&(period-1) != 0 || math.MaxUint32 < uint64(period) {
		return nil, fmt.Errorf("the timer census's sample period %d is not a positive power of two", period)
	}
	census := &timerCensus{
		mask:   uint32(period - 1),
		weight: float64(period),
	}
	for site := range timerCensusSiteCount {
		for horizon := range timerCensusHorizonCount {
			census.counters[site][horizon] = counterVec.WithLabelValues(timerCensusSiteNames[site], timerCensusHorizonNames[horizon])
		}
	}
	return census, nil
}

// Installs census, and returns the stop, which removes it if it is still the
// installed one.
func installTimerCensus(census *timerCensus) func() {
	activeTimerCensus.Store(census)
	return func() {
		activeTimerCensus.CompareAndSwap(census, nil)
	}
}

// StartTimerCensus counts this process's timer changes at the server's sites
// into urnetwork_connect_timer_modifications_total, and returns the stop. A
// non-positive SamplePeriod leaves it off. The connect service starts it, with
// the connect library's and quic-go's hooks (connect/timer_census.go).
func StartTimerCensus(settings *TimerCensusSettings) (func(), error) {
	if settings.SamplePeriod <= 0 {
		return func() {}, nil
	}
	census, err := newTimerCensus(settings, timerCensusModifications)
	if err != nil {
		return nil, err
	}
	return installTimerCensus(census), nil
}

// The running census when this call is sampled, or nil.
func sampledTimerCensus() *timerCensus {
	census := activeTimerCensus.Load()
	if census == nil {
		return nil
	}
	if mathrand.Uint32()&census.mask != 0 {
		return nil
	}
	return census
}

// CountTimerReset counts a Reset at site that arms its timer d from now.
func CountTimerReset(site TimerCensusSite, d time.Duration) {
	if census := sampledTimerCensus(); census != nil {
		census.counters[site][timerCensusHorizonOf(d)].Add(census.weight)
	}
}

// CountTimerDeadline counts a socket deadline set at site to deadline. A zero
// deadline, or one at or before now, stops the descriptor's timer, and counts
// as a stop.
func CountTimerDeadline(site TimerCensusSite, deadline time.Time) {
	if census := sampledTimerCensus(); census != nil {
		horizon := timerCensusHorizonStop
		if !deadline.IsZero() {
			if d := time.Until(deadline); 0 < d {
				horizon = timerCensusHorizonOf(d)
			}
		}
		census.counters[site][horizon].Add(census.weight)
	}
}

// CountTimerDeadlineAfter counts a socket deadline set at site timeout from
// now, as the caller computed it, without reading the clock. A nonpositive
// timeout stops the descriptor's timer, and counts as a stop.
func CountTimerDeadlineAfter(site TimerCensusSite, timeout time.Duration) {
	if census := sampledTimerCensus(); census != nil {
		horizon := timerCensusHorizonStop
		if 0 < timeout {
			horizon = timerCensusHorizonOf(timeout)
		}
		census.counters[site][horizon].Add(census.weight)
	}
}

// CountTimerStop counts a timer stopped, or a deadline cleared, at site.
func CountTimerStop(site TimerCensusSite) {
	if census := sampledTimerCensus(); census != nil {
		census.counters[site][timerCensusHorizonStop].Add(census.weight)
	}
}

// RecordTimerChange records count changes at site that a library's own census
// sampled: a timer set to fire horizon from now, or, when stop, a timer
// stopped. The connect library's and quic-go's TimerCensus.Record.
func RecordTimerChange(site TimerCensusSite, horizon time.Duration, stop bool, count int) {
	census := activeTimerCensus.Load()
	if census == nil {
		return
	}
	bucket := timerCensusHorizonStop
	if !stop {
		bucket = timerCensusHorizonOf(horizon)
	}
	census.counters[site][bucket].Add(float64(count))
}

// The running census's count at site and horizon, the bucket's label, or zero
// when no census runs. For tests.
func Testing_TimerCensusCount(site TimerCensusSite, horizon string) float64 {
	census := activeTimerCensus.Load()
	if census == nil {
		return 0
	}
	for bucket, name := range timerCensusHorizonNames {
		if name == horizon {
			metric := &dto.Metric{}
			if err := census.counters[site][bucket].Write(metric); err != nil {
				return 0
			}
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}

// The running census's counts, by site and horizon label, or nil when no
// census runs. For the performance harness's report.
func Testing_TimerCensusCounts() map[string]map[string]float64 {
	if activeTimerCensus.Load() == nil {
		return nil
	}
	counts := map[string]map[string]float64{}
	for site := range timerCensusSiteCount {
		siteCounts := map[string]float64{}
		for _, horizon := range timerCensusHorizonNames {
			siteCounts[horizon] = Testing_TimerCensusCount(site, horizon)
		}
		counts[timerCensusSiteNames[site]] = siteCounts
	}
	return counts
}

// Runs a census of its own, counting every call into counters no other census
// shares, until the returned stop. For tests in other packages, whose counts
// start at zero.
func Testing_StartTimerCensus() func() {
	census, err := newTimerCensus(&TimerCensusSettings{SamplePeriod: 1}, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "timer_census_testing_total",
		},
		[]string{"site", "horizon"},
	))
	if err != nil {
		panic(err)
	}
	return installTimerCensus(census)
}

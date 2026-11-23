// The timer census (timer_census.go): its fixed label set, the horizon
// buckets, one count per counted call in its bucket, the sampling, and the
// Redis and pgx sockets that count their deadlines. Each test runs a census of
// its own, so its counts start at zero and no other test's census sees them.
package server

import (
	"context"
	"errors"
	"net"
	"regexp"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A census of the test's own, installed until the test ends.
type testTimerCensus struct {
	counterVec *prometheus.CounterVec
}

func startTestTimerCensus(t testing.TB, samplePeriod int) *testTimerCensus {
	t.Helper()
	counterVec := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "timer_census_test_total",
		},
		[]string{"site", "horizon"},
	)
	census, err := newTimerCensus(&TimerCensusSettings{SamplePeriod: samplePeriod}, counterVec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(installTimerCensus(census))
	return &testTimerCensus{
		counterVec: counterVec,
	}
}

func (self *testTimerCensus) count(site TimerCensusSite, horizon string) float64 {
	return testutil.ToFloat64(self.counterVec.WithLabelValues(site.String(), horizon))
}

// Every bucket's count at site.
func (self *testTimerCensus) counts(site TimerCensusSite) map[string]float64 {
	counts := map[string]float64{}
	for _, horizon := range timerCensusHorizonNames {
		counts[horizon] = self.count(site, horizon)
	}
	return counts
}

// Requires that between before and now, site counted want in the horizon
// bucket and nothing in any other.
func (self *testTimerCensus) requireCounted(t testing.TB, what string, site TimerCensusSite, before map[string]float64, horizon string, want float64) {
	t.Helper()
	self.requireCounts(t, what, site, before, map[string]float64{horizon: want})
}

// Requires that between before and now, site counted the wanted count in each
// bucket, and nothing in a bucket wants leaves out.
func (self *testTimerCensus) requireCounts(t testing.TB, what string, site TimerCensusSite, before map[string]float64, wants map[string]float64) {
	t.Helper()
	for bucket, count := range self.counts(site) {
		if delta := count - before[bucket]; delta != wants[bucket] {
			t.Errorf("%s: %s counted %v in %s, want %v", what, site, delta, bucket, wants[bucket])
		}
	}
}

// The label values are a fixed, small set of unique snake_case names, every
// one of which a census makes as it starts: 27 sites and 7 horizons.
func TestTimerCensusLabelsAreAFixedSet(t *testing.T) {
	wantSites := []string{
		"exchange_read_deadline", "exchange_write_deadline",
		"front_read_deadline", "front_write_deadline", "front_write_bound",
		"front_response_deadline", "front_upstream_deadline",
		"h1_read_deadline", "h1_write_deadline", "h1_ping_timer", "h3_ping_timer",
		"forward_write_timer",
		"redis_read_deadline", "redis_write_deadline", "pg_deadline",
		"client_timer", "client_route_write_timer", "client_receive_handoff_timer",
		"client_receive_delivery_timer", "client_ack_compress_timer", "client_pack_wait_timer",
		"client_window_pacing_timer", "client_sequence_deadline_timer",
		"quic_stream_read_timer", "quic_stream_write_timer", "quic_conn_timer", "http3_idle_timer",
	}
	if got := timerCensusSiteNames[:]; !slices.Equal(got, wantSites) {
		t.Fatalf("sites %v, want %v", got, wantSites)
	}
	wantHorizons := []string{"lt_1ms", "1ms_10ms", "10ms_100ms", "100ms_1s", "1s_10s", "ge_10s", "stop"}
	if got := timerCensusHorizonNames[:]; !slices.Equal(got, wantHorizons) {
		t.Fatalf("horizons %v, want %v", got, wantHorizons)
	}
	label := regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
	for _, names := range [][]string{timerCensusSiteNames[:], timerCensusHorizonNames[:]} {
		seen := map[string]bool{}
		for _, name := range names {
			if !label.MatchString(name) || seen[name] {
				t.Errorf("label value %q is not a unique snake_case name", name)
			}
			seen[name] = true
		}
	}
	for site := range timerCensusSiteCount {
		if site.String() != timerCensusSiteNames[site] {
			t.Errorf("site %d is named %q, want %q", site, site.String(), timerCensusSiteNames[site])
		}
	}

	census := startTestTimerCensus(t, 64)
	if series := testutil.CollectAndCount(census.counterVec); series != len(wantSites)*len(wantHorizons) {
		t.Fatalf("a started census made %d series, want %d", series, len(wantSites)*len(wantHorizons))
	}
}

// Each bucket holds a half-open range; a nonpositive horizon is due at once.
func TestTimerCensusHorizonBuckets(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{d: -time.Second, want: "lt_1ms"},
		{d: 0, want: "lt_1ms"},
		{d: time.Millisecond - 1, want: "lt_1ms"},
		{d: time.Millisecond, want: "1ms_10ms"},
		{d: 10*time.Millisecond - 1, want: "1ms_10ms"},
		{d: 10 * time.Millisecond, want: "10ms_100ms"},
		{d: 100*time.Millisecond - 1, want: "10ms_100ms"},
		{d: 100 * time.Millisecond, want: "100ms_1s"},
		{d: time.Second - 1, want: "100ms_1s"},
		{d: time.Second, want: "1s_10s"},
		{d: 10*time.Second - 1, want: "1s_10s"},
		{d: 10 * time.Second, want: "ge_10s"},
		{d: 1000 * time.Hour, want: "ge_10s"},
	} {
		if got := timerCensusHorizonNames[timerCensusHorizonOf(c.d)]; got != c.want {
			t.Errorf("%s is in %s, want %s", c.d, got, c.want)
		}
	}
}

// With every call sampled, each counted call adds one in its bucket and
// nothing elsewhere. A deadline cleared, or at or before now, is a stop, and a
// library's sampled record adds its count.
func TestTimerCensusCountsEachCallOnceInItsBucket(t *testing.T) {
	census := startTestTimerCensus(t, 1)
	// nothing in this package counts at the h3 heartbeat but this test
	site := TimerCensusSiteH3PingTimer
	for _, c := range []struct {
		what    string
		count   func()
		horizon string
		want    float64
	}{
		{what: "a Reset 5 ms out", count: func() { CountTimerReset(site, 5*time.Millisecond) }, horizon: "1ms_10ms", want: 1},
		{what: "a Reset due at once", count: func() { CountTimerReset(site, 0) }, horizon: "lt_1ms", want: 1},
		{what: "a deadline 30 s out", count: func() { CountTimerDeadlineAfter(site, 30*time.Second) }, horizon: "ge_10s", want: 1},
		{what: "a deadline 0 out", count: func() { CountTimerDeadlineAfter(site, 0) }, horizon: "stop", want: 1},
		{what: "a deadline at 2 s", count: func() { CountTimerDeadline(site, time.Now().Add(2*time.Second)) }, horizon: "1s_10s", want: 1},
		{what: "a cleared deadline", count: func() { CountTimerDeadline(site, time.Time{}) }, horizon: "stop", want: 1},
		{what: "a deadline passed", count: func() { CountTimerDeadline(site, time.Now().Add(-time.Second)) }, horizon: "stop", want: 1},
		{what: "a stop", count: func() { CountTimerStop(site) }, horizon: "stop", want: 1},
		{what: "a library's sampled Reset", count: func() { RecordTimerChange(site, 50*time.Millisecond, false, 64) }, horizon: "10ms_100ms", want: 64},
		{what: "a library's sampled stop", count: func() { RecordTimerChange(site, time.Hour, true, 64) }, horizon: "stop", want: 64},
	} {
		before := census.counts(site)
		c.count()
		census.requireCounted(t, c.what, site, before, c.horizon, c.want)
	}
}

// One call in the period is counted, as the period: the count is a multiple
// of it, and over many calls near the calls made.
func TestTimerCensusSamplesOneCallInThePeriod(t *testing.T) {
	census := startTestTimerCensus(t, 64)
	site := TimerCensusSiteH3PingTimer
	const callCount = 64 * 4096
	for range callCount {
		CountTimerReset(site, time.Hour)
	}
	counted := census.count(site, "ge_10s")
	if int64(counted)%64 != 0 {
		t.Fatalf("counted %v, not a multiple of the period", counted)
	}
	// the sampled calls are binomial, with a standard deviation of 1.6% of
	// the calls here: 10% is more than six of them
	if counted < 0.9*callCount || 1.1*callCount < counted {
		t.Fatalf("counted %v for %d calls", counted, callCount)
	}
}

// With no census running, nothing is counted; a non-positive period starts
// none, and a period that is not a power of two is refused.
func TestTimerCensusOffCountsNothing(t *testing.T) {
	census := startTestTimerCensus(t, 1)
	site := TimerCensusSiteH3PingTimer
	activeTimerCensus.Store(nil)
	before := census.counts(site)
	CountTimerReset(site, time.Second)
	CountTimerDeadline(site, time.Now().Add(time.Second))
	CountTimerDeadlineAfter(site, time.Second)
	CountTimerStop(site)
	RecordTimerChange(site, time.Second, false, 1)
	census.requireCounted(t, "calls with no census", site, before, "1s_10s", 0)
	if count := Testing_TimerCensusCount(site, "1s_10s"); count != 0 {
		t.Fatalf("with no census the count is %v", count)
	}

	stop, err := StartTimerCensus(&TimerCensusSettings{SamplePeriod: 0})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if activeTimerCensus.Load() != nil {
		t.Fatal("a zero sample period started a census")
	}
	if _, err := StartTimerCensus(&TimerCensusSettings{SamplePeriod: 48}); err == nil {
		t.Fatal("a sample period of 48 was taken")
	}
}

// A census's stop leaves a census started after it running.
func TestTimerCensusStopLeavesALaterCensus(t *testing.T) {
	first := startTestTimerCensus(t, 1)
	stop := installTimerCensus(activeTimerCensus.Load())
	second := startTestTimerCensus(t, 1)
	stop()
	site := TimerCensusSiteH3PingTimer
	before := second.counts(site)
	CountTimerStop(site)
	second.requireCounted(t, "a stop after an earlier census's stop", site, before, "stop", 1)
	if count := first.count(site, "stop"); count != 0 {
		t.Fatalf("the stopped census counted %v", count)
	}
}

// A Redis socket counts each deadline go-redis sets at its site and keeps
// syscall.Conn, through which go-redis checks a pooled socket, and NetConn. A
// socket without syscall.Conn gets none.
func TestRedisTimerCensusConnCountsEachDeadline(t *testing.T) {
	census := startTestTimerCensus(t, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			var b [1]byte
			conn.Read(b[:])
		}
	}()
	dial := redisTimerCensusDialer(func(ctx context.Context, network string, addr string) (net.Conn, error) {
		return net.Dial(network, addr)
	})
	conn, err := dial(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		t.Fatalf("the counted socket %T hides syscall.Conn", conn)
	}
	if _, err := syscallConn.SyscallConn(); err != nil {
		t.Fatal(err)
	}
	if netConn, ok := conn.(interface{ NetConn() net.Conn }); !ok {
		t.Fatal("the counted socket hides NetConn")
	} else if _, ok := netConn.NetConn().(*net.TCPConn); !ok {
		t.Fatalf("NetConn is %T, not the socket", netConn.NetConn())
	}

	before := census.counts(TimerCensusSiteRedisReadDeadline)
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	census.requireCounted(t, "a read deadline 50 ms out", TimerCensusSiteRedisReadDeadline, before, "10ms_100ms", 1)
	before = census.counts(TimerCensusSiteRedisWriteDeadline)
	if err := conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	census.requireCounted(t, "a write deadline 500 ms out", TimerCensusSiteRedisWriteDeadline, before, "100ms_1s", 1)
	beforeRead, beforeWrite := census.counts(TimerCensusSiteRedisReadDeadline), census.counts(TimerCensusSiteRedisWriteDeadline)
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	census.requireCounted(t, "a socket check's clear", TimerCensusSiteRedisReadDeadline, beforeRead, "stop", 1)
	census.requireCounted(t, "a socket check's clear", TimerCensusSiteRedisWriteDeadline, beforeWrite, "stop", 1)

	pipe, peer := net.Pipe()
	defer pipe.Close()
	defer peer.Close()
	piped := newRedisTimerCensusConn(pipe)
	if _, ok := piped.(syscall.Conn); ok {
		t.Fatal("a socket without syscall.Conn was given one")
	}
	before = census.counts(TimerCensusSiteRedisReadDeadline)
	piped.SetReadDeadline(time.Now().Add(20 * time.Second))
	census.requireCounted(t, "a pipe's read deadline", TimerCensusSiteRedisReadDeadline, before, "ge_10s", 1)

	want := errors.New("synthetic dial failure")
	failed := redisTimerCensusDialer(func(context.Context, string, string) (net.Conn, error) {
		return nil, want
	})
	if conn, err := failed(context.Background(), "tcp", "192.0.2.1:6379"); conn != nil || !errors.Is(err, want) {
		t.Fatalf("a failed dial returned %v, %v", conn, err)
	}
}

// pgx's connection counts each deadline set on it at pg_deadline: pgx's
// immediate one when a query's context ends, its clear, and TLS's.
func TestPgWriteTrackedConnCountsEachDeadline(t *testing.T) {
	census := startTestTimerCensus(t, 1)
	pipe, peer := net.Pipe()
	defer pipe.Close()
	defer peer.Close()
	conn := &pgWriteTrackedConn{Conn: pipe}
	for _, c := range []struct {
		what    string
		set     func() error
		horizon string
	}{
		{what: "a context's end", set: func() error { return conn.SetDeadline(time.Now()) }, horizon: "stop"},
		{what: "its clear", set: func() error { return conn.SetDeadline(time.Time{}) }, horizon: "stop"},
		{what: "a close's 15 s", set: func() error { return conn.SetDeadline(time.Now().Add(15 * time.Second)) }, horizon: "ge_10s"},
		{what: "a read deadline", set: func() error { return conn.SetReadDeadline(time.Now().Add(3 * time.Second)) }, horizon: "1s_10s"},
		{what: "a write deadline", set: func() error { return conn.SetWriteDeadline(time.Now().Add(30 * time.Millisecond)) }, horizon: "10ms_100ms"},
	} {
		before := census.counts(TimerCensusSitePgDeadline)
		if err := c.set(); err != nil {
			t.Fatal(err)
		}
		census.requireCounted(t, c.what, TimerCensusSitePgDeadline, before, c.horizon, 1)
	}
}

// The cost of a counted call: off, as in every process but connect; on and
// not sampled, the common case at a period of 64; and sampled, which reads
// the clock and adds to a counter.
func BenchmarkTimerCensusCount(b *testing.B) {
	deadline := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name         string
		samplePeriod int
	}{
		{name: "off", samplePeriod: 0},
		{name: "period64", samplePeriod: 64},
		{name: "period1", samplePeriod: 1},
	} {
		b.Run(c.name, func(b *testing.B) {
			if 0 < c.samplePeriod {
				census, err := newTimerCensus(&TimerCensusSettings{SamplePeriod: c.samplePeriod}, prometheus.NewCounterVec(
					prometheus.CounterOpts{
						Name: "timer_census_benchmark_total",
					},
					[]string{"site", "horizon"},
				))
				if err != nil {
					b.Fatal(err)
				}
				defer installTimerCensus(census)()
			} else {
				defer installTimerCensus(nil)()
			}
			b.Run("reset", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						CountTimerReset(TimerCensusSiteH3PingTimer, time.Second)
					}
				})
			})
			b.Run("deadline", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						CountTimerDeadline(TimerCensusSiteH3PingTimer, deadline)
					}
				})
			})
		})
	}
}

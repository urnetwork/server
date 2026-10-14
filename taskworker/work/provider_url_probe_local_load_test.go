// A load run of the in-process URL pass at production geometry. Opt in with
// URNETWORK_URL_PROBE_LOAD=1; it seeds hundreds of providers per shard.
package work

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

func testLoadSetting(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && 0 < value {
		return value
	}
	return fallback
}

// Per-operation wall times of the operator calls a pass makes.
type testLoadCalls struct {
	lock      sync.Mutex
	durations map[string][]time.Duration
	errors    map[string]int
}

func (self *testLoadCalls) record(operation string, started time.Time, err error) {
	elapsed := time.Since(started)
	self.lock.Lock()
	defer self.lock.Unlock()
	self.durations[operation] = append(self.durations[operation], elapsed)
	if err != nil {
		self.errors[operation]++
	}
}

// Times every call the URL pass makes through the operator, unchanged.
type testTimedOperator struct {
	ingest.Operator
	calls *testLoadCalls
}

func (self *testTimedOperator) Due(ctx context.Context, limit int) ([]ingest.DueProvider, error) {
	started := time.Now()
	due, err := self.Operator.Due(ctx, limit)
	self.calls.record("due", started, err)
	return due, err
}

func (self *testTimedOperator) SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error {
	started := time.Now()
	err := self.Operator.SubmitEgressHealth(ctx, providerClientId, res)
	self.calls.record("health", started, err)
	return err
}

func (self *testTimedOperator) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	started := time.Now()
	err := self.Operator.ReportUrlProbeCompletion(ctx, completion)
	self.calls.record("completion", started, err)
	return err
}

// The main pool's acquire counters and acquired connections, read from the
// process's own exported pool metrics.
type testPoolSample struct {
	acquires map[string]float64
	waited   float64
	acquired float64
	maximum  float64
}

func testSamplePool(tb testing.TB) testPoolSample {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		tb.Fatal(err)
	}
	sample := testPoolSample{acquires: map[string]float64{}}
	label := func(labels map[string]string, name string) string { return labels[name] }
	for _, family := range families {
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			if label(labels, "pool") != "default" {
				continue
			}
			switch family.GetName() {
			case "urnetwork_pg_pool_acquires_total":
				sample.acquires[label(labels, "outcome")] = metric.Counter.GetValue()
			case "urnetwork_pg_pool_acquire_duration_seconds_total":
				sample.waited = metric.Counter.GetValue()
			case "urnetwork_pg_pool_connections":
				switch label(labels, "state") {
				case "acquired":
					sample.acquired = metric.Gauge.GetValue()
				case "maximum":
					sample.maximum = metric.Gauge.GetValue()
				}
			}
		}
	}
	return sample
}

func testQuantile(durations []time.Duration, q float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

// Several production-shaped URL passes -- 64 workers each, one in-process
// operator per shard with its own operator_concurrency slots, sharing the
// process's database pool as a taskworker's passes do -- claim, measure,
// publish and complete every seeded provider.
// The real prober runs; only the tunnel and its one URL load are replaced. A
// watchdog dumps every goroutine if a pass stops making progress. Afterwards
// every eligible provider must have exactly one completed claim and one accepted
// outcome, with no operator error and no call near its 30-second bound; a
// returned pass leaves no connection acquired.
func TestProviderUrlProbeLocalPassUnderLoad(t *testing.T) {
	if os.Getenv("URNETWORK_URL_PROBE_LOAD") != "1" {
		t.Skip("set URNETWORK_URL_PROBE_LOAD=1 to run the in-process URL pass load test")
	}
	shards := testLoadSetting("URNETWORK_URL_PROBE_LOAD_SHARDS", 4)
	workers := testLoadSetting("URNETWORK_URL_PROBE_LOAD_WORKERS", 64)
	perShard := testLoadSetting("URNETWORK_URL_PROBE_LOAD_PROVIDERS_PER_SHARD", 256)
	turnMillis := testLoadSetting("URNETWORK_URL_PROBE_LOAD_TURN_MILLIS", 50)
	stallSeconds := testLoadSetting("URNETWORK_URL_PROBE_LOAD_STALL_SECONDS", 120)
	operatorConcurrency := testLoadSetting("URNETWORK_URL_PROBE_LOAD_OPERATOR_CONCURRENCY", defaultProviderEgressOperatorConcurrency)
	// Optionally size this process's pool like a deployment's, e.g. Main's
	// taskworker at 128; the local default is 16. Read when the pool opens.
	if pool := testLoadSetting("URNETWORK_URL_PROBE_LOAD_POOL", 0); 0 < pool {
		defer server.Config.PushSimpleResource(server.DefaultPgConfigResourceName,
			[]byte(fmt.Sprintf("min_connections: %d\nmax_connections: %d\n", min(4, pool), pool)))()
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tb.Errorf("the URL pass requested the operator API: %s %s", r.Method, r.URL.Path)
			http.Error(w, "closed", http.StatusServiceUnavailable)
		}))
		defer trap.Close()
		defer server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testOperatorTransportCatalog))()

		// seed: one location, shards*perShard providers, one reliability pass
		city := &model.Location{LocationType: model.LocationTypeCity, City: "Palo Alto", Region: "California", Country: "United States", CountryCode: "us"}
		model.CreateLocation(ctx, city)
		total := shards * perShard
		seedStarted := time.Now()
		seeded := make([]server.Id, total)
		var seedGroup sync.WaitGroup
		var seedNext atomic.Int64
		for range 8 {
			seedGroup.Add(1)
			go func() {
				defer seedGroup.Done()
				for {
					index := int(seedNext.Add(1)) - 1
					if total <= index {
						return
					}
					clientId := server.NewId()
					model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
					// one provider per address, as the reliability rollup expects
					// of real providers (198.18.0.0/15 is the benchmarking range)
					address := fmt.Sprintf("198.%d.%d.%d:0", 18+index/(250*250), (index/250)%250, 1+index%250)
					connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, address, model.CreateNetworkClientHandler(ctx))
					if err != nil {
						tb.Errorf("connect client: %s", err)
						return
					}
					if err := model.SetConnectionLocation(ctx, connectionId, city.LocationId, &model.ConnectionLocationScores{}); err != nil {
						tb.Errorf("set connection location: %s", err)
						return
					}
					model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: []byte("provide-secret")})
					seeded[index] = clientId
				}
			}()
		}
		seedGroup.Wait()
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
		proberNetworkId := server.NewId()
		server.Raise(model.AddBasicTransferBalance(ctx, proberNetworkId, model.ByteCount(1024*1024*1024*1024), server.NowUtc().Add(-time.Minute), server.NowUtc().Add(time.Hour)))
		tb.Logf("seeded %d providers in %s", total, time.Since(seedStarted).Round(time.Millisecond))
		var serverMaxConnections string
		var serverConnections int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SHOW max_connections`).Scan(&serverMaxConnections))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pg_stat_activity`).Scan(&serverConnections))
		})
		tb.Logf("postgres: max_connections=%s connections_in_use=%d", serverMaxConnections, serverConnections)
		// only what the reliability rollup admits is URL work
		var cycles, eligible, dueNow int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE eligible), COUNT(*) FILTER (WHERE eligible AND next_attempt_at <= $1)
				FROM provider_egress_probe_cycle`, server.NowUtc()).Scan(&cycles, &eligible, &dueNow))
		})
		// A provider connected only moments before the rollup has not built the
		// independent reliability weight URL work requires, so a long seed admits
		// fewer than it seeded. Every admitted provider must be measured once.
		tb.Logf("seeded cycles=%d eligible=%d due_now=%d", cycles, eligible, dueNow)
		if dueNow < shards*workers {
			tb.Fatalf("only %d eligible providers; the run needs at least %d to fill every worker", dueNow, shards*workers)
		}

		calls := &testLoadCalls{durations: map[string][]time.Duration{}, errors: map[string]int{}}
		var turns atomic.Int64
		var running atomic.Int64
		var peakRunning atomic.Int64
		settings := defaultProviderEgressProbeSettings("example.test")
		settings.OperatorTransport = providerEgressOperatorLocal
		settings.OperatorConcurrency = operatorConcurrency
		passes := make([]*providerEgressProbePass, shards)
		passArgs := make([]*ProviderEgressProbeArgs, shards)
		for shard := range shards {
			args := providerEgressProbeArgs(settings, shard)
			args.ShardCount = shards
			args.APIURL, args.PublicAPIURL = trap.URL, trap.URL
			args.PlatformURL = "wss://" + strings.TrimPrefix(trap.URL, "http://")
			args.UrlProbe.Limit, args.UrlProbe.Concurrency = workers, workers
			operator, err := newProviderEgressOperator(args, settings)
			if err != nil {
				tb.Fatal(err)
			}
			pass := newProviderUrlProbePass(args, &testTimedOperator{Operator: operator, calls: calls},
				providertunnel.Config{ApiUrl: args.APIURL, PlatformUrl: args.PlatformURL}, proberNetworkId)
			pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
				p := &prober.Prober{
					Open: func(context.Context, string) (*http.Client, func() error, error) {
						return &http.Client{}, func() error { return nil }, nil
					},
					Health: func(ctx context.Context, _ *http.Client, _ egresshealth.Place) (*egresshealth.Result, error) {
						now := running.Add(1)
						for {
							peak := peakRunning.Load()
							if now <= peak || peakRunning.CompareAndSwap(peak, now) {
								break
							}
						}
						defer running.Add(-1)
						// the URL load's own time, spread so publications interleave
						select {
						case <-time.After(time.Duration(rand.IntN(turnMillis+1)) * time.Millisecond):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						return testOperatorTransportUrlSuccess(time.Now().UTC()), nil
					},
					Submit: options.Submit, Attempts: options.Attempts, HealthResults: options.HealthResults,
				}
				summary := (&prober.Scheduler{Prober: p, Concurrency: 1}).Run(ctx, providers)
				turns.Add(int64(summary.Attempted))
				return summary, nil
			}
			passes[shard], passArgs[shard] = pass, args
		}

		before := testSamplePool(tb)
		peakAcquired := 0.0
		sampleDone := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-sampleDone:
					return
				case <-ticker.C:
					peakAcquired = max(peakAcquired, testSamplePool(tb).acquired)
				}
			}
		}()

		// production passes run under the task's 900-second deadline
		passCtx, cancelPass := context.WithTimeout(ctx, time.Duration(settings.MaxTimeSeconds)*time.Second)
		defer cancelPass()
		started := time.Now()
		results := make([]*ProviderEgressProbeResult, shards)
		errs := make([]error, shards)
		durations := make([]time.Duration, shards)
		var passGroup sync.WaitGroup
		for shard := range shards {
			passGroup.Add(1)
			go func() {
				defer passGroup.Done()
				passStarted := time.Now()
				results[shard], errs[shard] = passes[shard].run(passCtx, passArgs[shard])
				durations[shard] = time.Since(passStarted)
			}()
		}
		finished := make(chan struct{})
		go func() { passGroup.Wait(); close(finished) }()
		// the watchdog: no completed turn for stallSeconds while passes run is a hang
		lastTurns, lastProgress := int64(-1), time.Now()
		stalled := false
	wait:
		for {
			select {
			case <-finished:
				break wait
			case <-time.After(time.Second):
				if current := turns.Load(); current != lastTurns {
					lastTurns, lastProgress = current, time.Now()
				} else if time.Duration(stallSeconds)*time.Second < time.Since(lastProgress) {
					stalled = true
					break wait
				}
			}
		}
		elapsed := time.Since(started)
		close(sampleDone)
		<-sampled
		if stalled {
			dump := filepath.Join(os.Getenv("GOTMPDIR"), fmt.Sprintf("url-probe-load-stall-%d.txt", time.Now().Unix()))
			if file, err := os.Create(dump); err == nil {
				_ = pprof.Lookup("goroutine").WriteTo(file, 2)
				file.Close()
			}
			cancelPass()
			<-finished
			tb.Fatalf("no URL turn completed for %ds with passes running; goroutines dumped to %s", stallSeconds, dump)
		}
		after := testSamplePool(tb)

		// A returned pass owns no database work: every connection it used is
		// back in the pool, with nothing left running in the background.
		idleDeadline := time.Now().Add(5 * time.Second)
		for 0 < testSamplePool(tb).acquired && time.Now().Before(idleDeadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if acquired := testSamplePool(tb).acquired; 0 < acquired {
			dump := filepath.Join(os.Getenv("GOTMPDIR"), fmt.Sprintf("url-probe-load-acquired-%d.txt", time.Now().Unix()))
			if file, err := os.Create(dump); err == nil {
				_ = pprof.Lookup("goroutine").WriteTo(file, 2)
				file.Close()
			}
			tb.Fatalf("%.0f connections are still acquired 5s after every pass returned; goroutines dumped to %s", acquired, dump)
		}

		// every shard drained its queue without an error
		dueTotal, attempted, submitted := 0, 0, 0
		for shard := range shards {
			if errs[shard] != nil {
				tb.Errorf("shard %d pass error: %v", shard, errs[shard])
			}
			if results[shard] != nil {
				dueTotal += results[shard].UrlDue
				attempted += results[shard].Attempted
				submitted += results[shard].Submitted
			}
		}

		// every provider has exactly one completed claim and one accepted outcome
		ids := make([]string, 0, len(seeded))
		for _, clientId := range seeded {
			ids = append(ids, clientId.String())
		}
		var claimed, completed, accepted, unfinished int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(DISTINCT client_id) FROM provider_url_probe_run WHERE client_id::text = ANY($1)`, ids).Scan(&claimed))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE client_id::text = ANY($1) AND completed_at IS NOT NULL AND probe_failure=''`, ids).Scan(&completed))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE client_id::text = ANY($1) AND completed_at IS NULL`, ids).Scan(&unfinished))
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_health_history WHERE client_id::text = ANY($1) AND total_count=1 AND ok_count=1`, ids).Scan(&accepted))
		})

		operations := []string{}
		for operation := range calls.durations {
			operations = append(operations, operation)
		}
		slices.Sort(operations)
		slowest := time.Duration(0)
		for _, operation := range operations {
			durations := calls.durations[operation]
			slowest = max(slowest, testQuantile(durations, 1))
			tb.Logf("%-10s calls=%d errors=%d p50=%s p99=%s max=%s", operation, len(durations), calls.errors[operation],
				testQuantile(durations, 0.5).Round(time.Microsecond), testQuantile(durations, 0.99).Round(time.Microsecond),
				testQuantile(durations, 1).Round(time.Microsecond))
			if calls.errors[operation] != 0 {
				tb.Errorf("%s returned %d errors", operation, calls.errors[operation])
			}
		}
		acquireDelta := map[string]float64{}
		for outcome, value := range after.acquires {
			acquireDelta[outcome] = value - before.acquires[outcome]
		}
		tb.Logf("shards=%d workers/shard=%d operator_concurrency/shard=%d providers=%d turn<=%dms elapsed=%s turns/s=%.0f peak_concurrent_turns=%d pass_durations=%v",
			shards, workers, operatorConcurrency, total, turnMillis, elapsed.Round(time.Millisecond), float64(turns.Load())/elapsed.Seconds(), peakRunning.Load(), durations)
		tb.Logf("main pool: max_connections=%.0f peak_acquired=%.0f acquires=%v acquire_wait=%.3fs", after.maximum, peakAcquired, acquireDelta, after.waited-before.waited)
		tb.Logf("durable: due=%d attempted=%d accepted=%d claimed=%d completed=%d unfinished=%d health=%d", dueTotal, attempted, submitted, claimed, completed, unfinished, accepted)

		if dueTotal != dueNow || attempted != dueNow || submitted != dueNow || claimed != dueNow || completed != dueNow || unfinished != 0 || accepted != dueNow {
			tb.Fatalf("the passes did not claim, measure and complete every eligible provider exactly once")
		}
		if 10*time.Second <= slowest {
			tb.Fatalf("an operator call took %s, near its 30-second bound", slowest)
		}
	})
}

//go:build sidecar_pending

package model

// §22.8's escrow benchmark (§20.9's gate, owner decision 92, D-PHASE-7; QUOTA-M4-03): contract admission and
// settlement on a payer holding a data pack and a team_credit line, with escrow_draw_order off and on, so M6's
// entry has evidence that the draw order costs admission nothing. One measurement is one run of one of the
// two paths in one gate state:
//
//	admission   CreateTransferEscrow, the path every transfer pair takes: p50 and p99 latency of the
//	            admissions that passed, the failed ones counted beside them, and the Postgres round trips of
//	            one admission, read two ways: the grant reads through redisGrantReadContextKey's seam, and the
//	            main pool's acquires over the run.
//	settlement  both sides' CloseContract on contracts admitted before the clock starts: settlements a
//	            second, and the same latencies.
//
// BenchmarkEscrowAdmission and BenchmarkEscrowSettlement are the `go test -bench` entries for a host whose
// environment is the benchmark's own; they skip unless ESCROWBENCH=1, since they write clients, balances and
// contracts into whatever database the process reaches. TestEscrowBenchmarkMeasurements runs the same
// measurements in a database of its own, as every server test has, and with ESCROWBENCH_OUT set writes them as
// the JSON server/cli/escrowbench compares against main's. The rates are inputs: ESCROWBENCH_ADMISSIONS_PER_S
// and ESCROWBENCH_SETTLEMENTS_PER_S pace a run, which an operator sets to twice the Y1 rates; unset, a run goes
// as fast as the paths answer, which is what the local smoke run does.

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

type escrowBenchSettings struct {
	// Operations a run measures.
	Count int
	// The pace of a run in operations a second. Zero runs one after another as fast as they answer.
	AdmissionsPerSecond  float64
	SettlementsPerSecond float64
	// What each contract asks for, and what each of the payer's two balances holds.
	ContractByteCount ByteCount
	BalanceByteCount  ByteCount
}

func defaultEscrowBenchSettings() *escrowBenchSettings {
	return &escrowBenchSettings{
		Count:             40,
		ContractByteCount: 1 * Mib,
		BalanceByteCount:  1024 * Gib,
	}
}

// The defaults with what the environment names: ESCROWBENCH_COUNT, ESCROWBENCH_ADMISSIONS_PER_S and
// ESCROWBENCH_SETTLEMENTS_PER_S.
func escrowBenchSettingsFromEnv(tb testing.TB) *escrowBenchSettings {
	settings := defaultEscrowBenchSettings()
	if value := os.Getenv("ESCROWBENCH_COUNT"); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil || count <= 0 {
			tb.Fatalf("ESCROWBENCH_COUNT %q is not a positive count", value)
		}
		settings.Count = count
	}
	for name, rate := range map[string]*float64{
		"ESCROWBENCH_ADMISSIONS_PER_S":  &settings.AdmissionsPerSecond,
		"ESCROWBENCH_SETTLEMENTS_PER_S": &settings.SettlementsPerSecond,
	} {
		if value := os.Getenv(name); value != "" {
			perSecond, err := strconv.ParseFloat(value, 64)
			if err != nil || perSecond <= 0 {
				tb.Fatalf("%s %q is not a positive rate", name, value)
			}
			*rate = perSecond
		}
	}
	return settings
}

// One measurement. The JSON is what server/cli/escrowbench reads.
type escrowBenchResult struct {
	// "admission" or "settlement"
	Name string `json:"name"`
	// escrow_draw_order's state for the run
	DrawOrder bool `json:"draw_order"`
	// operations that passed, and that failed
	Count       int `json:"count"`
	FailedCount int `json:"failed_count"`
	// latency of the operations that passed
	P50Ns int64 `json:"p50_ns"`
	P99Ns int64 `json:"p99_ns"`
	// operations that passed a second, over the run
	PerSecond float64 `json:"per_second"`
	// the pace asked for, 0 for none
	TargetPerSecond float64 `json:"target_per_second"`
	// Postgres round trips of one operation: the main pool's acquires, and for an admission the grant
	// reads through the seam
	PgAcquiresPerOp float64 `json:"pg_acquires_per_op"`
	GrantReadsPerOp float64 `json:"grant_reads_per_op"`
}

// What escrowbench reads: one build's measurements.
type escrowBenchMeasurements struct {
	Label   string               `json:"label"`
	Time    time.Time            `json:"time"`
	Results []*escrowBenchResult `json:"results"`
}

// Counts the queries admission's grant discovery makes through redisGrantReadContextKey's seam.
type escrowBenchReads struct {
	queryCount atomic.Int64
}

func (self *escrowBenchReads) observe(query server.PgCanQuery) server.PgCanQuery {
	return &escrowBenchQuery{PgCanQuery: query, reads: self}
}

type escrowBenchQuery struct {
	server.PgCanQuery
	reads *escrowBenchReads
}

func (self *escrowBenchQuery) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	self.reads.queryCount.Add(1)
	return self.PgCanQuery.Query(ctx, sql, args...)
}

// Holds a run to perSecond operations a second: waits until operation index is due. Zero does not wait.
func escrowBenchPace(start time.Time, index int, perSecond float64) {
	if perSecond <= 0 {
		return
	}
	due := start.Add(time.Duration(float64(index) / perSecond * float64(time.Second)))
	if wait := time.Until(due); 0 < wait {
		time.Sleep(wait)
	}
}

func escrowBenchPercentile(latencies []time.Duration, fraction float64) int64 {
	if len(latencies) == 0 {
		return 0
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	index := min(len(sorted)-1, int(float64(len(sorted))*fraction))
	return int64(sorted[index])
}

func escrowBenchPoolAcquires(tb testing.TB) int64 {
	stat, ok := server.DbPoolAcquireStats()
	if !ok {
		tb.Fatal("the main pool is not open")
	}
	return stat.AcquireCount
}

// Admits settings.Count contracts for a fresh payer with the gate at drawOrder. addedRoundTrip makes each
// admission one more pooled read, which the smoke test uses to show the harness counts it. Answers the
// measurement and the contracts admitted.
func escrowBenchAdmit(tb testing.TB, ctx context.Context, drawOrder bool, settings *escrowBenchSettings, addedRoundTrip bool) (*escrowBenchResult, *testingEscrowDrawPayer, []server.Id) {
	tb.Helper()
	defer Testing_SetEscrowDrawOrder(drawOrder)()
	payer := newTestingEscrowDrawPayer(tb, ctx, settings.BalanceByteCount)
	reads := &escrowBenchReads{}
	counted := context.WithValue(ctx, redisGrantReadContextKey{}, reads.observe)
	admit := func() (*TransferEscrow, error) {
		if addedRoundTrip {
			server.Db(ctx, func(conn server.PgConn) {
				var one int
				server.Raise(conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
			})
		}
		return CreateTransferEscrow(counted, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, settings.ContractByteCount)
	}
	// the first admission of a payer loads what the rest find warm
	contractIds := []server.Id{}
	if escrow, err := admit(); err != nil {
		tb.Fatalf("the warming admission: %v", err)
	} else {
		contractIds = append(contractIds, escrow.ContractId)
	}
	result := &escrowBenchResult{Name: "admission", DrawOrder: drawOrder, TargetPerSecond: settings.AdmissionsPerSecond}
	latencies := make([]time.Duration, 0, settings.Count)
	readsBefore := reads.queryCount.Load()
	acquiresBefore := escrowBenchPoolAcquires(tb)
	start := time.Now()
	for index := range settings.Count {
		escrowBenchPace(start, index, settings.AdmissionsPerSecond)
		opStart := time.Now()
		escrow, err := admit()
		if err != nil {
			result.FailedCount += 1
			continue
		}
		latencies = append(latencies, time.Since(opStart))
		contractIds = append(contractIds, escrow.ContractId)
	}
	elapsed := time.Since(start)
	result.Count = len(latencies)
	result.P50Ns = escrowBenchPercentile(latencies, 0.50)
	result.P99Ns = escrowBenchPercentile(latencies, 0.99)
	result.PerSecond = float64(result.Count) / elapsed.Seconds()
	result.PgAcquiresPerOp = float64(escrowBenchPoolAcquires(tb)-acquiresBefore) / float64(settings.Count)
	result.GrantReadsPerOp = float64(reads.queryCount.Load()-readsBefore) / float64(settings.Count)
	return result, payer, contractIds
}

// Settles settings.Count contracts admitted before the clock starts, each closed by both sides with half
// its bytes used, the gate at drawOrder.
func escrowBenchSettle(tb testing.TB, ctx context.Context, drawOrder bool, settings *escrowBenchSettings) *escrowBenchResult {
	tb.Helper()
	admitSettings := *settings
	admitSettings.AdmissionsPerSecond = 0
	_, payer, contractIds := escrowBenchAdmit(tb, ctx, drawOrder, &admitSettings, false)
	defer Testing_SetEscrowDrawOrder(drawOrder)()
	result := &escrowBenchResult{Name: "settlement", DrawOrder: drawOrder, TargetPerSecond: settings.SettlementsPerSecond}
	used := settings.ContractByteCount / 2
	latencies := make([]time.Duration, 0, len(contractIds))
	acquiresBefore := escrowBenchPoolAcquires(tb)
	start := time.Now()
	for index, contractId := range contractIds {
		escrowBenchPace(start, index, settings.SettlementsPerSecond)
		opStart := time.Now()
		err := CloseContract(ctx, contractId, payer.payerId, used, false)
		if err == nil {
			err = CloseContract(ctx, contractId, payer.providerId, used, false)
		}
		if err != nil {
			result.FailedCount += 1
			continue
		}
		latencies = append(latencies, time.Since(opStart))
	}
	elapsed := time.Since(start)
	result.Count = len(latencies)
	result.P50Ns = escrowBenchPercentile(latencies, 0.50)
	result.P99Ns = escrowBenchPercentile(latencies, 0.99)
	result.PerSecond = float64(result.Count) / elapsed.Seconds()
	result.PgAcquiresPerOp = float64(escrowBenchPoolAcquires(tb)-acquiresBefore) / float64(len(contractIds))
	return result
}

func escrowBenchReport(b *testing.B, result *escrowBenchResult) {
	b.ReportMetric(float64(result.P50Ns), "p50-ns/op")
	b.ReportMetric(float64(result.P99Ns), "p99-ns/op")
	b.ReportMetric(result.PerSecond, "ops/s")
	b.ReportMetric(float64(result.FailedCount), "failed")
	b.ReportMetric(result.PgAcquiresPerOp, "pg-acquires/op")
	if result.Name == "admission" {
		b.ReportMetric(result.GrantReadsPerOp, "grant-reads/op")
	}
}

func escrowBenchGateName(drawOrder bool) string {
	if drawOrder {
		return "draw_order=on"
	}
	return "draw_order=off"
}

// Skips unless the caller said this process's database is the benchmark's to write.
func escrowBenchRequireOwnDatabase(b *testing.B) {
	if os.Getenv("ESCROWBENCH") != "1" {
		b.Skip("set ESCROWBENCH=1 on a host whose database is this benchmark's to write; TestEscrowBenchmarkMeasurements runs it in a database of its own")
	}
}

func BenchmarkEscrowAdmission(b *testing.B) {
	escrowBenchRequireOwnDatabase(b)
	ctx := context.Background()
	for _, drawOrder := range []bool{false, true} {
		b.Run(escrowBenchGateName(drawOrder), func(b *testing.B) {
			settings := escrowBenchSettingsFromEnv(b)
			settings.Count = b.N
			b.ResetTimer()
			result, _, _ := escrowBenchAdmit(b, ctx, drawOrder, settings, false)
			escrowBenchReport(b, result)
		})
	}
}

func BenchmarkEscrowSettlement(b *testing.B) {
	escrowBenchRequireOwnDatabase(b)
	ctx := context.Background()
	for _, drawOrder := range []bool{false, true} {
		b.Run(escrowBenchGateName(drawOrder), func(b *testing.B) {
			settings := escrowBenchSettingsFromEnv(b)
			settings.Count = b.N
			b.ResetTimer()
			result := escrowBenchSettle(b, ctx, drawOrder, settings)
			escrowBenchReport(b, result)
		})
	}
}

// Both paths in both gate states, in this test's own database: every admission and settlement passes, the
// gate adds no round trip to an admission, and the measurements are written as escrowbench reads them, to
// ESCROWBENCH_OUT when it is set.
func TestEscrowBenchmarkMeasurements(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := context.Background()
		settings := escrowBenchSettingsFromEnv(tb)
		measurements := &escrowBenchMeasurements{Label: os.Getenv("ESCROWBENCH_LABEL"), Time: server.NowUtc()}
		admissions := map[bool]*escrowBenchResult{}
		for _, drawOrder := range []bool{false, true} {
			admission, _, _ := escrowBenchAdmit(tb, ctx, drawOrder, settings, false)
			settlement := escrowBenchSettle(tb, ctx, drawOrder, settings)
			for _, result := range []*escrowBenchResult{admission, settlement} {
				if result.FailedCount != 0 || result.Count == 0 || result.P99Ns <= 0 || result.PerSecond <= 0 {
					tb.Fatalf("%s, %s: %+v", result.Name, escrowBenchGateName(drawOrder), result)
				}
				measurements.Results = append(measurements.Results, result)
			}
			admissions[drawOrder] = admission
		}
		// the pool's acquires take in whatever else the process read meanwhile, so the seam's count is the
		// exact one
		if off, on := admissions[false].GrantReadsPerOp, admissions[true].GrantReadsPerOp; off < on {
			tb.Fatalf("the gate added grant reads to an admission: %v off, %v on", off, on)
		}
		content, err := json.MarshalIndent(measurements, "", "  ")
		connect.AssertEqual(tb, err, nil)
		path := os.Getenv("ESCROWBENCH_OUT")
		if path == "" {
			path = t.TempDir() + "/escrowbench.json"
		}
		connect.AssertEqual(tb, os.WriteFile(path, append(content, '\n'), 0o644), nil)
		read := &escrowBenchMeasurements{}
		content, err = os.ReadFile(path)
		connect.AssertEqual(tb, err, nil)
		connect.AssertEqual(tb, json.Unmarshal(content, read), nil)
		connect.AssertEqual(tb, len(read.Results), 4)
	})
}

// The harness counts a round trip added to admission on purpose: one more pooled read an admission shows
// as one more acquire an admission, which escrowbench's comparison then fails (its own test holds that half).
func TestEscrowBenchmarkCountsAnAddedRoundTrip(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := context.Background()
		settings := defaultEscrowBenchSettings()
		plain, _, _ := escrowBenchAdmit(tb, ctx, true, settings, false)
		added, _, _ := escrowBenchAdmit(tb, ctx, true, settings, true)
		if plain.FailedCount != 0 || added.FailedCount != 0 {
			tb.Fatalf("failed admissions: %+v, %+v", plain, added)
		}
		// exactly one more, but for what else the process acquired during either run
		if difference := added.PgAcquiresPerOp - plain.PgAcquiresPerOp; difference < 0.9 || 1.1 < difference {
			tb.Fatalf("an added round trip moved acquires an admission by %v: %v to %v", difference, plain.PgAcquiresPerOp, added.PgAcquiresPerOp)
		}
		connect.AssertEqual(tb, added.GrantReadsPerOp, plain.GrantReadsPerOp)
	})
}

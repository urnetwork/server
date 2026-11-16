package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

type providerCapabilityStatsQueryTest func(context.Context, string, ...any) (pgx.Rows, error)

func (self providerCapabilityStatsQueryTest) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return self(ctx, sql, args...)
}

type providerCapabilityStatsRowTest struct {
	kind   string
	label  string
	window string
	count  int64
}

type providerCapabilityStatsRowsTest struct {
	pgx.Rows
	rows      []providerCapabilityStatsRowTest
	index     int
	closed    bool
	failure   error
	afterScan func()
}

func (self *providerCapabilityStatsRowsTest) Next() bool {
	self.index++
	return self.index <= len(self.rows)
}
func (self *providerCapabilityStatsRowsTest) Err() error { return self.failure }
func (self *providerCapabilityStatsRowsTest) Close()     { self.closed = true }
func (self *providerCapabilityStatsRowsTest) Scan(dest ...any) error {
	r := self.rows[self.index-1]
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*string), *dest[3].(*int64) = r.kind, r.label, r.window, r.count
	*dest[4].(*time.Time) = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	if self.afterScan != nil {
		self.afterScan()
	}
	return nil
}

func TestProviderCapabilityStatsRefusesPartialAndInvalidSnapshots(t *testing.T) {
	for _, mode := range []string{"row failure", "cancel after decode", "empty", "unknown kind", "unknown pair", "unknown age", "blank key exchange", "negative"} {
		ctx, cancel := context.WithCancel(t.Context())
		rows := &providerCapabilityStatsRowsTest{rows: []providerCapabilityStatsRowTest{{"pair", "post_quantum", "resizable", 3}}}
		switch mode {
		case "row failure":
			rows.failure = errors.New("synthetic capability read failure")
		case "cancel after decode":
			rows.afterScan = cancel
		case "empty":
			rows.rows = nil
		case "unknown kind":
			rows.rows[0].kind = "unrecognized"
		case "unknown pair":
			rows.rows[0].window = "unknown"
		case "unknown age":
			rows.rows[0] = providerCapabilityStatsRowTest{"age", "2d", "", 1}
		case "blank key exchange":
			rows.rows[0] = providerCapabilityStatsRowTest{"key_exchange", "", "", 1}
		case "negative":
			rows.rows[0].count = -1
		}
		query := providerCapabilityStatsQueryTest(func(context.Context, string, ...any) (pgx.Rows, error) { return rows, nil })
		stats, err := readProviderCapabilityStats(ctx, query)
		cancel()
		if err == nil || stats != nil || !rows.closed {
			t.Fatalf("%s published a census: stats=%v err=%v closed=%t", mode, stats, err, rows.closed)
		}
	}
}

// Every pair and age is published, zero included, and the key exchange
// groups fold past the cap into other.
func TestProviderCapabilityStatsZeroFillsAndFoldsKeyExchanges(t *testing.T) {
	rows := &providerCapabilityStatsRowsTest{rows: []providerCapabilityStatsRowTest{
		{"age", "1d", "", 4},
		{"age", "none", "", 0},
		{"pair", "post_quantum", "resizable", 4},
	}}
	for i := range 10 {
		rows.rows = append(rows.rows, providerCapabilityStatsRowTest{"key_exchange", fmt.Sprintf("group-%02d", i), "", int64(20 - i)})
	}
	query := providerCapabilityStatsQueryTest(func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
		if sql != providerCapabilityStatsSql || len(args) != 2 || args[0] != ProvideModePublic || args[1] != ProviderModernEvidenceMaxAge.Microseconds() {
			t.Fatal("capability census lost its public provider or staleness binding")
		}
		return rows, nil
	})
	stats, err := readProviderCapabilityStats(t.Context(), query)
	if err != nil || stats.ObservedAt.IsZero() || !rows.closed {
		t.Fatalf("census err=%v", err)
	}
	if len(stats.Pairs) != len(ProviderCapabilityPairs) || stats.Pairs[[2]string{ProviderCapabilityE2ePostQuantum, ProviderCapabilityWindowResizable}] != 4 || stats.Pairs[[2]string{ProviderCapabilityE2eNoEvidence, ProviderCapabilityWindowUnknown}] != 0 {
		t.Fatalf("pairs = %v", stats.Pairs)
	}
	if len(stats.Ages) != len(ProviderCapabilityAges) || stats.Ages[ProviderCapabilityAge1d] != 4 || stats.Ages[ProviderCapabilityAgeStale] != 0 {
		t.Fatalf("ages = %v", stats.Ages)
	}
	if len(stats.KeyExchanges) != providerCapabilityKeyExchangeLimit+1 || stats.KeyExchanges["group-00"] != 20 || stats.KeyExchanges["group-07"] != 13 || stats.KeyExchanges[ProviderCapabilityKeyExchangeOther] != 12+11 {
		t.Fatalf("key exchanges = %v", stats.KeyExchanges)
	}
}

// Real PostgreSQL fixtures exercise the population filter, deduplication,
// every classification and its age, and the staleness boundary.
func TestProviderCapabilityStatsSqlClassifiesThePopulation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			_, err := tx.Exec(t.Context(), `
				CREATE TEMP TABLE network_client (client_id uuid PRIMARY KEY, active boolean, source_client_id uuid) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_location_reliability (client_id uuid, connected boolean, valid boolean) ON COMMIT DROP;
				CREATE TEMP TABLE provide_key (client_id uuid, provide_mode int) ON COMMIT DROP;
				CREATE TEMP TABLE provider_modern_evidence (client_id uuid PRIMARY KEY, post_quantum bool, key_exchange varchar(64), window_resizable bool, measured_at timestamp) ON COMMIT DROP;
			`)
			server.Raise(err)
			var now time.Time
			server.Raise(tx.QueryRow(t.Context(), `SELECT statement_timestamp() AT TIME ZONE 'UTC'`).Scan(&now))
			type fixture struct {
				name        string
				pq          bool
				keyExchange string
				resizable   bool
				age         time.Duration
				noEvidence  bool
				excluded    string
			}
			fixtures := []fixture{
				{name: "modern", pq: true, keyExchange: "X25519MLKEM768", resizable: true, age: time.Hour},
				{name: "pq fixed", pq: true, keyExchange: "X25519MLKEM768", age: 2 * 24 * time.Hour},
				{name: "classical", keyExchange: "X25519", resizable: true, age: 5 * 24 * time.Hour},
				{name: "no responder", age: time.Hour},
				{name: "stale", pq: true, keyExchange: "X25519MLKEM768", resizable: true, age: 8 * 24 * time.Hour},
				{name: "none", noEvidence: true},
				{name: "inactive", noEvidence: true, excluded: "inactive"},
				{name: "derived", noEvidence: true, excluded: "derived"},
				{name: "no key", noEvidence: true, excluded: "no key"},
				{name: "invalid", noEvidence: true, excluded: "invalid"},
			}
			for _, f := range fixtures {
				clientId := server.NewId()
				var source any
				if f.excluded == "derived" {
					source = server.NewId()
				}
				_, err = tx.Exec(t.Context(), `INSERT INTO network_client VALUES ($1,$2,$3)`, clientId, f.excluded != "inactive", source)
				server.Raise(err)
				// duplicate reliability rows and keys cannot multiply a provider
				_, err = tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability VALUES ($1,true,$2),($1,true,$2)`, clientId, f.excluded != "invalid")
				server.Raise(err)
				if f.excluded != "no key" {
					_, err = tx.Exec(t.Context(), `INSERT INTO provide_key VALUES ($1,$2),($1,$2)`, clientId, ProvideModePublic)
					server.Raise(err)
				}
				if !f.noEvidence {
					_, err = tx.Exec(t.Context(), `INSERT INTO provider_modern_evidence VALUES ($1,$2,$3,$4,$5)`, clientId, f.pq, f.keyExchange, f.resizable, now.Add(-f.age))
					server.Raise(err)
				}
			}
			stats, err := readProviderCapabilityStats(t.Context(), tx)
			if err != nil {
				t.Fatal(err)
			}
			wantPairs := map[[2]string]int64{
				{ProviderCapabilityE2ePostQuantum, ProviderCapabilityWindowResizable}: 1,
				{ProviderCapabilityE2ePostQuantum, ProviderCapabilityWindowFixed}:     1,
				{ProviderCapabilityE2eClassical, ProviderCapabilityWindowResizable}:   1,
				{ProviderCapabilityE2eNoResponder, ProviderCapabilityWindowFixed}:     1,
				{ProviderCapabilityE2eStale, ProviderCapabilityWindowUnknown}:         1,
				{ProviderCapabilityE2eNoEvidence, ProviderCapabilityWindowUnknown}:    1,
			}
			for _, pair := range ProviderCapabilityPairs {
				if stats.Pairs[pair] != wantPairs[pair] {
					t.Errorf("pair %v = %d, want %d", pair, stats.Pairs[pair], wantPairs[pair])
				}
			}
			wantAges := map[string]int64{"1d": 2, "3d": 1, "7d": 1, "stale": 1, "none": 1}
			for _, age := range ProviderCapabilityAges {
				if stats.Ages[age] != wantAges[age] {
					t.Errorf("age %s = %d, want %d", age, stats.Ages[age], wantAges[age])
				}
			}
			// the stale post-quantum session is not counted as a fresh group
			if stats.KeyExchanges["X25519MLKEM768"] != 2 || stats.KeyExchanges["X25519"] != 1 || stats.KeyExchanges[ProviderCapabilityKeyExchangeOther] != 0 || len(stats.KeyExchanges) != 3 {
				t.Errorf("key exchanges = %v", stats.KeyExchanges)
			}
		}, server.OptNoRetry())
	})
}

// An empty fleet is a snapshot of zeros, not a missing one.
func TestProviderCapabilityStatsSqlEmptyFleet(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			_, err := tx.Exec(t.Context(), `
				CREATE TEMP TABLE network_client (client_id uuid PRIMARY KEY, active boolean, source_client_id uuid) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_location_reliability (client_id uuid, connected boolean, valid boolean) ON COMMIT DROP;
				CREATE TEMP TABLE provide_key (client_id uuid, provide_mode int) ON COMMIT DROP;
				CREATE TEMP TABLE provider_modern_evidence (client_id uuid PRIMARY KEY, post_quantum bool, key_exchange varchar(64), window_resizable bool, measured_at timestamp) ON COMMIT DROP;
			`)
			server.Raise(err)
			stats, err := readProviderCapabilityStats(t.Context(), tx)
			if err != nil || stats.ObservedAt.IsZero() || stats.Ages[ProviderCapabilityAgeNone] != 0 || len(stats.Pairs) != len(ProviderCapabilityPairs) {
				t.Fatalf("empty fleet stats=%v err=%v", stats, err)
			}
		}, server.OptNoRetry())
	})
}

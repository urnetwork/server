// All public provider statistics reuse their PostgreSQL owner for payout policy.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

// A completed request retains only backend identity and bounded query counts.
type providerPayoutConnectionCounts struct {
	ownerPid          uint32
	acquires          int
	held              int
	peakHeld          int
	nestedAcquires    int
	foreignStatements int
	statements        int
	schemaReads       int
	boundaryReads     int
	payoutReads       int
}

// The tripwire rejects a forbidden acquisition immediately, so the single-slot
// regression fails from the ownership violation rather than a timeout or sleep.
type providerPayoutConnectionObserver struct {
	stateLock sync.Mutex
	counts    providerPayoutConnectionCounts
}

// Every PostgreSQL pool participates in the same acquisition tripwire.
func (self *providerPayoutConnectionObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts.acquires++
	if self.counts.held != 0 {
		self.counts.nestedAcquires++
		panic(errors.New("synthetic provider stats nested PostgreSQL acquisition"))
	}
	return ctx
}

// Successful acquisition owns a slot until pgx reports its release.
func (self *providerPayoutConnectionObserver) TraceAcquireEnd(_ context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if data.Err != nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts.ownerPid = data.Conn.PgConn().PID()
	self.counts.held++
	self.counts.peakHeld = max(self.counts.peakHeld, self.counts.held)
}

// Releasing the callback's connection ends its resource ownership.
func (self *providerPayoutConnectionObserver) TraceRelease(*pgxpool.Pool, pgxpool.TraceReleaseData) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts.held--
}

// Retained-boundary and payout reads share one backend without runtime catalogs.
func (self *providerPayoutConnectionObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts.statements++
	if self.counts.ownerPid != 0 && self.counts.ownerPid != conn.PgConn().PID() {
		self.counts.foreignStatements++
	}
	sql := strings.TrimSpace(data.SQL)
	if strings.Contains(sql, "migration_catalog") || strings.Contains(sql, "pg_trigger") || strings.Contains(sql, "pg_attribute") || strings.Contains(sql, "to_regclass") {
		self.counts.schemaReads++
	}
	if strings.HasPrefix(sql, "SELECT earning_identity, identity_sha256, initial_config_sha256, prepared_at FROM provider_payout_boundary") {
		self.counts.boundaryReads++
	}
	if strings.HasPrefix(sql, "WITH payout_sweeps AS MATERIALIZED") {
		self.counts.payoutReads++
	}
	return ctx
}

// The public API returns query errors; no query text or arguments are retained.
func (self *providerPayoutConnectionObserver) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Completed requests are checked before the next request resets observation.
func (self *providerPayoutConnectionObserver) takeCounts() providerPayoutConnectionCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	counts := self.counts
	self.counts = providerPayoutConnectionCounts{}
	return counts
}

// Absent optional configuration still checks for retained earning authority.
func TestProviderStatsPayoutConnectionWithoutPolicy(t *testing.T) {
	testProviderStatsPayoutConnection(t, "absent", nil)
}

// A prepared policy preserves delayed legacy revenue with only one pool slot.
func TestProviderStatsPayoutConnectionWithPolicy(t *testing.T) {
	testProviderStatsPayoutConnection(t, "prepared", nil)
}

// Readers never prepare authority while serving statistics.
func TestProviderStatsPayoutConnectionRejectsMissingBoundary(t *testing.T) {
	testProviderStatsPayoutConnection(t, "unprepared", server.ErrProviderEarningBoundaryUnprepared)
}

// Removing configuration must not reinterpret retained earning authority.
func TestProviderStatsPayoutConnectionRejectsRemovedPolicy(t *testing.T) {
	testProviderStatsPayoutConnection(t, "removed", server.ErrProviderEarningBoundaryMismatch)
}

// Explicit preflight reports schema drift; statistics read retained authority.
func TestProviderStatsPayoutConnectionLeavesGuardChecksToPreflight(t *testing.T) {
	testProviderStatsPayoutConnection(t, "changed_guard", nil)
}

// Exercise the three real database callbacks, including both public list aliases.
func testProviderStatsPayoutConnection(t *testing.T, mode string, wantErr error) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if policy, err := server.LoadProviderPayoutTransition(ctx); err != nil || policy != nil {
			t.Fatal("fixture must begin without a declared payout schedule", policy, err)
		}
		if mode != "absent" {
			data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", payoutTestCutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
			pop := server.Config.PushSimpleResource("sn.yml", []byte(data))
			defer pop()
			policy, err := server.LoadProviderPayoutTransition(ctx)
			if err != nil || policy == nil {
				t.Fatal("invalid synthetic schedule", err)
			}
			if mode != "unprepared" {
				if _, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "removed" {
				pop()
			}
			if mode == "changed_guard" {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE provider_payout_boundary DISABLE TRIGGER provider_payout_boundary_guard`))
				})
				if _, err := server.RequireProviderPayoutBoundary(ctx, policy); !errors.Is(err, server.ErrProviderEarningBoundarySchema) {
					t.Fatal("explicit preflight lost its guard diagnostic", err)
				}
			}
		}
		f := newPayoutTransitionCohort(t, ctx)
		statsInsertProvideKey(ctx, f.client, ProvideModePublic)
		closed := payoutTestCutoff.Add(-time.Minute)
		f.insert(t, ctx, closed.Add(-time.Hour), &closed, server.NowUtc().Add(-time.Hour), 1024, UsdToNanoCents(1))
		popPool := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popPool(); server.PgReset() }()
		observer := &providerPayoutConnectionObserver{}
		scope, err := server.NewTestPgQueryScope(ctx, observer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, request := range []struct {
			name string
			read func() (float64, bool, error)
		}{
			{name: "list", read: func() (float64, bool, error) {
				result, err := StatsProviders(f.session)
				if result == nil {
					return 0, false, err
				}
				if len(result.Providers) != 1 || result.Providers[0].ClientId != f.client {
					t.Fatal("list did not reach the synthetic provider", result)
				}
				return result.Providers[0].PayoutLast24h, true, err
			}},
			{name: "bounded_list", read: func() (float64, bool, error) {
				result, err := StatsProvidersLastN(&StatsProvidersArgs{LastN: 24}, f.session)
				if result == nil {
					return 0, false, err
				}
				if len(result.Providers) != 1 || result.Providers[0].ClientId != f.client {
					t.Fatal("bounded list did not reach the synthetic provider", result)
				}
				return result.Providers[0].PayoutLast24h, true, err
			}},
			{name: "detail", read: func() (float64, bool, error) {
				result, err := StatsProvider(&StatsProviderArgs{ClientId: f.client, LastN: 24}, f.session)
				if result == nil {
					return 0, false, err
				}
				return statsSumFloat(result.Payout), true, err
			}},
			{name: "overview", read: func() (float64, bool, error) {
				result, err := StatsProvidersOverview(&StatsProvidersOverviewArgs{LastN: 24}, f.session)
				if result == nil {
					return 0, false, err
				}
				return statsSumFloat(result.Payout), true, err
			}},
		} {
			started := time.Now()
			payout, present, err := request.read()
			elapsed := time.Since(started)
			counts := observer.takeCounts()
			if wantErr == nil {
				if err != nil || !present || payout != 1 {
					t.Errorf("%s/%s lost the delayed legacy payout: present=%t payout=%g error=%v", mode, request.name, present, payout, err)
				}
			} else if present || !errors.Is(err, wantErr) {
				t.Errorf("%s/%s changed boundary refusal: present=%t error=%v want=%v", mode, request.name, present, err, wantErr)
			}
			if counts.acquires != 1 || counts.nestedAcquires != 0 || counts.peakHeld != 1 || counts.held != 0 || counts.ownerPid == 0 || counts.foreignStatements != 0 {
				t.Errorf("%s/%s acquired another PostgreSQL owner: %+v", mode, request.name, counts)
			}
			wantBoundaryReads, wantPayoutReads := 1, 1
			if wantErr != nil {
				wantPayoutReads = 0
			}
			if counts.schemaReads != 0 || counts.boundaryReads != wantBoundaryReads || counts.payoutReads != wantPayoutReads || counts.statements > 11 {
				t.Errorf("%s/%s skipped validation or expanded its query budget: %+v", mode, request.name, counts)
			}
			t.Logf("mode=%s route=%s pool_slots=1 acquire_count=%d nested_acquires=%d peak_held=%d query_count=%d schema_reads=%d boundary_reads=%d payout_reads=%d duration=%s",
				mode, request.name, counts.acquires, counts.nestedAcquires, counts.peakHeld, counts.statements, counts.schemaReads, counts.boundaryReads, counts.payoutReads, elapsed)
		}
	})
}

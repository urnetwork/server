//go:build sidecar_pending

package quotahost

// QUOTA-M2-11's T3 run: quotasuite.Run, the same body FakeQuota passes at T2, on the Quota NewLocalQuota
// builds (model over the private stack's Postgres, its rates and reservations on the test's Redis), so a
// divergence between the fake and the real thing fails here; quota_api_t3_test.go runs it again with api's
// handlers between the Quota and model. And a cached network's Limits reaching no
// Postgres under DenyPostgresForTest.
//
// The target's levers, each what a real process does on its own time: SetLimits stands in for pro.yml and
// network_sidecar_limit rows, which a test cannot write per network, and forgets the cached value; Settle
// runs the lease set's round, the debit flush and the 60 s spent pass, which production runs on their own
// goroutines and taskworker. The lease set's rounds are driven (manualTicks), and a Spend the lease refuses
// first runs the round once, standing in for a stream whose next spend comes after the renewal's goroutine
// ran, the slow rate quotasuite's lease cases are written at (DESIGN-DETAILS 13.7).

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// model's QuotaSource with limits a test sets by hand.
type t3QuotaSource struct {
	QuotaSource
	stateLock     sync.Mutex
	networkLimits map[server.Id]*sidecar.QuotaLimits
}

func (self *t3QuotaSource) setLimits(networkId server.Id, limits *sidecar.QuotaLimits) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkLimits[networkId] = &sidecar.QuotaLimits{Tier: limits.Tier, Limits: maps.Clone(limits.Limits)}
}

func (self *t3QuotaSource) LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error) {
	self.stateLock.Lock()
	limits, ok := self.networkLimits[networkId]
	self.stateLock.Unlock()
	if ok {
		return cloneQuotaLimits(limits), nil
	}
	return self.QuotaSource.LoadLimits(ctx, networkId)
}

// This process's Redis, failing every call while a case says so.
type t3FailingRedis struct {
	redis   model.QuotaRateRedis
	failing atomic.Bool
}

func (self *t3FailingRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	if self.failing.Load() {
		return nil, errors.New("redis is failing")
	}
	return self.redis.RunScript(ctx, script, keys, args)
}

// The Quota whose leases run their round before a refused Spend answers (see the file header).
type t3Quota struct {
	*HostQuota
}

func (self *t3Quota) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	lease, refusal, err := self.HostQuota.Lease(ctx, networkId, meter)
	if lease == nil {
		return nil, refusal, err
	}
	return &t3Lease{lease: lease, set: self.leases}, nil, nil
}

type t3Lease struct {
	lease sidecar.QuotaLease
	set   *quotaLeaseSet
}

func (self *t3Lease) Spend(byteCount int64) bool {
	if self.lease.Spend(byteCount) {
		return true
	}
	self.set.tickAll(self.set.ctx)
	return self.lease.Spend(byteCount)
}

func (self *t3Lease) Close() {
	self.lease.Close()
}

// The lease settings T3 draws at: small, so a case moves a few bytes.
func t3LeaseSettings() *LeaseSettings {
	settings := DefaultLeaseSettings()
	settings.DrawByteCount = 64 * 1024
	settings.CommunityDrawByteCount = 16 * 1024
	return settings
}

// Replaces a network's transfer balances, an open start or end reading a century back or ahead.
func t3SetBalances(ctx context.Context, now time.Time, networkId server.Id, balances []*sidecartest.QuotaBalance) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE network_id = $1`, networkId))
		for _, balance := range balances {
			balanceId := server.Id(balance.BalanceId)
			if balanceId == (server.Id{}) {
				balanceId = server.NewId()
			}
			startTime := balance.StartTime
			if startTime.IsZero() {
				startTime = now.AddDate(-100, 0, 0)
			}
			endTime := balance.EndTime
			if endTime.IsZero() {
				endTime = now.AddDate(100, 0, 0)
			}
			var grantKind *string
			if balance.Credit {
				credit := "team_credit"
				grantKind = &credit
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_balance (
						balance_id,
						network_id,
						start_time,
						end_time,
						start_balance_byte_count,
						net_revenue_nano_cents,
						balance_byte_count,
						pro,
						grant_kind
					)
					VALUES ($1, $2, $3, $4, $5, 0, $5, false, $6)
				`,
				balanceId,
				networkId,
				startTime.UTC(),
				endTime.UTC(),
				balance.ByteCount,
				grantKind,
			))
		}
	})
}

// What the balance's reservations hold in Redis: its lease and carved tokens.
func t3ReservedByteCount(ctx context.Context, balanceId server.Id) int64 {
	total := int64(0)
	server.Redis(ctx, func(client server.RedisClient) {
		tokenAmounts, err := client.HGetAll(ctx, "{escrow_"+balanceId.String()+"}approx_contracts").Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for _, amount := range tokenAmounts {
			value, err := strconv.ParseInt(amount, 10, 64)
			server.Raise(err)
			total += value
		}
	})
	return total
}

// A network's balances in draw order as Postgres holds them, each with what Redis reserves of it.
func t3ReadBalances(ctx context.Context, networkId server.Id) (balances []*sidecartest.QuotaBalance) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT balance_id, balance_byte_count, grant_kind IS NOT DISTINCT FROM 'team_credit', start_time, end_time
				FROM transfer_balance
				WHERE network_id = $1
				ORDER BY grant_kind IS NOT DISTINCT FROM 'team_credit', end_time, start_time, balance_id
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				balance := &sidecartest.QuotaBalance{}
				server.Raise(result.Scan(&balanceId, &balance.ByteCount, &balance.Credit, &balance.StartTime, &balance.EndTime))
				balance.BalanceId = connect.Id(balanceId)
				balances = append(balances, balance)
			}
		})
	})
	for _, balance := range balances {
		balance.ReservedByteCount = t3ReservedByteCount(ctx, server.Id(balance.BalanceId))
	}
	return
}

// The debit flush over every shard, as taskworker's chains run it.
func t3FlushServiceDebits(ctx context.Context, clock sidecar.Clock) error {
	for shard := range model.ServiceDebitShardCount {
		var after *server.Id
		for range 1000 {
			result, err := model.FlushServiceDebits(ctx, shard, after, clock)
			if err != nil {
				return err
			}
			if 0 < result.Failed {
				return errors.New("a flush failed on a balance")
			}
			if result.LastBalanceId == nil {
				break
			}
			after = result.LastBalanceId
		}
	}
	return nil
}

func newT3Target(ctx context.Context) quotasuite.NewTarget {
	return newT3TargetOn(ctx, func(t *testing.T, clock sidecar.Clock) QuotaSource {
		return NewLocalQuotaSource(clock)
	})
}

// The T3 target on the QuotaSource newSource builds for each case at the case's clock: model directly
// (newT3Target), or api's own handlers over HTTP (quota_api_t3_test.go).
func newT3TargetOn(ctx context.Context, newSource func(t *testing.T, clock sidecar.Clock) QuotaSource) quotasuite.NewTarget {
	return func(t *testing.T) *quotasuite.Target {
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		faults := sidecartest.NewFaultQueue()
		redis := &t3FailingRedis{redis: model.NewQuotaRateServerRedis(10 * time.Second)}
		source := &t3QuotaSource{QuotaSource: newSource(t, clock), networkLimits: map[server.Id]*sidecar.QuotaLimits{}}
		settings := DefaultQuotaSettings()
		settings.Lease = t3LeaseSettings()
		quota, err := newHostQuota(ctx, source, redis, clock, settings)
		if err != nil {
			t.Fatal(err)
		}
		quota.leases.manualTicks = true
		t.Cleanup(func() { quota.Drain(context.Background()) })
		return &quotasuite.Target{
			Quota:  sidecartest.NewFaultQuota(&t3Quota{HostQuota: quota}, faults),
			Clock:  clock,
			Faults: faults,
			SetLimits: func(networkId connect.Id, limits *sidecar.QuotaLimits) error {
				source.setLimits(server.Id(networkId), limits)
				quota.forgetLimits(server.Id(networkId))
				return nil
			},
			SetBalances: func(networkId connect.Id, balances []*sidecartest.QuotaBalance) error {
				t3SetBalances(ctx, clock.Now(), server.Id(networkId), balances)
				quota.leases.forgetBalanceRead(server.Id(networkId))
				return nil
			},
			ReadBalances: func(networkId connect.Id) ([]*sidecartest.QuotaBalance, error) {
				return t3ReadBalances(ctx, server.Id(networkId)), nil
			},
			Settle: func() error {
				quota.leases.tickAll(ctx)
				if err := t3FlushServiceDebits(ctx, clock); err != nil {
					return err
				}
				// the rounds again: a closed lease whose last carve the flush settled leaves the set
				quota.leases.tickAll(ctx)
				_, err := model.ClearSpentNetworks(ctx, clock.Now(), nil)
				return err
			},
			LoseRedis: func() error {
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.FlushDB(ctx).Err())
				})
				return nil
			},
			SetRedisFailing: func(failing bool) error {
				redis.failing.Store(failing)
				return nil
			},
			LeaseDrawByteCount:          settings.Lease.DrawByteCount,
			CommunityLeaseDrawByteCount: settings.Lease.CommunityDrawByteCount,
			LeaseRenewFraction:          1 - settings.Lease.RenewFraction,
		}
	}
}

// newT3TargetOn for this package's external test, which reaches api's handlers: an import this package's own
// tests cannot make, since the handlers import quotahost.
var NewT3TargetOn = newT3TargetOn

// quotasuite on NewLocalQuota's parts at T3: the body FakeQuota passes at T2.
func TestLocalQuotaConformsToTheSuite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		quotasuite.Run(t, newT3Target(t.Context()))
	})
}

// A network whose limits are cached answers Limits with no Postgres acquisition, inside LimitsTtl (§20.7's
// cache; I-12's inline paths read only this).
func TestLocalQuotaLimitsCacheHitReachesNoPostgres(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		quota, err := NewLocalQuota(ctx, nil, clock, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer quota.Drain(context.Background())
		networkId := connect.NewId()
		first, err := quota.Limits(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		attempts, stop := server.DenyPostgresForTest(t)
		defer stop()
		clock.Advance(quota.settings.LimitsTtl - time.Millisecond)
		for range 100 {
			limits, err := quota.Limits(ctx, networkId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, limits, first)
		}
		connect.AssertEqual(t, attempts(), uint64(0))
	})
}

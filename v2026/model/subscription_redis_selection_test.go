// Actual PG rows and Redis counters discriminate selection work, ownership and
// funding policy. Existing public prober roots retain payer-hash expectations.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026"
)

// Count every actual row crossing each grant Query. This composes the existing
// real-query fixture; no production observer or global hook is added.
type redisSelectionTestTx struct {
	*escrowGrantQueryTestTx
	pageCounts []int
}

func (self *redisSelectionTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	before := self.grantQueries
	rows, err := self.escrowGrantQueryTestTx.Query(ctx, sql, args...)
	if err != nil || self.grantQueries == before {
		return rows, err
	}
	index := len(self.pageCounts)
	self.pageCounts = append(self.pageCounts, 0)
	return &redisSelectionTestRows{Rows: rows, next: func() { self.pageCounts[index]++ }}, nil
}

type redisSelectionTestRows struct {
	pgx.Rows
	next func()
}

func (self *redisSelectionTestRows) Next() bool {
	if !self.Rows.Next() {
		return false
	}
	self.next()
	return true
}

func redisSelectionCreateCounted(t testing.TB, ctx context.Context, clients escrowSelectionTestClients, amount ByteCount) (*TransferEscrow, *redisSelectionTestTx, error) {
	t.Helper()
	var observed *redisSelectionTestTx
	escrow, err := runRedisContractAdmission(ctx, func(owned context.Context) (*TransferEscrow, error) {
		var escrow *TransferEscrow
		var failure error
		server.Tx(owned, func(tx server.PgTx) {
			observed = &redisSelectionTestTx{escrowGrantQueryTestTx: &escrowGrantQueryTestTx{PgTx: tx}}
			escrow, _, failure = createTransferEscrowInTx(owned, observed, clients.payerNetworkId, clients.payerId,
				clients.providerNetworkId, clients.providerId, clients.payerNetworkId, amount, nil)
		}, server.TxReadCommitted, server.OptNoRetry())
		return escrow, failure
	})
	return escrow, observed, err
}

// Unique synthetic fixed times make both indexes' orders observable. Funding
// metadata is identical except an explicitly selected paid grant below.
func redisSelectionTestGrants(t testing.TB, ctx context.Context, clients escrowSelectionTestClients, count int, amount ByteCount, preferred bool) []server.Id {
	t.Helper()
	ids := make([]server.Id, count)
	start := server.NowUtc().Add(-time.Hour)
	end := start.Add(2 * time.Hour)
	initial := amount
	if preferred {
		initial = ProberTransferBalanceTopUp
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for index := range ids {
				ids[index] = server.NewId()
				batch.Queue(`INSERT INTO transfer_balance
					(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
					VALUES($1,$2,$3,$4,$5,$6,0,false)`, ids[index], clients.payerNetworkId,
					start.Add(time.Duration(index)*time.Millisecond), end.Add(time.Duration(index)*time.Millisecond), initial, amount)
			}
		})
	})
	return ids
}

// Existing legacy debt is real SQL custody as well as the Redis mirror. Tests
// cannot make free credit by merely removing an unexplained positive counter.
func redisSelectionReserveLegacy(t testing.TB, ctx context.Context, clients escrowSelectionTestClients, ids []server.Id, amount ByteCount) {
	t.Helper()
	contract := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id)
			VALUES($1,$2,$3,$4,$5,$6,$2)`, contract, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, amount*ByteCount(len(ids))))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, id := range ids {
				batch.Queue(`INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,$3)`, contract, id, amount)
			}
		})
	})
	server.Redis(ctx, func(client server.RedisClient) {
		for _, id := range ids {
			server.Raise(client.Set(ctx, netEscrowKey(id), amount, time.Hour).Err())
		}
	})
}

func TestRedisGrantDiscoveryBoundsManyCurrentRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		ids := redisSelectionTestGrants(t, ctx, clients, 4096, 4096, true)
		escrow, query, err := redisSelectionCreateCounted(t, ctx, clients, 1024)
		if err != nil || escrow == nil || query == nil {
			t.Fatal("actual prober writer failed", escrow, err)
		}
		if query.grantQueries != 1 || query.grantRows != proberGrantFirstCount || len(query.pageCounts) != 1 || query.pageCounts[0] != proberGrantFirstCount {
			t.Fatal("Redis prober discovery read its whole current grant history", query.grantQueries, query.grantRows, query.pageCounts)
		}
		want := ids[len(ids)-1-int(clients.payerId.Hash()%proberGrantFirstCount)]
		if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != want || escrow.Balances[0].BalanceByteCount != 1024 || escrow.Priority != UnpaidPriority {
			t.Fatal("bounded Redis discovery changed payer spread or exact funding", escrow)
		}
	})
}

func TestRedisGrantFallbackPagesKeepFinancialOrderAndPriority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 2*redisGrantPageRows+3, 1024, false)
		redisSelectionReserveLegacy(t, ctx, clients, ids[:2*redisGrantPageRows], 1024)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, ids[2*redisGrantPageRows]))
		})
		escrow, query, err := redisSelectionCreateCounted(t, ctx, clients, 1536)
		if err != nil || escrow == nil || query == nil {
			t.Fatal("paged fallback failed", escrow, err)
		}
		if query.grantQueries != 4 || query.grantRows != len(ids) || len(query.pageCounts) != 4 || query.pageCounts[0] != 0 {
			t.Fatal("fallback repeated or omitted a keyset page", query.grantQueries, query.grantRows, query.pageCounts)
		}
		for _, count := range query.pageCounts {
			if count > redisGrantPageRows {
				t.Fatal("one grant query exceeded its decoded row budget", count)
			}
		}
		allocations := map[server.Id]ByteCount{}
		for _, balance := range escrow.Balances {
			allocations[balance.BalanceId] = balance.BalanceByteCount
		}
		if len(allocations) != 2 || allocations[ids[128]] != 1024 || allocations[ids[129]] != 512 || escrow.Priority != (PaidPriority+UnpaidPriority)/2 {
			t.Fatal("paged fallback repriced or reordered financial grants", escrow)
		}
		if got := Testing_NetEscrowByteCount(ctx, ids[0]); got != 1024 {
			t.Fatal("fallback changed earlier retained legacy debt", got)
		}
	})
}

func TestRedisGrantCapacityRetainsDebtAndLargerOwnerCanContinue(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, redisGrantPageRows+2, 1024, false)
		redisSelectionReserveLegacy(t, ctx, clients, ids[:redisGrantPageRows+1], 1024)
		bounded := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 16})
		var beforeWarning, afterWarning, declaredRows dto.Metric
		server.Raise(redisContractReservationResults.WithLabelValues("selection", "capacity-warning").Write(&beforeWarning))
		escrow, query, err := redisSelectionCreateCounted(t, bounded, clients, 17)
		var capacity *redisGrantSelectionCapacityError
		if escrow != nil || !errors.As(err, &capacity) || errors.Is(err, errRedisReservationInsufficient) || query == nil || query.grantRows != 64 || capacity.Rows != 64 || capacity.LastBalance != ids[63] {
			t.Fatal("selection cap became insufficient funding or lost its examined cursor", escrow, query, err)
		}
		server.Raise(redisContractReservationResults.WithLabelValues("selection", "capacity-warning").Write(&afterWarning))
		server.Raise(redisGrantSelectionLimitsMetric.WithLabelValues("rows").Write(&declaredRows))
		if afterWarning.GetCounter().GetValue() < beforeWarning.GetCounter().GetValue()+1 || declaredRows.GetGauge().GetValue() != 64 {
			t.Fatal("selection capacity warning or declared limit was not observable")
		}
		for _, id := range ids[:65] {
			if got := Testing_NetEscrowByteCount(ctx, id); got != 1024 {
				t.Fatal("capacity hold erased retained debt", got)
			}
		}
		larger := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 128, MaxSelected: 16})
		escrow, err = clients.create(larger, 17, false)
		if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[65] || escrow.Balances[0].BalanceByteCount != 17 {
			t.Fatal("larger reviewed owner could not observe later valid credit", escrow, err)
		}
	})
}

func TestRedisGrantSelectedCapacityCompensatesEveryPartialToken(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 4, 1, false)
		bounded := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 2})
		escrow, err := clients.create(bounded, 4, false)
		var capacity *redisGrantSelectionCapacityError
		if escrow != nil || !errors.As(err, &capacity) || capacity.Selected != 2 {
			t.Fatal("selected capacity was not an explicit partial-allocation hold", escrow, err)
		}
		for _, id := range ids {
			if got := Testing_NetEscrowByteCount(ctx, id); got != 0 {
				t.Fatal("selection capacity retained a refused partial token", got)
			}
		}
		larger := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 4})
		escrow, err = clients.create(larger, 4, false)
		if err != nil || escrow == nil || len(escrow.Balances) != 4 || escrow.TransferByteCount != 4 {
			t.Fatal("larger selection owner duplicated or lost exact available credit", escrow, err)
		}
	})
}

func TestRedisGrantUnknownCounterPreservesDebtAndHealthyPeer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, counter := range []string{"-1024", "malformed"} {
			clients := newEscrowSelectionTestClients(t, ctx)
			ids := redisSelectionTestGrants(t, ctx, clients, 2, 1024, false)
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, netEscrowKey(ids[0]), counter, time.Hour).Err())
			})
			escrow, err := clients.create(ctx, 17, false)
			if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[1] || escrow.Priority != UnpaidPriority {
				t.Fatal("unknown counter stopped an independently valid funding peer", counter, escrow, err)
			}
			server.Redis(ctx, func(client server.RedisClient) {
				value, err := client.Get(ctx, netEscrowKey(ids[0])).Result()
				server.Raise(err)
				if value != counter {
					t.Fatal("unknown counter was reset to available zero", value)
				}
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, ids[1], server.NowUtc().Add(-time.Second)))
			})
			escrow, err = clients.create(ctx, 17, false)
			if escrow != nil || err == nil || errors.Is(err, errRedisReservationInsufficient) || !strings.Contains(err.Error(), "counter remains unknown") {
				t.Fatal("unresolved funding was reported as known insufficient balance", escrow, err)
			}
		}
	})
}

func TestRedisGrantWholePreferredAttemptDoesNotKeepPartialToken(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		ids := redisSelectionTestGrants(t, ctx, clients, 4, 100, true)
		position := int(clients.payerId.Hash() % 4)
		first := ids[3-position]
		second := ids[3-(position+1)%4]
		redisSelectionReserveLegacy(t, ctx, clients, []server.Id{first}, 50)
		escrow, err := clients.create(ctx, 100, false)
		if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != second || escrow.Balances[0].BalanceByteCount != 100 {
			t.Fatal("whole preferred attempt left partial authority or changed the next preference", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, first); got != 50 {
			t.Fatal("rejected whole-grant candidate retained partial requested bytes", got)
		}
	})
}

func TestRedisGrantPostWaitExpiryRefusesAndCompensates(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, f.destinationId))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		done := make(chan error, 1)
		go func() {
			var err error
			value := server.HandleError(func() {
				_, err = CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
			})
			if value != nil {
				if cause, ok := value.(error); ok {
					err = cause
				} else {
					err = errors.New("unexpected non-error admission panic")
				}
			}
			done <- err
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("expiry barrier preceded actual reservation", got)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, server.NowUtc().Add(-time.Second)))
		})
		server.Raise(held.Rollback(ctx))
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "selected Redis grant expired") {
				t.Fatal("publication used a grant which expired during endpoint wait", err)
			}
		case <-ctx.Done():
			t.Fatal("post-wait expiry did not join", ctx.Err())
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("post-wait expiry changed existing reservation or kept refused bytes", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

func TestRedisGrantReviewedLimitsAreFiniteAndStrict(t *testing.T) {
	for _, text := range []string{"max_rows: 64\nmax_selected: 1\n", "max_rows: 65536\nmax_selected: 4096\n"} {
		if _, err := parseRedisGrantSelectionLimits([]byte(text)); err != nil {
			t.Fatal("reviewed finite capacity could not be read", err)
		}
	}
	for _, text := range []string{"", "max_rows: 63\nmax_selected: 1\n", "max_rows: 65537\nmax_selected: 1\n", "max_rows: 64\nmax_selected: 65\n", "max_rows: 64\nmax_selected: 0\n", "max_rows: 64\nmax_selected: 1\nunknown: true\n", "max_rows: 64\nmax_selected: 1\n---\nmax_rows: 64\n", strings.Repeat("x", 4097)} {
		if _, err := parseRedisGrantSelectionLimits([]byte(text)); err == nil {
			t.Fatal("malformed or unbounded selection profile was accepted")
		}
	}
}

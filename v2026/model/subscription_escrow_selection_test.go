// Escrow allocation uses only available grants for positive-byte contracts;
// zero-byte anchors retain their existing balance and priority semantics.
package model

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Independent payer/provider identities keep table cases isolated without
// resetting the database or changing any process-wide configuration.
type escrowSelectionTestClients struct {
	payerNetworkId    server.Id
	payerId           server.Id
	providerNetworkId server.Id
	providerId        server.Id
}

// Creates active clients only; each case supplies its own ordered grants.
func newEscrowSelectionTestClients(t testing.TB, ctx context.Context) escrowSelectionTestClients {
	t.Helper()
	clients := escrowSelectionTestClients{
		payerNetworkId: server.NewId(), payerId: server.NewId(),
		providerNetworkId: server.NewId(), providerId: server.NewId(),
	}
	insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
		clients.payerId: clients.payerNetworkId, clients.providerId: clients.providerNetworkId,
	})
	return clients
}

// Exercises either public creation path against the same payer's grants.
func (self escrowSelectionTestClients) create(ctx context.Context, byteCount ByteCount, companion bool) (*TransferEscrow, error) {
	if companion {
		return CreateCompanionTransferEscrow(ctx, self.providerNetworkId, self.providerId,
			self.payerNetworkId, self.payerId, byteCount, time.Hour)
	}
	return CreateTransferEscrow(ctx, self.payerNetworkId, self.payerId,
		self.providerNetworkId, self.providerId, byteCount)
}

// Availability tests need a durable reservation behind the mirrored bytes.
// Redis-only amounts are drift, which an authoritative post correctly repairs.
func (self escrowSelectionTestClients) reserve(ctx context.Context, balanceId server.Id, byteCount ByteCount) {
	contractId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id)
			VALUES ($1,$2,$3,$4,$5,$6,$2)`, contractId,
			self.payerNetworkId, self.payerId, self.providerNetworkId, self.providerId, byteCount))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
			VALUES ($1,$2,$3)`, contractId, balanceId, byteCount))
	}, server.TxReadCommitted)
}

// Exact and excessive reservations must not affect funding, priority, or
// mirror deadlines in either origin or companion creation.
func TestCreateTransferEscrowReservedGrantsDoNotAffectPriorityOrMirrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, legacy := range []bool{false, true} {
			for _, test := range []struct {
				name      string
				paid      bool
				companion bool
			}{
				{name: "paid origin", paid: true},
				{name: "unpaid origin"},
				{name: "paid companion", paid: true, companion: true},
				{name: "unpaid companion", companion: true},
			} {
				clients := newEscrowSelectionTestClients(t, ctx)
				if test.companion {
					// Make the anchor before grants exist so it cannot touch the
					// reservation mirrors whose exact deadlines this case checks.
					if _, err := clients.create(ctx, 0, false); err != nil {
						t.Fatal(err)
					}
				}
				now := server.NowUtc()
				expiration := now.Truncate(time.Second).Add(time.Hour)
				balances := []*TransferBalance{}
				reservedByteCounts := []ByteCount{2048, 3072, 1024, 0}
				for index := range reservedByteCounts {
					paid := test.paid
					if index < 2 {
						paid = !paid
					}
					balance := &TransferBalance{
						NetworkId: clients.payerNetworkId,
						StartTime: now.Add(-time.Minute), EndTime: now.Add(time.Duration(index+1) * 24 * time.Hour),
						StartBalanceByteCount: 2048, BalanceByteCount: 2048,
					}
					if paid {
						balance.NetRevenue = 2048
					}
					AddTransferBalance(ctx, balance)
					balances = append(balances, balance)
					if 0 < reservedByteCounts[index] {
						clients.reserve(ctx, balance.BalanceId, reservedByteCounts[index])
						server.Redis(ctx, func(r server.RedisClient) {
							key := netEscrowKey(balance.BalanceId)
							server.Raise(r.Set(ctx, key, reservedByteCounts[index], 0).Err())
							server.Raise(r.ExpireAt(ctx, key, expiration).Err())
						})
					}
				}
				var escrow *TransferEscrow
				var err error
				if legacy {
					if test.companion {
						escrow, err = createCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId, clients.payerNetworkId, clients.payerId, 1536, time.Hour)
					} else {
						escrow, err = createTransferEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 1536)
					}
				} else {
					escrow, err = clients.create(ctx, 1536, test.companion)
				}
				if err != nil {
					t.Fatalf("%s: %v", test.name, err)
				}
				wantPriority := Priority(UnpaidPriority)
				if test.paid {
					wantPriority = PaidPriority
				}
				if escrow.Priority != wantPriority {
					t.Errorf("%s: priority = %d, want %d", test.name, escrow.Priority, wantPriority)
				}
				wantBalanceIdByteCounts := map[server.Id]ByteCount{
					balances[2].BalanceId: 1024, balances[3].BalanceId: 512,
				}
				gotBalanceIdByteCounts := map[server.Id]ByteCount{}
				for _, balance := range escrow.Balances {
					gotBalanceIdByteCounts[balance.BalanceId] = balance.BalanceByteCount
				}
				if !maps.Equal(gotBalanceIdByteCounts, wantBalanceIdByteCounts) {
					t.Errorf("%s: returned allocation contains %d grants, want two correctly funded grants", test.name, len(escrow.Balances))
				}
				server.Db(ctx, func(conn server.PgConn) {
					result, err := conn.Query(ctx, `
					SELECT transfer_escrow.balance_id, transfer_escrow.balance_byte_count, transfer_contract.priority
					FROM transfer_escrow INNER JOIN transfer_contract USING (contract_id)
					WHERE contract_id = $1
				`, escrow.ContractId)
					gotBalanceIdByteCounts = map[server.Id]ByteCount{}
					server.WithPgResult(result, err, func() {
						for result.Next() {
							var balanceId server.Id
							var byteCount ByteCount
							var priority Priority
							server.Raise(result.Scan(&balanceId, &byteCount, &priority))
							gotBalanceIdByteCounts[balanceId] = byteCount
							if priority != wantPriority {
								t.Errorf("%s: stored priority = %d, want %d", test.name, priority, wantPriority)
							}
						}
					})
				})
				if !maps.Equal(gotBalanceIdByteCounts, wantBalanceIdByteCounts) {
					t.Errorf("%s: stored allocation contains %d grants, want two correctly funded grants", test.name, len(gotBalanceIdByteCounts))
				}
				var marked int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved`, escrow.ContractId).Scan(&marked))
				})
				if (legacy && marked != 0) || (!legacy && marked != 2) {
					t.Fatalf("mode marker legacy=%t rows=%d", legacy, marked)
				}
				if !legacy {
					for _, balance := range balances {
						want := reservedByteCounts[slices.Index(balances, balance)] + wantBalanceIdByteCounts[balance.BalanceId]
						if got := Testing_NetEscrowByteCount(ctx, balance.BalanceId); got != want {
							t.Fatalf("combined ledger=%d want%d", got, want)
						}
					}
				}
				server.Redis(ctx, func(r server.RedisClient) {
					for index, balance := range balances {
						key := netEscrowKey(balance.BalanceId)
						got, err := r.Get(ctx, key).Int64()
						want := reservedByteCounts[index]
						if legacy {
							want += wantBalanceIdByteCounts[balance.BalanceId]
						}
						if err != nil && !(errors.Is(err, redis.Nil) && want == 0) {
							server.Raise(err)
						}
						if got != want {
							t.Errorf("%s: grant %d mirror = %d, want %d", test.name, index, got, want)
						}
						if index < 2 {
							gotExpiration, err := r.ExpireTime(ctx, key).Result()
							server.Raise(err)
							if gotExpiration != time.Duration(expiration.Unix())*time.Second {
								t.Errorf("%s: exhausted grant %d mirror deadline was rewritten", test.name, index)
							}
						}
					}
				})
			}
		}
	})
}

// Zero-byte contracts remain valid without grants, or keep the earliest
// grant's zero-byte anchor and priority even when its mirror is fully reserved.
func TestCreateTransferEscrowZeroByteCompatibility(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, test := range []struct {
			name     string
			balances int
			paid     bool
			reserved bool
		}{
			{name: "no grants"},
			{name: "paid grant", balances: 2, paid: true},
			{name: "reserved paid grant", balances: 2, paid: true, reserved: true},
			{name: "reserved unpaid grant", balances: 2, reserved: true},
		} {
			clients := newEscrowSelectionTestClients(t, ctx)
			now := server.NowUtc()
			var firstBalanceId server.Id
			for index := range test.balances {
				balance := &TransferBalance{
					NetworkId: clients.payerNetworkId,
					StartTime: now.Add(-time.Minute), EndTime: now.Add(time.Duration(index+1) * 24 * time.Hour),
					StartBalanceByteCount: 2048, BalanceByteCount: 2048,
				}
				if test.paid {
					balance.NetRevenue = 2048
				}
				AddTransferBalance(ctx, balance)
				if index == 0 {
					firstBalanceId = balance.BalanceId
					if test.reserved {
						clients.reserve(ctx, firstBalanceId, 2048)
						server.Redis(ctx, func(r server.RedisClient) {
							server.Raise(r.Set(ctx, netEscrowKey(firstBalanceId), 2048, time.Hour).Err())
						})
					}
				}
			}
			for _, companion := range []bool{false, true} {
				escrow, err := clients.create(ctx, 0, companion)
				if err != nil {
					t.Fatalf("%s companion=%t: %v", test.name, companion, err)
				}
				wantBalanceCount := min(test.balances, 1)
				wantPriority := Priority(UnpaidPriority)
				if test.paid {
					wantPriority = PaidPriority
				}
				if len(escrow.Balances) != wantBalanceCount || escrow.Priority != wantPriority || escrow.TransferByteCount != 0 {
					t.Fatalf("%s companion=%t: balances=%d priority=%d bytes=%d", test.name, companion,
						len(escrow.Balances), escrow.Priority, escrow.TransferByteCount)
				}
				if 0 < wantBalanceCount && (escrow.Balances[0].BalanceId != firstBalanceId || escrow.Balances[0].BalanceByteCount != 0) {
					t.Fatalf("%s companion=%t: zero-byte anchor did not retain the earliest grant", test.name, companion)
				}
				if test.reserved && Testing_NetEscrowByteCount(ctx, firstBalanceId) != 2048 {
					t.Fatalf("%s companion=%t: zero-byte contract changed reserved bytes", test.name, companion)
				}
			}
		}
	})
}

// Skipping empty grants cannot turn a partial reservation into a funded
// contract or publish mirror writes from an insufficient-balance attempt.
func TestCreateTransferEscrowReservedGrantShortfallHasNoWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, companion := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			wantContractCount := 0
			if companion {
				if _, err := clients.create(ctx, 0, false); err != nil {
					t.Fatal(err)
				}
				wantContractCount = 1
			}
			now := server.NowUtc()
			balances := []*TransferBalance{}
			for index := range 2 {
				balance := &TransferBalance{
					NetworkId: clients.payerNetworkId,
					StartTime: now.Add(-time.Minute), EndTime: now.Add(time.Duration(index+1) * 24 * time.Hour),
					StartBalanceByteCount: 2048, BalanceByteCount: 2048,
				}
				AddTransferBalance(ctx, balance)
				balances = append(balances, balance)
			}
			clients.reserve(ctx, balances[0].BalanceId, 2048)
			wantContractCount++
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, netEscrowKey(balances[0].BalanceId), 2048, time.Hour).Err())
			})
			escrow, err := clients.create(ctx, 3072, companion)
			if err == nil || escrow != nil {
				t.Fatalf("companion=%t: insufficient balance returned escrow=%v error=%v", companion, escrow, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var contractCount, escrowCount int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id = $1`, clients.payerNetworkId).Scan(&contractCount))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id = ANY($1)`, []server.Id{balances[0].BalanceId, balances[1].BalanceId}).Scan(&escrowCount))
				if contractCount != wantContractCount || escrowCount != 1 {
					t.Fatalf("companion=%t: shortfall wrote contracts=%d escrows=%d, want %d/1", companion, contractCount, escrowCount, wantContractCount)
				}
			})
			server.Redis(ctx, func(r server.RedisClient) {
				reserved, err := r.Get(ctx, netEscrowKey(balances[0].BalanceId)).Int64()
				server.Raise(err)
				exists, err := r.Exists(ctx, netEscrowKey(balances[1].BalanceId)).Result()
				server.Raise(err)
				if reserved != 2048 || exists != 0 {
					t.Fatalf("companion=%t: shortfall changed reservation mirrors", companion)
				}
			})
		}
	})
}

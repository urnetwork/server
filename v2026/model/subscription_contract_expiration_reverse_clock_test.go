// Reverse companion fixtures must fund their destination payer against the
// real database clock without changing the source payer's live reservations.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// The existing renewal root and this proof use the same reverse-payer grant
// constructor. Host-domain argument shifts never change PostgreSQL's clock.
func TestContractExpirationReversePayerFixtureReservesWhenHostTimeLeadsDatabase(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, c := range []struct {
			name        string
			hostLead    time.Duration
			wantCurrent bool
		}{
			{name: "already_current", hostLead: 30 * time.Minute, wantCurrent: true},
			{name: "genuinely_future", hostLead: 2 * time.Hour, wantCurrent: false},
		} {
			func() {
				ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 30*time.Second)
				defer cancel()
				fixture := newNetEscrowOrderingTestFixture(t, ctx)
				origin := createRedisAdmissionTest(ctx, fixture, 100)
				companion, err := CreateCompanionTransferEscrow(ctx, fixture.destinationNetworkId, fixture.destinationId,
					fixture.sourceNetworkId, fixture.sourceId, 100, time.Minute)
				if err != nil || companion == nil || companion.CompanionContractId == nil ||
					*companion.CompanionContractId != origin.ContractId || companion.TransferByteCount != 100 ||
					len(companion.Balances) != 1 || companion.Balances[0].BalanceId != fixture.balanceId || companion.Balances[0].BalanceByteCount != 100 {
					t.Fatalf("%s: public return companion setup changed its source payer or origin: error=%v escrow=%+v", c.name, err, companion)
				}
				var databaseNow time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&databaseNow))
				})
				clock := &netEscrowFixtureClock{offset: databaseNow.Add(c.hostLead).Sub(server.NowUtc())}
				if clock.offset <= 30*time.Second {
					t.Fatalf("%s: test host-clock shift does not exceed the whole operation bound: %s", c.name, clock.offset)
				}
				scope, err := server.NewTestPgQueryScope(ctx, clock)
				server.Raise(err)
				defer func() { server.Raise(scope.Close()) }()
				shifted := context.WithValue(ctx, netEscrowFixtureClockContextKey{}, clock)
				addContractExpirationReversePayerGrant(shifted, fixture.destinationNetworkId)
				balances := GetActiveTransferBalances(shifted, fixture.destinationNetworkId)
				if len(balances) != 1 {
					t.Fatalf("%s: reverse payer did not discover its one host-current grant: %d", c.name, len(balances))
				}
				balanceId := balances[0].BalanceId
				reverse, admissionErr := server.HandleError2(func() (*TransferEscrow, error) {
					return CreateCompanionTransferEscrow(shifted, fixture.sourceNetworkId, fixture.sourceId,
						fixture.destinationNetworkId, fixture.destinationId, 100, time.Minute)
				}, func(err error) (*TransferEscrow, error) { return nil, err })
				drainRedisContractCompensations(t, ctx)
				var current, neighborsCurrent bool
				var published, children, reservations, neighbors int
				var reservedBytes, neighborBytes ByteCount
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC'
 AND clock_timestamp() AT TIME ZONE 'UTC'<end_time FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&current))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_network_id=ANY($1)`,
						[]server.Id{fixture.sourceNetworkId, fixture.destinationNetworkId}).Scan(&published))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE companion_contract_id=$1`, companion.ContractId).Scan(&children))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(balance_byte_count),0)::bigint
 FROM transfer_escrow WHERE balance_id=$1`, balanceId).Scan(&reservations, &reservedBytes))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(escrow.balance_byte_count),0)::bigint,
 COALESCE(bool_and(contract.outcome IS NULL AND escrow.redis_reserved AND NOT escrow.settled),false)
 FROM transfer_escrow AS escrow JOIN transfer_contract AS contract USING(contract_id)
 WHERE escrow.balance_id=$1 AND contract.contract_id=ANY($2)`, fixture.balanceId,
						[]server.Id{origin.ContractId, companion.ContractId}).Scan(&neighbors, &neighborBytes, &neighborsCurrent))
				})
				var sourceLegacyBytes, sourceNativeBytes, reverseLegacyBytes, reverseNativeBytes ByteCount
				var sourceTokenAmountKVs, reverseTokenAmountKVs map[string]string
				server.Redis(ctx, func(client server.RedisClient) {
					readCounter := func(key string) ByteCount {
						amount, err := client.Get(ctx, key).Int64()
						if errors.Is(err, redis.Nil) {
							return 0
						}
						server.Raise(err)
						return ByteCount(amount)
					}
					sourceKeys := redisContractReservationKeys(fixture.balanceId)
					reverseKeys := redisContractReservationKeys(balanceId)
					sourceLegacyBytes, sourceNativeBytes = readCounter(sourceKeys[3]), readCounter(sourceKeys[0])
					reverseLegacyBytes, reverseNativeBytes = readCounter(reverseKeys[3]), readCounter(reverseKeys[0])
					var err error
					sourceTokenAmountKVs, err = client.HGetAll(ctx, sourceKeys[1]).Result()
					server.Raise(err)
					reverseTokenAmountKVs, err = client.HGetAll(ctx, reverseKeys[1]).Result()
					server.Raise(err)
				})
				if clock.invalidShape.Load() || clock.fixtureWrites.Load() != 1 || clock.activeReads.Load() != 1 ||
					clock.spreadReads.Load() != 1 || clock.fundingReads.Load() != 1 || clock.publicationChecks.Load() != 1 {
					t.Fatalf("%s: reverse clock proof did not traverse the exact grant/discovery/publication shapes: invalid=%t writes=%d active=%d spread=%d funding=%d publication=%d",
						c.name, clock.invalidShape.Load(), clock.fixtureWrites.Load(), clock.activeReads.Load(), clock.spreadReads.Load(), clock.fundingReads.Load(), clock.publicationChecks.Load())
				}
				if neighbors != 2 || neighborBytes != 200 || !neighborsCurrent || sourceLegacyBytes != 0 || sourceNativeBytes != 200 ||
					len(sourceTokenAmountKVs) != 2 || sourceTokenAmountKVs[origin.ContractId.String()] != "100" || sourceTokenAmountKVs[companion.ContractId.String()] != "100" {
					t.Fatalf("%s: reverse admission changed the source payer's live neighbors: rows=%d bytes=%d current=%t legacy=%d native=%d tokens=%d",
						c.name, neighbors, neighborBytes, neighborsCurrent, sourceLegacyBytes, sourceNativeBytes, len(sourceTokenAmountKVs))
				}
				if c.wantCurrent {
					if !current || admissionErr != nil || reverse == nil || reverse.CompanionContractId == nil ||
						*reverse.CompanionContractId != companion.ContractId || reverse.TransferByteCount != 100 ||
						len(reverse.Balances) != 1 || reverse.Balances[0].BalanceId != balanceId || reverse.Balances[0].BalanceByteCount != 100 ||
						published != 3 || children != 1 || reservations != 1 || reservedBytes != 100 || reverseLegacyBytes != 0 || reverseNativeBytes != 100 ||
						len(reverseTokenAmountKVs) != 1 || reverseTokenAmountKVs[reverse.ContractId.String()] != "100" {
						t.Fatalf("%s: host-current reverse payer did not publish its exact public companion: database_current=%t published=%d children=%d escrow_rows=%d escrow_bytes=%d error=%v escrow=%+v",
							c.name, current, published, children, reservations, reservedBytes, admissionErr, reverse)
					}
					server.Db(ctx, func(conn server.PgConn) {
						var payer, parent, source, destination server.Id
						var redisReserved bool
						server.Raise(conn.QueryRow(ctx, `SELECT contract.payer_network_id,contract.companion_contract_id,
 contract.source_network_id,contract.destination_network_id,escrow.redis_reserved
 FROM transfer_contract AS contract JOIN transfer_escrow AS escrow USING(contract_id)
 WHERE contract.contract_id=$1 AND escrow.balance_id=$2`, reverse.ContractId, balanceId).
							Scan(&payer, &parent, &source, &destination, &redisReserved))
						if payer != fixture.destinationNetworkId || parent != companion.ContractId || source != fixture.sourceNetworkId || destination != fixture.destinationNetworkId || !redisReserved {
							t.Fatalf("%s: published reverse companion changed destination-payer, parent or Redis custody", c.name)
						}
					})
				} else if current || reverse != nil || admissionErr == nil || admissionErr.Error() != "selected Redis grant expired or changed before contract publication" ||
					published != 2 || children != 0 || reservations != 0 || reservedBytes != 0 || reverseLegacyBytes != 0 || reverseNativeBytes != 0 || len(reverseTokenAmountKVs) != 0 {
					t.Fatalf("%s: real future reverse grant acquired publication or reservation authority: database_current=%t published=%d children=%d escrow_rows=%d escrow_bytes=%d error=%v escrow=%+v",
						c.name, current, published, children, reservations, reservedBytes, admissionErr, reverse)
				}
			}()
		}
	})
}

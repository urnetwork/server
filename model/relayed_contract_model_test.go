//go:build sidecar_pending

package model

// QUOTA-M2-08: relayed reservations at CreateContract (DESIGN.md 20.6), under -tags sidecar_pending,
// which links MG0's marks and MG2's journal and spent mark: the grant formula at its boundaries,
// grants that shrink near empty and stop at the balance (FS-45's first clause), a spent refusal that
// reserves nothing, a stream-bound pair that reserves nothing, a failed insert that releases, and the
// warm path's one script and no Postgres read.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

const testingRelayedFloor = 1 * Mib

// One network with two clients and its balances, each ending an hour after the one before.
type testingRelayedNetwork struct {
	networkId     server.Id
	sourceId      server.Id
	destinationId server.Id
	balanceIds    []server.Id
}

func newTestingRelayedNetwork(t testing.TB, ctx context.Context, balanceByteCounts ...ByteCount) *testingRelayedNetwork {
	t.Helper()
	network := &testingRelayedNetwork{
		networkId:     server.NewId(),
		sourceId:      server.NewId(),
		destinationId: server.NewId(),
	}
	insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
		network.sourceId:      network.networkId,
		network.destinationId: network.networkId,
	})
	now := server.NowUtc()
	for index, byteCount := range balanceByteCounts {
		network.balanceIds = append(network.balanceIds, testingInsertEscrowDrawBalance(t, ctx, network.networkId, byteCount, 0, nil,
			now.Add(-time.Hour), now.Add(time.Duration(index+1)*time.Hour)))
	}
	return network
}

func (self *testingRelayedNetwork) reserve(ctx context.Context, requested ByteCount) (server.Id, ByteCount, error) {
	contractId, grant, _, err := ReserveRelayedContract(ctx, self.networkId, self.sourceId, self.destinationId, requested, testingRelayedFloor, true)
	return contractId, grant, err
}

// Each balance's bytes held by Redis reservations.
func (self *testingRelayedNetwork) held(ctx context.Context) []ByteCount {
	held := []ByteCount{}
	for _, balanceId := range self.balanceIds {
		held = append(held, Testing_NetEscrowByteCount(ctx, balanceId))
	}
	return held
}

// The draw-order cache and settings fresh for one test.
func testingRelayedSettings(t testing.TB) {
	t.Helper()
	t.Cleanup(Testing_SetRelayedReservationSettings(DefaultRelayedReservationSettings()))
}

// grant = min(requested, max(floor, available / 8)), below the floor refused, read on one balance.
func TestRelayedGrantFormula(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		connect.AssertEqual(t, RelayedReservationEnabled(), true)
		for _, c := range []struct {
			room  ByteCount
			grant ByteCount
		}{
			// available / 8 past the request: the request
			{room: 1 * Gib, grant: 64 * Mib},
			// available / 8 under it
			{room: 256 * Mib, grant: 32 * Mib},
			// available / 8 under the floor: the floor
			{room: 4 * Mib, grant: 1 * Mib},
			// less than the floor available: refused
			{room: 512 * Kib, grant: 0},
		} {
			network := newTestingRelayedNetwork(t, ctx, c.room)
			contractId, grant, err := network.reserve(ctx, 64*Mib)
			drainRedisContractCompensations(t, ctx)
			if c.grant == 0 {
				if !errors.Is(err, errRedisReservationInsufficient) {
					t.Fatalf("room %d: %v, want a refusal", c.room, err)
				}
				connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
				continue
			}
			if err != nil {
				t.Fatalf("room %d: %v", c.room, err)
			}
			connect.AssertEqual(t, grant, c.grant)
			connect.AssertEqual(t, network.held(ctx), []ByteCount{c.grant})
			var payerNetworkId *server.Id
			var transferByteCount ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id, transfer_byte_count FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&payerNetworkId, &transferByteCount))
			})
			connect.AssertEqual(t, *payerNetworkId, network.networkId)
			connect.AssertEqual(t, transferByteCount, c.grant)
		}
	})
}

// FS-45's first clause: pairs on a network near empty get grants that shrink as the balance nears
// zero and stop at it; the refusal marks the network spent.
func TestRelayedGrantsShrinkAndStopAtTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		room := 100 * Mib
		network := newTestingRelayedNetwork(t, ctx, room)
		grants := []ByteCount{}
		var total ByteCount
		var refusal error
		for range 200 {
			_, grant, err := network.reserve(ctx, 64*Mib)
			if err != nil {
				refusal = err
				break
			}
			grants = append(grants, grant)
			total += grant
		}
		drainRedisContractCompensations(t, ctx)
		if !errors.Is(refusal, errRedisReservationInsufficient) {
			t.Fatalf("no refusal after %d grants: %v", len(grants), refusal)
		}
		for index := 1; index < len(grants); index++ {
			if grants[index-1] < grants[index] {
				t.Fatalf("grant %d grew: %v", index, grants)
			}
		}
		if grants[0] != room/8 || grants[len(grants)-1] != testingRelayedFloor {
			t.Fatalf("grants did not shrink from an eighth to the floor: %v", grants)
		}
		if room < total || room-total >= testingRelayedFloor {
			t.Fatalf("grants of %d stopped short of or past the balance of %d", total, room)
		}
		connect.AssertEqual(t, network.held(ctx), []ByteCount{total})
		state, err := GetNetworkQuotaState(ctx, network.networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, state != nil && state.SpentTime != nil, true)
	})
}

// A network with nothing available is refused with nothing reserved and marked spent; a balance
// whose window has not begun is not drawn.
func TestRelayedSpentRefusalReservesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx)
		now := server.NowUtc()
		future := testingInsertEscrowDrawBalance(t, ctx, network.networkId, 1*Gib, 0, nil, now.Add(time.Hour), now.Add(2*time.Hour))
		_, _, err := network.reserve(ctx, 64*Mib)
		drainRedisContractCompensations(t, ctx)
		if !errors.Is(err, errRedisReservationInsufficient) {
			t.Fatalf("%v, want a refusal", err)
		}
		connect.AssertEqual(t, Testing_NetEscrowByteCount(ctx, future), ByteCount(0))
		state, err := GetNetworkQuotaState(ctx, network.networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, state != nil && state.SpentTime != nil, true)
	})
}

// A pair that rides a stream reserves nothing: P2P bytes never count.
func TestRelayedStreamBoundPairReservesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		bound := WithPairStreamBinding(ctx, network.sourceId, network.destinationId, true)
		contractId, grant, err := network.reserve(bound, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, grant, 64*Mib)
		connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
		var payerNetworkId *server.Id
		var streamId *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id, stream_id FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&payerNetworkId, &streamId))
		})
		connect.AssertEqual(t, payerNetworkId == nil, true)
		connect.AssertEqual(t, streamId != nil, true)
	})
}

// A reservation whose contract insert fails is released by the request's compensation.
func TestRelayedFailedInsertReleases(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, network.destinationId))
		})
		_, _, err := network.reserve(ctx, 64*Mib)
		drainRedisContractCompensations(t, ctx)
		if err == nil || errors.Is(err, errRedisReservationInsufficient) {
			t.Fatalf("%v, want the insert's refusal", err)
		}
		connect.AssertEqual(t, network.held(ctx), []ByteCount{0})
	})
}

// A warm network's relayed contract reads no balance from Postgres and runs one reservation script
// for a one-balance network (20.6's cost row); a network with several balances takes the rest of a
// grant the first cannot cover from the next, in draw order.
func TestRelayedWarmPathCost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingRelayedSettings(t)
		network := newTestingRelayedNetwork(t, ctx, 1*Gib)
		share := redisContractReservationResults.WithLabelValues("reserve-pending-share", "accepted")
		plain := redisContractReservationResults.WithLabelValues("reserve-pending", "accepted")
		if _, _, err := network.reserve(ctx, 64*Mib); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, Testing_RelayedDrawOrderCacheLoads(), 1)
		shareBefore, plainBefore := testutil.ToFloat64(share), testutil.ToFloat64(plain)
		if _, _, err := network.reserve(ctx, 64*Mib); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, Testing_RelayedDrawOrderCacheLoads(), 1)
		connect.AssertEqual(t, testutil.ToFloat64(share)-shareBefore, 1.0)
		connect.AssertEqual(t, testutil.ToFloat64(plain)-plainBefore, 0.0)

		// a first balance with 2 MiB and a second with plenty: the share aims at an eighth of
		// both, the first gives what it has and the second the rest
		several := newTestingRelayedNetwork(t, ctx, 2*Mib, 1*Gib)
		_, grant, err := several.reserve(ctx, 64*Mib)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, grant, 64*Mib)
		connect.AssertEqual(t, several.held(ctx), []ByteCount{2 * Mib, 62 * Mib})
	})
}

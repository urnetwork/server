//go:build sidecar_pending

package quotahost

// FS-45 (§20.6's relayed bytes, FI-13 (a); QUOTA-M2-13) on T3: model's relayed reservation and settlement
// (QUOTA-M2-08, QUOTA-M2-09) on the private stack's Postgres and Redis, with FI-13's oracle reading
// transfer_balance and service_debit_journal after every step. A network near empty with N relayed pairs
// gets grants that shrink and stop at the balance, and spending stops there; the refusal marks the network
// spent and the next contract is refused; a pair bound to a stream is granted its request with nothing
// reserved; a contract's close journals one debit under its contract id, however its reports and its deadline
// are replayed; and with Redis lost the grants then outstanding are forgotten, which bounds what the next
// grants over-admit, each contract still debiting once.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	fs45T3Request = 64 * model.Mib
	fs45T3Floor   = 1 * model.Mib
)

// One network with a relayed pair of clients and one balance.
type fs45T3Network struct {
	ctx           context.Context
	oracle        *sidecartest.QuotaOracle
	networkId     server.Id
	sourceId      server.Id
	destinationId server.Id
	balanceId     server.Id
	room          model.ByteCount
}

func newFs45T3Network(ctx context.Context, room model.ByteCount) *fs45T3Network {
	network := &fs45T3Network{
		ctx:           ctx,
		oracle:        newT3Oracle(ctx),
		networkId:     server.NewId(),
		sourceId:      server.NewId(),
		destinationId: server.NewId(),
		balanceId:     server.NewId(),
		room:          room,
	}
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, clientId := range []server.Id{network.sourceId, network.destinationId} {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client (client_id, network_id, active, create_time, auth_time)
					VALUES ($1, $2, true, $3, $3)
				`,
				clientId,
				network.networkId,
				now,
			))
		}
	})
	t3SetBalances(ctx, now, network.networkId, []*sidecartest.QuotaBalance{{BalanceId: connect.Id(network.balanceId), ByteCount: int64(room)}})
	network.oracle.Grant(connect.Id(network.networkId), int64(room))
	return network
}

func (self *fs45T3Network) reserve(ctx context.Context) (server.Id, model.ByteCount, error) {
	contractId, grant, _, err := model.ReserveRelayedContract(ctx, self.networkId, self.sourceId, self.destinationId, fs45T3Request, fs45T3Floor, true)
	return contractId, grant, err
}

// Contracts until one is refused: their ids and grants.
func (self *fs45T3Network) reserveUntilRefused(t *testing.T) (contractIds []server.Id, grants []model.ByteCount) {
	t.Helper()
	for range 400 {
		contractId, grant, err := self.reserve(self.ctx)
		if err != nil {
			return
		}
		contractIds = append(contractIds, contractId)
		grants = append(grants, grant)
		self.check(t)
	}
	t.Fatal("no refusal after 400 grants")
	return
}

// Both sides report used and the contract closes; the oracle is told what moved.
func (self *fs45T3Network) closeBoth(t *testing.T, contractId server.Id, used model.ByteCount) {
	t.Helper()
	self.oracle.Meter(connect.Id(self.networkId), sidecar.QuotaMeterRelay, int64(used))
	if err := model.CloseContract(self.ctx, contractId, self.sourceId, used, false); err != nil {
		t.Fatal(err)
	}
	if err := model.CloseContract(self.ctx, contractId, self.destinationId, used, false); err != nil {
		t.Fatal(err)
	}
	self.check(t)
}

func (self *fs45T3Network) check(t *testing.T) {
	t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		t.Fatal(err)
	}
}

// The debit flush, then FI-13 (a) whole.
func (self *fs45T3Network) checkSettled(t *testing.T) {
	t.Helper()
	if err := t3FlushServiceDebits(self.ctx, sidecartest.NewFakeClock(server.NowUtc())); err != nil {
		t.Fatal(err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		t.Fatal(err)
	}
}

func (self *fs45T3Network) balance() *sidecartest.QuotaBalance {
	return t3ReadBalances(self.ctx, self.networkId)[0]
}

func (self *fs45T3Network) spent(t *testing.T) bool {
	t.Helper()
	state, err := model.GetNetworkQuotaState(self.ctx, self.networkId)
	if err != nil {
		t.Fatal(err)
	}
	return state != nil && state.SpentTime != nil
}

// The debits journaled under the contract's id: how many rows, and their bytes.
func (self *fs45T3Network) journaled(contractId server.Id) (rowCount int, byteCount int64) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			self.ctx,
			`SELECT COUNT(*), COALESCE(SUM(debit_byte_count), 0) FROM service_debit_journal WHERE usage_id = $1`,
			contractId,
		).Scan(&rowCount, &byteCount))
	})
	return
}

func fs45T3GrantsShrinkAndStopAtTheBalance(t *testing.T, ctx context.Context) {
	room := 100 * model.Mib
	network := newFs45T3Network(ctx, room)
	contractIds, grants := network.reserveUntilRefused(t)
	total := model.ByteCount(0)
	for index, grant := range grants {
		total += grant
		if 0 < index && grants[index-1] < grant {
			t.Fatalf("grant %d grew: %v", index, grants)
		}
	}
	if len(grants) == 0 || grants[0] != room/8 || grants[len(grants)-1] != fs45T3Floor {
		t.Fatalf("grants did not shrink from an eighth to the floor: %v", grants)
	}
	if room < total || fs45T3Floor <= room-total {
		t.Fatalf("grants of %d stopped short of or past the balance of %d", total, room)
	}
	if !network.spent(t) {
		t.Fatal("the refusal left the network unmarked")
	}
	if got := network.balance().ReservedByteCount; got != int64(total) {
		t.Fatalf("%d reserved for %d granted", got, total)
	}
	// spent refuses the next contract
	if _, _, err := network.reserve(ctx); err == nil {
		t.Fatal("a spent network was granted a contract")
	}

	// a P2P-bound pair is unaffected, on the spent network: its request granted, nothing reserved, no payer
	bound := model.WithPairStreamBinding(ctx, network.sourceId, network.destinationId, true)
	boundContractId, grant, err := network.reserve(bound)
	if err != nil || grant != fs45T3Request {
		t.Fatalf("the stream-bound pair: %d, %v", grant, err)
	}
	if got := network.balance().ReservedByteCount; got != int64(total) {
		t.Fatalf("the stream-bound pair reserved: %d held", got)
	}
	var payerNetworkId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id FROM transfer_contract WHERE contract_id = $1`, boundContractId).Scan(&payerNetworkId))
	})
	if payerNetworkId != nil {
		t.Fatalf("the stream-bound pair has a payer: %s", payerNetworkId)
	}
	network.check(t)

	// every pair spends its whole grant: spending stops at the balance
	for index, contractId := range contractIds {
		network.closeBoth(t, contractId, grants[index])
	}
	network.checkSettled(t)
	if balance := network.balance(); balance.ByteCount != int64(room-total) || balance.ByteCount < 0 || balance.ReservedByteCount != 0 {
		t.Fatalf("after %d granted and spent of %d: %+v", total, room, balance)
	}
}

func fs45T3OneDebitPerContract(t *testing.T, ctx context.Context) {
	room := 1 * model.Gib
	network := newFs45T3Network(ctx, room)
	// both reports, the mean settled, the closes replayed
	contractId, grant, err := network.reserve(ctx)
	if err != nil || grant != fs45T3Request {
		t.Fatalf("grant %d, %v", grant, err)
	}
	network.oracle.Meter(connect.Id(network.networkId), sidecar.QuotaMeterRelay, 800)
	if err := model.CloseContract(ctx, contractId, network.sourceId, 1000, false); err != nil {
		t.Fatal(err)
	}
	network.check(t)
	if err := model.CloseContract(ctx, contractId, network.destinationId, 600, false); err != nil {
		t.Fatal(err)
	}
	network.check(t)
	for _, clientId := range []server.Id{network.sourceId, network.destinationId} {
		if err := model.CloseContract(ctx, contractId, clientId, 1000, false); err == nil {
			t.Fatal("a close replayed on a settled contract was taken")
		}
		network.check(t)
	}
	if rowCount, byteCount := network.journaled(contractId); rowCount != 1 || byteCount != 800 {
		t.Fatalf("the contract's journal: %d rows, %d bytes", rowCount, byteCount)
	}

	// one report, and the deadline closing it twice
	deadlineContractId, _, err := network.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	network.oracle.Meter(connect.Id(network.networkId), sidecar.QuotaMeterRelay, 3000)
	if err := model.CloseContract(ctx, deadlineContractId, network.sourceId, 3000, false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := model.ReconcileContractAtDeadline(ctx, deadlineContractId, server.NowUtc().Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		network.check(t)
	}
	if rowCount, byteCount := network.journaled(deadlineContractId); rowCount != 1 || byteCount != 3000 {
		t.Fatalf("the deadline contract's journal: %d rows, %d bytes", rowCount, byteCount)
	}
	network.checkSettled(t)
	// a second flush finds nothing more to take
	network.checkSettled(t)
	if balance := network.balance(); balance.ByteCount != int64(room)-3800 || balance.ReservedByteCount != 0 {
		t.Fatalf("after both contracts: %+v", balance)
	}
}

func fs45T3RedisLoss(t *testing.T, ctx context.Context) {
	room := 100 * model.Mib
	network := newFs45T3Network(ctx, room)
	contractIds, grants := network.reserveUntilRefused(t)
	forgotten := network.balance().ReservedByteCount
	if forgotten == 0 {
		t.Fatal("nothing reserved before the loss")
	}
	// Redis fails over empty: every reservation is forgotten, and the next contracts are judged afresh
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.FlushDB(ctx).Err())
	})
	defer model.Testing_SetRelayedReservationSettings(model.DefaultRelayedReservationSettings())()
	laterContractIds, laterGrants := network.reserveUntilRefused(t)
	later := int64(0)
	for _, grant := range laterGrants {
		later += int64(grant)
	}
	if int64(room) < later {
		t.Fatalf("after the loss %d was granted of a balance of %d", later, room)
	}
	contractIds = append(contractIds, laterContractIds...)
	grants = append(grants, laterGrants...)
	for index, contractId := range contractIds {
		network.closeBoth(t, contractId, grants[index])
		if rowCount, _ := network.journaled(contractId); 1 < rowCount {
			t.Fatalf("contract %s journaled %d debits", contractId, rowCount)
		}
	}
	if err := t3FlushServiceDebits(ctx, sidecartest.NewFakeClock(server.NowUtc())); err != nil {
		t.Fatal(err)
	}
	network.check(t)
	// what was over-admitted is at most what the loss forgot
	if balance := network.balance(); balance.ByteCount < -forgotten {
		t.Fatalf("%d left: the loss forgot %d, which bounds the over-admission", balance.ByteCount, forgotten)
	}
}

// FS-45 on Postgres and Redis.
func TestFs45T3RelayedPairs(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetRelayedReservationSettings(model.DefaultRelayedReservationSettings())()
		ctx := t.Context()
		fs45T3GrantsShrinkAndStopAtTheBalance(t, ctx)
		fs45T3OneDebitPerContract(t, ctx)
		fs45T3RedisLoss(t, ctx)
	})
}

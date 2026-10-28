// The payer drain retires one payer's due contracts once each, through owner
// turns, and leaves everything else; its request passes one per payer per
// interval, and only a final balance refusal makes one; the balance summary
// reports the Pending no current grant reserves.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Every synthetic contract reserves this escrow and reports this final usage.
const payerDueCloseEscrowBytes = ByteCount(100)
const payerDueCloseUsedBytes = ByteCount(10)
const payerDueCloseInitialBytes = ByteCount(1_000_000)

// A provider endpoint and one payer with a grant that ended and a current one.
type payerDueCloseFixture struct {
	providerNetworkId server.Id
	providerClientId  server.Id
	payerNetworkId    server.Id
	payerClientId     server.Id
	endedBalanceId    server.Id
	currentBalanceId  server.Id
}

func newPayerDueCloseFixture(t testing.TB, ctx context.Context, name string) *payerDueCloseFixture {
	t.Helper()
	f := &payerDueCloseFixture{
		providerNetworkId: server.NewId(), providerClientId: server.NewId(),
		payerNetworkId: server.NewId(), payerClientId: server.NewId(),
	}
	Testing_CreateNetwork(ctx, f.providerNetworkId, name+"-provider", server.NewId())
	Testing_CreateDevice(ctx, f.providerNetworkId, server.NewId(), f.providerClientId, "synthetic-provider", "synthetic")
	Testing_CreateNetwork(ctx, f.payerNetworkId, name+"-payer", server.NewId())
	Testing_CreateDevice(ctx, f.payerNetworkId, server.NewId(), f.payerClientId, "synthetic-payer", "synthetic")
	now := server.NowUtc()
	server.Raise(AddBasicTransferBalance(ctx, f.payerNetworkId, payerDueCloseInitialBytes, now.Add(-48*time.Hour), now.Add(-24*time.Hour)))
	server.Raise(AddBasicTransferBalance(ctx, f.payerNetworkId, payerDueCloseInitialBytes, now.Add(-time.Hour), now.Add(24*time.Hour)))
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT balance_id FROM transfer_balance WHERE network_id=$1 AND end_time<$2),
			(SELECT balance_id FROM transfer_balance WHERE network_id=$1 AND end_time>$2)`,
			f.payerNetworkId, now).Scan(&f.endedBalanceId, &f.currentBalanceId))
	})
	return f
}

// Contracts minted at mintedAt, a millisecond apart, with the deadline,
// reserving the fixture escrow on balanceId (none for a zero id) and carrying
// bilateral final reports of the fixture usage. disputed contracts are
// reported in dispute.
func (self *payerDueCloseFixture) addContracts(ctx context.Context, balanceId server.Id, mintedAt time.Time, count int,
	deadline time.Time, disputed bool) []server.Id {
	ids := make([]server.Id, count)
	for index := range ids {
		id := ContractIdTimeBound(mintedAt.Add(time.Duration(index) * time.Millisecond))
		entropy := server.NewId()
		copy(id[6:], entropy[6:])
		ids[index] = id
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id,usage_origin_is_source,create_time,expiration_time,dispute)
			SELECT contract_id,$2,$3,$4,$5,$6,$2,true,$7,$8,$9 FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, self.payerNetworkId, self.payerClientId, self.providerNetworkId, self.providerClientId,
			payerDueCloseEscrowBytes, mintedAt, deadline, disputed))
		if balanceId != (server.Id{}) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
				SELECT contract_id,$2,$3 FROM unnest($1::uuid[]) AS candidate(contract_id)`, ids, balanceId, payerDueCloseEscrowBytes))
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT contract_id,party,$2,$3,false FROM unnest($1::uuid[]) AS candidate(contract_id)
			CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, ids, payerDueCloseUsedBytes, mintedAt.Add(time.Minute)))
	})
	return ids
}

// Fails unless exactly the listed contracts of the fixture settled, each once
// at its stored deadline with the fixture usage paid from its escrow, and each
// grant was debited once per settled contract it reserved for.
func (self *payerDueCloseFixture) requireSettled(t testing.TB, ctx context.Context, settled []server.Id, open []server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var closed, paid, atDeadline, stillOpen, swept int
		var endedBalance, currentBalance ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled' AND NOT dispute),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=$3),
			(SELECT count(*) FROM transfer_escrow e JOIN transfer_contract c USING (contract_id) WHERE e.contract_id=ANY($1) AND e.settle_time=c.expiration_time),
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($2) AND outcome IS NULL),
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$4),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$5)`,
			settled, open, payerDueCloseUsedBytes, self.endedBalanceId, self.currentBalanceId).Scan(
			&closed, &paid, &atDeadline, &stillOpen, &swept, &endedBalance, &currentBalance))
		var endedSettled, currentSettled int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND balance_id=$2 AND settled),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND balance_id=$3 AND settled)`,
			settled, self.endedBalanceId, self.currentBalanceId).Scan(&endedSettled, &currentSettled))
		if closed != len(settled) || atDeadline != paid || stillOpen != len(open) || swept != paid ||
			endedBalance != payerDueCloseInitialBytes-ByteCount(endedSettled)*payerDueCloseUsedBytes ||
			currentBalance != payerDueCloseInitialBytes-ByteCount(currentSettled)*payerDueCloseUsedBytes {
			t.Fatalf("payer drain accounting is not exact: closed=%d/%d paid=%d at_deadline=%d swept=%d open=%d/%d ended=%d current=%d",
				closed, len(settled), paid, atDeadline, swept, stillOpen, len(open), endedBalance, currentBalance)
		}
	})
}

// Counts admitted and refused owner turns per balance.
type payerDueCloseOwnerEvents struct {
	stateLock sync.Mutex
	admitted  map[server.Id]int
	refused   map[server.Id]int
	balances  map[server.PgOwnershipKey]server.Id
}

func newPayerDueCloseOwnerEvents(balanceIds ...server.Id) *payerDueCloseOwnerEvents {
	events := &payerDueCloseOwnerEvents{admitted: map[server.Id]int{}, refused: map[server.Id]int{}, balances: map[server.PgOwnershipKey]server.Id{}}
	for _, balanceId := range balanceIds {
		events.balances[server.NewPgOwnershipKey("transfer_balance", balanceId)] = balanceId
	}
	return events
}

func (self *payerDueCloseOwnerEvents) observe(event server.PgOwnershipEvent) {
	if event.TransactionScoped {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, key := range event.Keys {
		if balanceId, ok := self.balances[key]; ok {
			switch event.Kind {
			case server.PgOwnershipAdmitted:
				self.admitted[balanceId]++
			case server.PgOwnershipRefused:
				self.refused[balanceId]++
			}
		}
	}
}

func (self *payerDueCloseOwnerEvents) counts(balanceId server.Id) (admitted, refused int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.admitted[balanceId], self.refused[balanceId]
}

func unlimitedPayerDueClose() bool { return true }

// Pending beyond what the current grants reserve is stranded: the reservations
// of a grant that ended are not listed, and a superseded grant's leave Pending
// with their grant. Reservations the open total cannot cover strand nothing.
func TestSummarizeTransferBalancesReportsPendingNoCurrentGrantReserves(t *testing.T) {
	overlap := testGrantBoundary.Add(30 * time.Minute)
	yesterday := testingFreeGrant(GrantKindFree, testGrantBoundary.Add(-time.Minute))
	yesterday.reservedByteCount = 1 * Gib
	today := testingFreeGrant(GrantKindFree, overlap)
	today.reservedByteCount = 2 * Gib

	// 7 GiB of Pending belongs to contracts on grants that ended
	summary := SummarizeTransferBalances([]*TransferBalance{yesterday, today}, 10*Gib, overlap)
	connect.AssertEqual(t, summary.OpenTransferByteCount, 9*Gib)
	connect.AssertEqual(t, summary.StrandedTransferByteCount, 7*Gib)

	// every open contract reserves from a current grant
	summary = SummarizeTransferBalances([]*TransferBalance{yesterday, today}, 3*Gib, overlap)
	connect.AssertEqual(t, summary.OpenTransferByteCount, 2*Gib)
	connect.AssertEqual(t, summary.StrandedTransferByteCount, ByteCount(0))

	// counters that drifted past the open total strand nothing
	summary = SummarizeTransferBalances([]*TransferBalance{yesterday, today}, 2*Gib, overlap)
	connect.AssertEqual(t, summary.StrandedTransferByteCount, ByteCount(0))

	// outside the hour yesterday's grant has ended and lists no reservation
	later := testGrantBoundary.Add(5 * time.Hour)
	summary = SummarizeTransferBalances([]*TransferBalance{today}, 10*Gib, later)
	connect.AssertEqual(t, summary.OpenTransferByteCount, 10*Gib)
	connect.AssertEqual(t, summary.StrandedTransferByteCount, 8*Gib)
}

// One drain retires the payer's due contracts on a grant that ended and on its
// current grant, oldest first, each owner's members in turns of at most one
// owner batch, and a due contract without escrow alone. A contract not yet
// due, a disputed one and another payer's stay open. A second drain finds
// nothing and changes nothing, so every contract settles and debits once.
func TestClosePayerDueContractsRetiresDueContractsOnceThroughOwnerTurns(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newPayerDueCloseFixture(t, t.Context(), "synthetic-payer-drain")
		other := newPayerDueCloseFixture(t, t.Context(), "synthetic-payer-drain-other")
		now := server.NowUtc().Truncate(time.Millisecond)
		mintedAt := now.Add(-30 * time.Hour)
		deadline := mintedAt.Add(DefaultContractExpiration)
		const endedCount = ContractDeadlineOwnerBatchLimit + 6
		ended := f.addContracts(t.Context(), f.endedBalanceId, mintedAt, endedCount, deadline, false)
		currentMintedAt := now.Add(-2 * time.Hour)
		currentDeadline := currentMintedAt.Add(DefaultContractExpiration)
		current := f.addContracts(t.Context(), f.currentBalanceId, currentMintedAt, 3, currentDeadline, false)
		unescrowed := f.addContracts(t.Context(), server.Id{}, currentMintedAt.Add(time.Second), 1, currentDeadline.Add(time.Second), false)
		live := f.addContracts(t.Context(), f.currentBalanceId, now.Add(-10*time.Minute), 2, now.Add(50*time.Minute), false)
		disputed := f.addContracts(t.Context(), f.currentBalanceId, currentMintedAt.Add(2*time.Second), 1, currentDeadline.Add(2*time.Second), true)
		otherDue := other.addContracts(t.Context(), other.endedBalanceId, mintedAt, 2, deadline, false)
		events := newPayerDueCloseOwnerEvents(f.endedBalanceId, f.currentBalanceId)
		ctx := server.Testing_WithPgOwnershipObservation(WithProviderWorkSessionSource(t.Context(), nil), events.observe)

		due, err := ReadPayerDueContracts(ctx, f.payerNetworkId, now, 1024)
		if err != nil {
			t.Fatal(err)
		}
		dueIds := []server.Id{}
		for _, deadline := range due {
			dueIds = append(dueIds, deadline.ContractId)
		}
		wantIds := append(append(append([]server.Id{}, ended...), current...), unescrowed...)
		slices.SortFunc(wantIds, server.Id.Cmp)
		if !slices.Equal(dueIds, wantIds) {
			t.Fatalf("due read returned %d contracts, want the %d due ones oldest first", len(dueIds), len(wantIds))
		}

		result, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now, unlimitedPayerDueClose, DefaultPayerDueContractCloseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if result.Due != len(wantIds) || result.Closed != len(wantIds) || result.AlreadyClosed != 0 || result.Busy != 0 || result.Failed != 0 || result.More {
			t.Fatalf("drain did not close exactly the due contracts: %+v", result)
		}
		f.requireSettled(t, ctx, wantIds, append(append([]server.Id{}, live...), disputed...))
		if admitted, refused := events.counts(f.endedBalanceId); admitted != 2 || refused != 0 {
			t.Fatal("the ended grant's members did not close in two owner turns", admitted, refused)
		}
		if admitted, refused := events.counts(f.currentBalanceId); admitted != 1 || refused != 0 {
			t.Fatal("the current grant's members did not close in one owner turn", admitted, refused)
		}
		other.requireSettled(t, ctx, nil, otherDue)

		again, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now.Add(time.Minute), unlimitedPayerDueClose, DefaultPayerDueContractCloseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if again.Due != 0 || again.Closed != 0 || again.More {
			t.Fatalf("a second drain found work: %+v", again)
		}
		f.requireSettled(t, ctx, wantIds, append(append([]server.Id{}, live...), disputed...))
	})
}

// A page of the drain costs about a page of the payer's open-contract index,
// however many due contracts the payer holds and wherever they fall in
// contract order. Ordered by contract_id, the read sorted every due contract
// of the payer, or walked the primary key from the oldest contract of every
// payer until it met a page of the payer's. On Main, with payers holding
// millions of unresolved contracts, the planner chose the walk, and the read
// outlived its 20 s bound on every run (2026-10-28). Sequential scans are off
// as they are in effect on Main's billions of contracts; this table is small.
func TestReadPayerDueContractsExaminesAboutOnePage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newPayerDueCloseFixture(t, t.Context(), "synthetic-payer-due-page")
		other := newPayerDueCloseFixture(t, t.Context(), "synthetic-payer-due-page-other")
		now := server.NowUtc().Truncate(time.Millisecond)
		const page = 32
		// The other payer's contracts are older, so they come first in contract order.
		otherMintedAt := now.Add(-40 * time.Hour)
		other.addContracts(t.Context(), other.endedBalanceId, otherMintedAt, 40*page, otherMintedAt.Add(DefaultContractExpiration), false)
		mintedAt := now.Add(-30 * time.Hour)
		deadline := mintedAt.Add(DefaultContractExpiration)
		due := f.addContracts(t.Context(), f.endedBalanceId, mintedAt, 4*page, deadline, false)

		if examined := payerDueReadExaminedRows(t, t.Context(), f.payerNetworkId, now, page); 2*page < examined {
			t.Fatalf("the due read examined %.0f contracts for a page of %d", examined, page)
		}

		read, err := ReadPayerDueContracts(t.Context(), f.payerNetworkId, now, page)
		if err != nil {
			t.Fatal(err)
		}
		readIds := []server.Id{}
		for _, contract := range read {
			if !slices.Contains(due, contract.ContractId) || !contract.Deadline.Equal(deadline) {
				t.Fatal("the due read returned a contract that is not one of the payer's due ones, or its deadline")
			}
			readIds = append(readIds, contract.ContractId)
		}
		if len(readIds) != page || !slices.IsSortedFunc(readIds, server.Id.Cmp) {
			t.Fatalf("the due read returned %d contracts, want a page of %d in contract order", len(readIds), page)
		}
	})
}

// The transfer_contract rows the due read's scans examined: those they
// returned and those their filters or rechecks removed, over every loop.
func payerDueReadExaminedRows(t testing.TB, ctx context.Context, payerNetworkId server.Id, dueBefore time.Time, limit int) float64 {
	t.Helper()
	type planNode struct {
		Relation  string     `json:"Relation Name"`
		Rows      float64    `json:"Actual Rows"`
		Loops     float64    `json:"Actual Loops"`
		Removed   float64    `json:"Rows Removed by Filter"`
		Rechecked float64    `json:"Rows Removed by Index Recheck"`
		Plans     []planNode `json:"Plans"`
	}
	var examined float64
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+payerDueContractsSql,
			payerNetworkId, dueBefore.UTC(), dueBefore.UTC().Add(-DefaultContractExpiration), limit).Scan(&raw))
		var plans []struct {
			Plan planNode
		}
		if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
			t.Fatal("invalid query plan")
		}
		var inspect func(planNode)
		inspect = func(node planNode) {
			if node.Relation == "transfer_contract" {
				examined += (node.Rows + node.Removed + node.Rechecked) * node.Loops
			}
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
	})
	return examined
}

// A held owner refuses its turn without waiting: its members stay open for
// the next run, which the result asks for, while another owner's close. Once
// the holder releases, the next run closes them. A budget that ends after one
// turn leaves the rest for the next run too.
func TestClosePayerDueContractsLeavesAHeldOwnerAndTheBudgetsRestForTheNextRun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newPayerDueCloseFixture(t, t.Context(), "synthetic-payer-drain-held")
		now := server.NowUtc().Truncate(time.Millisecond)
		mintedAt := now.Add(-30 * time.Hour)
		deadline := mintedAt.Add(DefaultContractExpiration)
		ended := f.addContracts(t.Context(), f.endedBalanceId, mintedAt, 3, deadline, false)
		currentMintedAt := now.Add(-2 * time.Hour)
		current := f.addContracts(t.Context(), f.currentBalanceId, currentMintedAt, 2, currentMintedAt.Add(DefaultContractExpiration), false)
		ctx := WithProviderWorkSessionSource(t.Context(), nil)

		admittedHolder, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(holderDone)
			server.OwnedTx(t.Context(), []server.PgOwnershipKey{server.NewPgOwnershipKey("transfer_balance", f.endedBalanceId)}, func(tx server.PgTx) {
				close(admittedHolder)
				<-release
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		<-admittedHolder
		held, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now, unlimitedPayerDueClose, DefaultPayerDueContractCloseSettings())
		close(release)
		<-holderDone
		if err != nil {
			t.Fatal(err)
		}
		if held.Closed != len(current) || held.Busy != len(ended) || !held.More {
			t.Fatalf("held owner's members were not kept for the next run: %+v", held)
		}
		f.requireSettled(t, ctx, current, ended)

		next, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now, unlimitedPayerDueClose, DefaultPayerDueContractCloseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if next.Closed != len(ended) || next.Busy != 0 || next.More {
			t.Fatalf("released owner's members did not close: %+v", next)
		}
		f.requireSettled(t, ctx, append(append([]server.Id{}, ended...), current...), nil)

		// a budget for one turn
		later := f.addContracts(t.Context(), f.endedBalanceId, mintedAt.Add(time.Hour), 2, deadline.Add(time.Hour), false)
		latest := f.addContracts(t.Context(), f.currentBalanceId, currentMintedAt.Add(time.Minute), 2, currentMintedAt.Add(DefaultContractExpiration+time.Minute), false)
		turns := 1
		cut, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now, func() bool {
			turns--
			return 0 <= turns
		}, DefaultPayerDueContractCloseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if cut.Closed != len(later) || !cut.More {
			t.Fatalf("a run whose budget ended after one turn did not ask for the next: %+v", cut)
		}
		rest, err := ClosePayerDueContracts(ctx, f.payerNetworkId, now, unlimitedPayerDueClose, DefaultPayerDueContractCloseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if rest.Closed != len(latest) || rest.More {
			t.Fatalf("the next run did not close what the budget left: %+v", rest)
		}
	})
}

// The gate passes one request per payer per interval: the first publishes the
// payer's drain, a repeat within the interval publishes nothing, and once the
// interval ends the next request merges into the pending drain. A request
// without a payer is refused.
func TestRequestPayerDueContractClosePassesOneRequestPerInterval(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		payerNetworkId := server.NewId()
		settings := DefaultPayerDueContractCloseSettings()
		runOnceKey := payerDueContractCloseRunOnce(payerNetworkId).String()
		pending := func() (count int, generation int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(max(run_once_generation),0) FROM pending_task WHERE run_once_key=$1 AND function_name=$2`,
					runOnceKey, NewClosePayerDueContractsTaskTarget().TargetFunctionName()).Scan(&count, &generation))
			})
			return
		}

		if !requestPayerDueContractClose(ctx, payerNetworkId, settings) {
			t.Fatal("the first request did not pass the gate")
		}
		count, generation := pending()
		if count != 1 {
			t.Fatal("the first request did not publish one drain", count)
		}
		if requestPayerDueContractClose(ctx, payerNetworkId, settings) {
			t.Fatal("a repeat within the interval passed the gate")
		}
		if again, againGeneration := pending(); again != 1 || againGeneration != generation {
			t.Fatal("a held request published", again, againGeneration)
		}

		// the interval ends
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, payerDueContractCloseGateKey(payerNetworkId)).Err())
		})
		if !requestPayerDueContractClose(ctx, payerNetworkId, settings) {
			t.Fatal("a request after the interval did not pass the gate")
		}
		if merged, mergedGeneration := pending(); merged != 1 || mergedGeneration <= generation {
			t.Fatal("a request after the interval did not merge into the pending drain", merged, mergedGeneration)
		}
		if requestPayerDueContractClose(ctx, server.Id{}, settings) {
			t.Fatal("a request without a payer passed")
		}
	})
}

// A refusal the admission recovers from asks for nothing: recovery releases an
// abandoned request's credit and the rerun admits the contract. A refusal that
// stands after the rerun asks for the payer's drain. A request at the refused
// grant selection asked for both, and cost the recovered admission one more
// database connection.
func TestOnlyAFinalBalanceRefusalRequestsThePayersDrain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		createRedisAdmissionTest(ctx, f, 17)
		redisRecoveryAbandonedTestRequest(t, ctx, f, 983)
		drains := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=$1`,
					payerDueContractCloseRunOnce(f.sourceNetworkId).String()).Scan(&count))
			})
			return
		}

		// the grant's 1000 bytes stay held until recovery releases the abandoned 983
		if escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23); err != nil || escrow == nil {
			t.Fatal("the recovered request was not admitted", err)
		}
		if count := drains(); count != 0 {
			t.Fatal("a refusal the admission recovered from requested the payer's drain", count)
		}
		// 960 bytes remain, below the request and its shrink floor
		if escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, Mib); escrow != nil || !errors.Is(err, ErrContractBalanceExhausted) {
			t.Fatal("the over-balance request was not refused for its balance", escrow, err)
		}
		if count := drains(); count != 1 {
			t.Fatal("a final refusal did not request the payer's drain", count)
		}
	})
}

// A run that left due contracts publishes its successor: at once after
// progress, after the busy delay when only held owners kept them. A run that
// finished publishes nothing.
func TestClosePayerDueContractsTaskPostSchedulesItsSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultPayerDueContractCloseSettings()
		successor := func(result *PayerDueContractClose) (scheduled bool, delay time.Duration) {
			payerNetworkId := server.NewId()
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(ClosePayerDueContractsTaskPost(&ClosePayerDueContractsArgs{PayerNetworkId: payerNetworkId}, result, owner, tx))
			})
			server.Db(ctx, func(conn server.PgConn) {
				var runAt *time.Time
				err := conn.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key=$1`,
					payerDueContractCloseRunOnce(payerNetworkId).String()).Scan(&runAt)
				if err == nil && runAt != nil {
					scheduled, delay = true, runAt.Sub(before)
				}
			})
			return
		}
		if scheduled, delay := successor(&PayerDueContractClose{Closed: 3, More: true}); !scheduled || settings.BusyRetryDelay/2 < delay {
			t.Fatal("progress did not schedule an immediate successor", scheduled, delay)
		}
		if scheduled, delay := successor(&PayerDueContractClose{Busy: 3, More: true}); !scheduled || delay < settings.BusyRetryDelay/2 {
			t.Fatal("held owners did not delay the successor", scheduled, delay)
		}
		if scheduled, _ := successor(&PayerDueContractClose{Closed: 3}); scheduled {
			t.Fatal("a finished drain scheduled a successor")
		}
	})
}

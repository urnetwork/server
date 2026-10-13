package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The sn-reconcile tests replace the vault's Circle configuration, the Circle
// client and the payout ledger with fakes; no Circle endpoint is ever called.
// Legacy rows are written directly, as the historical planner and sender left
// them.

const (
	snReconcileTestSolWallet   = "sn-reconcile-sol-wallet"
	snReconcileTestMaticWallet = "sn-reconcile-matic-wallet"
	snReconcileTestSolUsdc     = "sn-reconcile-sol-usdc"
	snReconcileTestMaticUsdc   = "sn-reconcile-matic-usdc"
)

var snReconcileTestCutoff = time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)

// Every legacy contract closes here unless a test says otherwise.
var snReconcileTestClosed = snReconcileTestCutoff.Add(-40 * 24 * time.Hour)

const snReconcileTestDay = 24 * time.Hour

type snReconcileTestLedger struct {
	transactions     map[string][]CircleTransaction
	balances         map[string]model.NanoCents
	developerWallets []CircleWallet
	listErr          error
	balanceErr       error
	developerErr     error
	listed           []string
	froms            []time.Time
	balanceReads     int
	developerReads   int
	beforeBalance    func()
}

func (self *snReconcileTestLedger) ListWalletTransactions(ctx context.Context, walletId string, from time.Time) ([]CircleTransaction, error) {
	self.listed = append(self.listed, walletId)
	self.froms = append(self.froms, from)
	if self.listErr != nil {
		return nil, self.listErr
	}
	return self.transactions[walletId], nil
}

func (self *snReconcileTestLedger) GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error) {
	self.balanceReads++
	if self.beforeBalance != nil {
		self.beforeBalance()
		self.beforeBalance = nil
	}
	if self.balanceErr != nil {
		return 0, self.balanceErr
	}
	return self.balances[walletId+"/"+tokenAddress], nil
}

func (self *snReconcileTestLedger) ListDeveloperWallets(ctx context.Context) ([]CircleWallet, error) {
	self.developerReads++
	if self.developerErr != nil {
		return nil, self.developerErr
	}
	return self.developerWallets, nil
}

type snReconcileTestPost struct {
	key         server.Id
	amount      float64
	destination string
	network     string
}

type snReconcileTestFixture struct {
	t             testing.TB
	ctx           context.Context
	session       *session.ClientSession
	sourceNetwork server.Id
	source        server.Id
	balance       server.Id
	clients       map[server.Id]server.Id
	ledger        *snReconcileTestLedger
	circle        *mockCircleApiClient
	records       map[string]CircleTransaction
	reads         []string
	posts         []snReconcileTestPost
}

func newSnReconcileTestFixture(t testing.TB) *snReconcileTestFixture {
	t.Helper()
	ctx := context.Background()
	sol, matic, solUsdc, maticUsdc := solanaWalletId, polygonWalletId, solanaUSDCAddress, polygonUSDCAddress
	solanaWalletId = func() string { return snReconcileTestSolWallet }
	polygonWalletId = func() string { return snReconcileTestMaticWallet }
	solanaUSDCAddress = func() string { return snReconcileTestSolUsdc }
	polygonUSDCAddress = func() string { return snReconcileTestMaticUsdc }
	t.Cleanup(func() {
		solanaWalletId, polygonWalletId, solanaUSDCAddress, polygonUSDCAddress = sol, matic, solUsdc, maticUsdc
	})
	f := &snReconcileTestFixture{
		t:       t,
		ctx:     ctx,
		session: session.NewLocalClientSession(ctx, "0.0.0.0:0", nil),
		clients: map[server.Id]server.Id{},
		ledger: &snReconcileTestLedger{transactions: map[string][]CircleTransaction{}, balances: map[string]model.NanoCents{
			snReconcileTestMaticWallet + "/" + snReconcileTestMaticUsdc: model.UsdToNanoCents(1000),
			snReconcileTestSolWallet + "/" + snReconcileTestSolUsdc:     model.UsdToNanoCents(1000),
		}},
		records: map[string]CircleTransaction{},
	}
	t.Cleanup(f.session.Cancel)
	previousLedger := NewCirclePayoutLedger()
	SetCirclePayoutLedger(f.ledger)
	t.Cleanup(func() { SetCirclePayoutLedger(previousLedger) })
	f.circle = controllerPayoutMock(t, f.post)
	f.circle.GetTransactionFunc = f.get
	f.sourceNetwork, f.source = server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, f.sourceNetwork, f.source)
	code, err := model.CreateBalanceCode(ctx, 1024*1024*1024, 365*24*time.Hour, model.UsdToNanoCents(1000), "sn-reconcile-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	testingRedeemPaymentBalanceCode(t, ctx, f.sourceNetwork, code.Secret)
	balances := model.GetActiveTransferBalances(ctx, f.sourceNetwork)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	f.balance = balances[0].BalanceId
	return f
}

func (self *snReconcileTestFixture) get(ctx context.Context, id string) (*GetTransactionResult, error) {
	self.reads = append(self.reads, id)
	transaction, ok := self.records[id]
	if !ok {
		return nil, errors.New("synthetic Circle transaction not found")
	}
	return &GetTransactionResult{Transaction: transaction, ResponseBodyBytes: []byte(`{}`)}, nil
}

// The sender's POST, after the same final boundary check the Core client runs.
// The accepted transfer is then readable and listed like a real one.
func (self *snReconcileTestFixture) post(ctx context.Context, key server.Id, amount float64, destination string, network string) (*CreateTransferTransactionResult, error) {
	if err := requireCircleProviderTransfer(ctx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: destination, Network: network}); err != nil {
		return nil, err
	}
	self.posts = append(self.posts, snReconcileTestPost{key: key, amount: amount, destination: destination, network: network})
	walletId, _ := getWalletIdByNetwork(network)
	transaction := CircleTransaction{Id: fmt.Sprintf("sn-reconcile-post-%d", len(self.posts)), State: "INITIATED", WalletId: walletId,
		DestinationAddress: destination, TransactionType: "OUTBOUND", CreateDate: snReconcileTestDate(server.NowUtc())}
	self.records[transaction.Id] = transaction
	self.list(walletId, transaction)
	return &CreateTransferTransactionResult{Id: transaction.Id, State: transaction.State}, nil
}

func (self *snReconcileTestFixture) exec(sql string, args ...any) {
	self.t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, sql, args...))
	})
}

func (self *snReconcileTestFixture) wallet(networkId server.Id, blockchain string, address string, active bool) server.Id {
	walletId := server.NewId()
	self.exec(`INSERT INTO account_wallet(wallet_id,network_id,wallet_type,blockchain,wallet_address,active,default_token_type)
		VALUES($1,$2,'external',$3,$4,$5,'USDC')`, walletId, networkId, blockchain, address, active)
	return walletId
}

// A provider network with one client and no wallet.
func (self *snReconcileTestFixture) bareNetwork() server.Id {
	networkId, clientId := server.NewId(), server.NewId()
	testingCreatePaymentClient(self.ctx, networkId, clientId)
	self.clients[networkId] = clientId
	return networkId
}

// A provider network whose active payout wallet is on `blockchain`, at an
// address unique to the network.
func (self *snReconcileTestFixture) network(blockchain string) (server.Id, server.Id, string) {
	networkId := self.bareNetwork()
	address := "0x" + strings.ReplaceAll(networkId.String(), "-", "")
	walletId := self.wallet(networkId, blockchain, address, true)
	self.exec(`INSERT INTO payout_wallet(network_id,wallet_id) VALUES($1,$2)`, networkId, walletId)
	return networkId, walletId, address
}

// A settled contract opened an hour before it closed at `closed`, whose sweep
// is not yet planned.
func (self *snReconcileTestFixture) contract(networkId server.Id, closed time.Time, revenue model.NanoCents) server.Id {
	contractId, clientId := server.NewId(), self.clients[networkId]
	usage := fmt.Sprintf(`{"version":1,"byte_count":100,"providers":[{"client_id":%q,"network_id":%q,"byte_count":100}]}`, clientId.String(), networkId.String())
	self.exec(`INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,100,$6,$7,'settled',$8,true)`, contractId, self.sourceNetwork, self.source, networkId, clientId, closed.Add(-time.Hour), closed, usage)
	self.exec(`INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,100,$5,$6)`, contractId, self.balance, networkId, clientId, revenue, closed)
	return contractId
}

// A planned payment owning the sweeps of `contracts`, `plus` above its census.
// The amount guard refuses a later payout change, so it is written here. Its
// plan epoch starts when the earliest contract opened, or when the payment
// was created without contracts, so none of its sweeps is early (A2).
func (self *snReconcileTestFixture) payment(networkId server.Id, walletId *server.Id, created time.Time, plus model.NanoCents, contracts ...server.Id) server.Id {
	paymentId := server.NewId()
	self.exec(`INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,min_sweep_time,create_time)
		SELECT $1,$2,$3,$4,COALESCE(SUM(payout_byte_count),0),COALESCE(SUM(payout_net_revenue_nano_cents),0)+$7,$5,$5
		FROM transfer_escrow_sweep WHERE contract_id=ANY($6)`, paymentId, server.NewId(), networkId, walletId, created, contracts, plus)
	self.exec(`UPDATE transfer_escrow_sweep SET payment_id=$1 WHERE contract_id=ANY($2)`, paymentId, contracts)
	self.exec(`INSERT INTO subsidy_payment(payment_plan_id,start_time,end_time,active_user_count,paid_user_count,
		net_payout_byte_count_paid,net_payout_byte_count_unpaid,net_revenue_nano_cents,net_payout_nano_cents)
		SELECT p.payment_plan_id,e.start_time,e.start_time+INTERVAL '1 microsecond',0,0,0,0,0,0 FROM account_payment p
		CROSS JOIN LATERAL (SELECT COALESCE((SELECT MIN(c.create_time) FROM transfer_contract c WHERE c.contract_id=ANY($2)),p.create_time) start_time) e
		WHERE p.payment_id=$1`, paymentId, contracts)
	return paymentId
}

// The payment's plan epoch starts at `start` instead: the planner clamps an
// epoch to the end of the one before it (A2).
func (self *snReconcileTestFixture) epoch(paymentId server.Id, start time.Time) {
	self.exec(`UPDATE subsidy_payment SET start_time=$2,end_time=$2::timestamp+INTERVAL '1 microsecond'
		WHERE payment_plan_id=(SELECT payment_plan_id FROM account_payment WHERE payment_id=$1)`, paymentId, start)
}

// A planned payment of `revenue` on one contract closed at `snReconcileTestClosed`.
func (self *snReconcileTestFixture) legacy(networkId server.Id, walletId *server.Id, created time.Time, revenue model.NanoCents) server.Id {
	return self.payment(networkId, walletId, created, 0, self.contract(networkId, snReconcileTestClosed, revenue))
}

// The planner re-planned the row's sweeps into `into`.
func (self *snReconcileTestFixture) moveSweeps(from server.Id, into server.Id) {
	self.exec(`UPDATE transfer_escrow_sweep SET payment_id=$2 WHERE payment_id=$1`, from, into)
}

// Moves a correction the way the planner carries it: the origin is safely
// canceled, then its bonus row moves to `target`. The origin keeps its amounts.
func (self *snReconcileTestFixture) moveBonus(origin server.Id, target server.Id, amount model.NanoCents) {
	self.exec(`INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
		SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'synthetic correction',$3,'' FROM account_payment WHERE payment_id=$4`,
		server.NewId(), amount, snReconcileTestClosed, origin)
	self.exec(`UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, origin, amount)
	self.exec(`UPDATE account_payment SET canceled=true,cancel_time=create_time WHERE payment_id=$1`, origin)
	self.exec(`UPDATE account_payment_bonus SET payment_id=$2 WHERE origin_payment_id=$1`, origin, target)
	self.exec(`UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, target, amount)
}

// RC-1 with only a key: the attempt may or may not have reached Circle.
func (self *snReconcileTestFixture) cancelWithKey(paymentId server.Id) {
	self.exec(`UPDATE account_payment SET canceled=true,cancel_time=create_time+INTERVAL '30 days',circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, server.NewId())
}

// RC-1 with a processor record that Circle reports in `state`.
func (self *snReconcileTestFixture) cancelWithRecord(paymentId server.Id, state string, txHash string) string {
	record := "record-" + paymentId.String()
	self.exec(`UPDATE account_payment SET canceled=true,cancel_time=create_time+INTERVAL '30 days',circle_idempotency_key=$2,payment_record=$3,token_type='USDC',token_amount=1
		WHERE payment_id=$1`, paymentId, server.NewId(), record)
	self.records[record] = CircleTransaction{Id: record, State: state, TxHash: txHash, WalletId: snReconcileTestMaticWallet}
	return record
}

// RC-2: a key reserved before request journaling, never journaled.
func (self *snReconcileTestFixture) reserveUnjournaled(paymentId server.Id) {
	self.exec(`UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, server.NewId())
}

func (self *snReconcileTestFixture) review(paymentIds ...server.Id) {
	self.exec(`UPDATE account_payment SET attribution_review_required=true WHERE payment_id=ANY($1)`, paymentIds)
}

// A completed payment whose record names a listed transfer.
func (self *snReconcileTestFixture) completedRecord(networkId server.Id, walletId *server.Id, record string, paymentTime *time.Time) server.Id {
	paymentId := self.payment(networkId, walletId, snReconcileTestClosed, 0)
	self.exec(`UPDATE account_payment SET completed=true,payment_record=$2,payment_time=$3 WHERE payment_id=$1`, paymentId, record, paymentTime)
	return paymentId
}

func (self *snReconcileTestFixture) list(walletId string, transactions ...CircleTransaction) {
	self.ledger.transactions[walletId] = append(self.ledger.transactions[walletId], transactions...)
}

// A transfer from the MATIC payout wallet.
func (self *snReconcileTestFixture) transfer(id string, state string, destination string, created time.Time) CircleTransaction {
	return CircleTransaction{Id: id, State: state, TransactionType: "OUTBOUND", Amounts: []string{"0.99"}, DestinationAddress: destination,
		CreateDate: snReconcileTestDate(created), WalletId: snReconcileTestMaticWallet}
}

func (self *snReconcileTestFixture) dry() *SnReconcileReport {
	self.t.Helper()
	report, err := SnReconcile(self.session, false, "")
	if err != nil {
		self.t.Fatal(err)
	}
	return report
}

func (self *snReconcileTestFixture) sendWith(digest string) *SnReconcileReport {
	self.t.Helper()
	report, err := SnReconcile(self.session, true, digest)
	if err != nil {
		self.t.Fatal(err)
	}
	return report
}

// A dry run, then --send with its digest (A7).
func (self *snReconcileTestFixture) send() *SnReconcileReport {
	self.t.Helper()
	return self.sendWith(self.dry().Digest)
}

// Requires exactly these stuck rows with these classes.
func (self *snReconcileTestFixture) expect(report *SnReconcileReport, want map[server.Id]string) {
	self.t.Helper()
	got := map[server.Id]string{}
	for _, item := range report.Rows {
		got[item.PaymentId] = snReconcileClassText(item.Class, item.Reason)
	}
	if !maps.Equal(got, want) {
		for paymentId, class := range want {
			if got[paymentId] != class {
				self.t.Errorf("row %s = %q, want %q", paymentId, got[paymentId], class)
			}
		}
		self.t.Fatalf("classes = %v, want %v", got, want)
	}
}

func snReconcileTestRow(t testing.TB, report *SnReconcileReport, paymentId server.Id) *SnReconcileRowReport {
	t.Helper()
	for _, item := range report.Rows {
		if item.PaymentId == paymentId {
			return item
		}
	}
	t.Fatal("row missing from report", paymentId)
	return nil
}

func snReconcileTestFunding(t testing.TB, report *SnReconcileReport, chain string) *SnReconcileFunding {
	t.Helper()
	for _, funding := range report.Funding {
		if funding.Chain == chain {
			return funding
		}
	}
	t.Fatal("chain missing from funding", chain)
	return nil
}

func snReconcileTestSkips(report *SnReconcileReport) []string {
	skips := []string{}
	for _, skip := range report.Skipped {
		skips = append(skips, skip.PaymentId.String()+" "+skip.Reason+" "+strings.Join(skip.OrphanIds, ","))
	}
	return skips
}

// An md5 of every row of each table the command could write.
func (self *snReconcileTestFixture) tables() (digest string) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT concat_ws(' ',
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.payment_id),'')) FROM account_payment t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.event_id),'')) FROM audit_account_payment t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.task_id),'')) FROM pending_task t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.contract_id,t.balance_id,t.network_id),'')) FROM transfer_escrow_sweep t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.operation_id,t.origin_payment_id),'')) FROM account_payment_bonus t))`).Scan(&digest))
	})
	return
}

// Every account_payment row as JSON, by payment id.
func (self *snReconcileTestFixture) payments() map[server.Id]string {
	rows := map[server.Id]string{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT payment_id,row_to_json(t)::text FROM account_payment t`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var row string
				server.Raise(result.Scan(&paymentId, &row))
				rows[paymentId] = row
			}
		})
	})
	return rows
}

// The payments with an advance_payment task scheduled, sorted.
func (self *snReconcileTestFixture) scheduled() []server.Id {
	ids := []server.Id{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT args_json FROM pending_task WHERE run_once_key LIKE '["advance_payment",%'`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var args string
				server.Raise(result.Scan(&args))
				var advance AdvancePaymentArgs
				server.Raise(json.Unmarshal([]byte(args), &advance))
				ids = append(ids, advance.PaymentId)
			}
		})
	})
	return snReconcileTestSorted(ids...)
}

func (self *snReconcileTestFixture) auditCount(paymentId server.Id, eventType string) (count int) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_type=$2`, paymentId, eventType).Scan(&count))
	})
	return
}

func snReconcileTestSorted(ids ...server.Id) []server.Id {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// A revenue-only planner configuration, so a plan pays exactly its sweeps.
func snReconcileTestRevenueConfig() *model.SubsidyConfig {
	config := *model.EnvSubsidyConfig()
	config.MinPayoutUsd = 0
	config.UsdPerActiveUser = 0
	config.SubscriptionNetRevenueFraction = 0
	config.MinWalletPayoutUsd = 0
	return &config
}

func snReconcileTestDate(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// RC-1: a stuck row without a network fails the command before Circle is read.
func TestSnReconcileStuckRowWithoutNetworkFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		paymentId := f.legacy(networkId, &walletId, snReconcileTestCutoff, model.UsdToNanoCents(1))
		f.exec(`UPDATE account_payment SET network_id=NULL WHERE payment_id=$1`, paymentId)
		f.cancelWithRecord(paymentId, "COMPLETE", "0xpaid")
		report, err := SnReconcile(f.session, false, "")
		if report != nil || err == nil || !strings.Contains(err.Error(), paymentId.String()) || len(f.reads) != 0 || len(f.ledger.listed) != 0 {
			t.Fatal("a stuck row without a network did not fail the command first", report, err)
		}
	})
}

// RC-3
func TestSnReconcileOwnRecordStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		want := map[server.Id]string{}
		row := func(state string, circleHash string, rowHash string, class string) server.Id {
			networkId, walletId, _ := f.network("MATIC")
			paymentId := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
			f.cancelWithRecord(paymentId, state, circleHash)
			if rowHash != "" {
				f.exec(`UPDATE account_payment SET tx_hash=$2 WHERE payment_id=$1`, paymentId, rowHash)
			}
			want[paymentId] = class
			return paymentId
		}
		complete := row("COMPLETE", "0xcomplete", "", snReconcilePaid)
		f.records["record-"+complete.String()] = CircleTransaction{Id: "record-" + complete.String(), State: "COMPLETE", TxHash: "0xcomplete", WalletId: "rc3-other-wallet"}
		failed := row("FAILED", "", "", snReconcilePayable)
		row("DENIED", "", "", snReconcilePayable)
		row("cancelled", "", "", snReconcilePayable)
		row("FAILED", "0xmoved", "", "unresolved: contradictory")
		rowSide := row("CANCELLED", "", "0xrow", "unresolved: contradictory")
		for _, state := range []string{"INITIATED", "PENDING_RISK_SCREENING", "CLEARED", "QUEUED", "SENT", "STUCK", "CONFIRMED", "SOMETHING_NEW"} {
			row(state, "", "", "unresolved: in_flight")
		}
		missing := row("FAILED", "", "", "unresolved: record_unavailable")
		delete(f.records, "record-"+missing.String())
		mismatch := row("FAILED", "", "", "unresolved: record_unavailable")
		f.records["record-"+mismatch.String()] = CircleTransaction{Id: "another-transaction", State: "FAILED"}

		report := f.dry()
		f.expect(report, want)
		if evidence := snReconcileTestRow(t, report, complete).Evidence; *evidence != (model.SnReconcileEvidence{TransactionId: "record-" + complete.String(), State: "COMPLETE", TxHash: "0xcomplete"}) {
			t.Fatalf("complete evidence = %+v", evidence)
		}
		if evidence := snReconcileTestRow(t, report, failed).Evidence; *evidence != (model.SnReconcileEvidence{TransactionId: "record-" + failed.String(), State: "FAILED"}) {
			t.Fatalf("failed evidence = %+v", evidence)
		}
		if evidence := snReconcileTestRow(t, report, rowSide).Evidence; evidence.State != "CANCELLED" || evidence.TxHash != "" {
			t.Fatalf("row-side hash evidence = %+v", evidence)
		}
		for _, unavailable := range []server.Id{missing, mismatch} {
			if evidence := snReconcileTestRow(t, report, unavailable).Evidence; *evidence != (model.SnReconcileEvidence{TransactionId: "record-" + unavailable.String()}) {
				t.Fatalf("unavailable evidence = %+v", evidence)
			}
		}
		if !slices.Contains(report.Listing.Wallets, "rc3-other-wallet") || len(f.reads) != len(want) {
			t.Fatal("a record's wallet was not listed or a record was not read once", report.Listing.Wallets, len(f.reads))
		}
	})
}

// RC-4
func TestSnReconcileOwnHashStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		want := map[server.Id]string{}
		row := func(hash string, class string) server.Id {
			networkId, walletId, _ := f.network("MATIC")
			paymentId := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
			f.exec(`UPDATE account_payment SET canceled=true,tx_hash=$2 WHERE payment_id=$1`, paymentId, hash)
			want[paymentId] = class
			return paymentId
		}
		complete := row("0xabc1", snReconcilePaid)
		row("0xabc2", "unresolved: contradictory")
		row("0xabc3", "unresolved: in_flight")
		missing := row("0xabc4", "unresolved: record_unavailable")
		row("0xabc5", "unresolved: contradictory")
		hashed := func(id string, state string, hash string) CircleTransaction {
			transaction := f.transfer(id, state, "0xelsewhere", created)
			transaction.TxHash = hash
			return transaction
		}
		f.list(snReconcileTestMaticWallet, hashed("hash-1", "COMPLETE", "0xABC1"), hashed("hash-2", "FAILED", "0xAbC2"), hashed("hash-3", "SENT", "0xabc3"), hashed("hash-5a", "COMPLETE", "0xabc5"))
		f.list(snReconcileTestSolWallet, hashed("hash-5b", "COMPLETE", "0xabc5"))

		report := f.dry()
		f.expect(report, want)
		if evidence := snReconcileTestRow(t, report, complete).Evidence; *evidence != (model.SnReconcileEvidence{TransactionId: "hash-1", State: "COMPLETE", TxHash: "0xabc1"}) {
			t.Fatalf("hash evidence = %+v", evidence)
		}
		if evidence := snReconcileTestRow(t, report, missing).Evidence; *evidence != (model.SnReconcileEvidence{TxHash: "0xabc4"}) {
			t.Fatalf("unmatched hash evidence = %+v", evidence)
		}
		if len(f.reads) != 0 {
			t.Fatal("a hash-only row was read by record", f.reads)
		}
	})
}

// RC-5, A3, A4: the wallet set, listed from the earliest orphan window.
func TestSnReconcileListingWalletSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		f.ledger.developerWallets = []CircleWallet{{ID: "developer-wallet", Address: "0xdeveloper"}, {ID: snReconcileTestSolWallet, Address: "0xsol"}}
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		from := snReconcileTestClosed.Add(-snReconcileTestDay)
		networkId, walletId, address := f.network("MATIC")
		failed := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		record := f.cancelWithRecord(failed, "FAILED", "")
		f.records[record] = CircleTransaction{Id: record, State: "FAILED", WalletId: "legacy-wallet-a"}
		walletless := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		record = f.cancelWithRecord(walletless, "FAILED", "")
		f.records[record] = CircleTransaction{Id: record, State: "FAILED"}
		inside := f.completedRecord(networkId, &walletId, "rc5-b", &[]time.Time{from.Add(2 * time.Hour)}[0])
		f.exec(`UPDATE account_payment SET payment_receipt=$2 WHERE payment_id=$1`, inside, `{"data":{"transaction":{"id":"rc5-b","walletId":"legacy-wallet-b"}}}`)
		outside := f.completedRecord(networkId, &walletId, "rc5-c", &[]time.Time{from.Add(-time.Microsecond)}[0])
		f.exec(`UPDATE account_payment SET payment_receipt=$2 WHERE payment_id=$1`, outside, `{"walletId":"legacy-wallet-c"}`)
		listed := f.transfer("rc5-b", "COMPLETE", address, from.Add(2*time.Hour))
		listed.WalletId = "legacy-wallet-b"
		f.list("legacy-wallet-b", listed)

		before := server.NowUtc()
		report := f.dry()
		after := server.NowUtc()
		f.expect(report, map[server.Id]string{failed: snReconcilePayable, walletless: snReconcilePayable})
		want := []string{"developer-wallet", "legacy-wallet-a", "legacy-wallet-b", snReconcileTestMaticWallet, snReconcileTestSolWallet}
		if !slices.Equal(f.ledger.listed, want) || !slices.Equal(report.Listing.Wallets, want) || f.ledger.developerReads != 1 {
			t.Fatalf("listed %v, report %v, want %v", f.ledger.listed, report.Listing.Wallets, want)
		}
		for _, listedFrom := range f.ledger.froms {
			if !listedFrom.Equal(from) {
				t.Fatal("listing from", listedFrom, "want", from)
			}
		}
		if !report.Listing.From.Equal(from) || report.Listing.Start.Before(before) || report.Listing.Start.After(after) ||
			report.Listing.TransactionCount != 1 || !report.Listing.Complete || len(report.Listing.MissingIds) != 0 || len(report.Listing.MismatchedIds) != 0 {
			t.Fatalf("listing = %+v", report.Listing)
		}
	})
}

// RC-5, A4: a listing error fails the command; it is never a short result.
func TestSnReconcileListingErrorFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.cancelWithKey(f.legacy(networkId, &walletId, snReconcileTestCutoff, model.UsdToNanoCents(1)))
		before := f.tables()
		for _, failure := range []*error{&f.ledger.developerErr, &f.ledger.listErr} {
			*failure = errors.New("synthetic listing failure")
			for _, send := range []bool{false, true} {
				report, err := SnReconcile(f.session, send, "")
				if report != nil || !errors.Is(err, *failure) {
					t.Fatal("a listing error did not fail the command", report, err)
				}
			}
			*failure = nil
		}
		if f.tables() != before || f.ledger.balanceReads != 0 {
			t.Fatal("a failed listing went on to write or read balances")
		}
	})
}

// RC-5: with no stuck row nothing is listed; the balances are still reported.
func TestSnReconcileWithoutStuckRowsListsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.legacy(networkId, &walletId, snReconcileTestCutoff, model.UsdToNanoCents(1))
		report := f.send()
		if len(f.ledger.listed) != 0 || f.ledger.developerReads != 0 || report.Listing.From != nil || report.Listing.Start != nil || !report.Listing.Complete ||
			len(report.Rows) != 0 || len(report.Revived) != 0 || report.Relisting != nil || f.ledger.balanceReads != 4 || len(report.Funding) != 2 {
			t.Fatalf("a run without stuck rows = %+v", report)
		}
	})
}

// RC-5: the configured payout wallets are in every listing; the command
// refuses without them.
func TestSnReconcileRequiresConfiguredWallets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		polygonWalletId = func() string { return "" }
		if report, err := SnReconcile(f.session, false, ""); report != nil || err == nil || !strings.Contains(err.Error(), "MATIC") {
			t.Fatal("a missing MATIC wallet id was not refused", err)
		}
		polygonWalletId = func() string { return snReconcileTestMaticWallet }
		solanaUSDCAddress = func() string { return "" }
		if report, err := SnReconcile(f.session, false, ""); report != nil || err == nil || !strings.Contains(err.Error(), "SOL") {
			t.Fatal("a missing SOL token address was not refused", err)
		}
		if len(f.ledger.listed) != 0 || f.ledger.balanceReads != 0 {
			t.Fatal("an unconfigured command read Circle")
		}
	})
}

// RC-6
func TestSnReconcileListingIncomplete(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		from := snReconcileTestClosed.Add(-snReconcileTestDay)
		networkId, walletId, address := f.network("MATIC")
		keyOnly := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(keyOnly)
		paid := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")
		at := func(value time.Time) *time.Time {
			return &value
		}
		f.completedRecord(networkId, &walletId, "rc6-missing", at(from.Add(2*time.Hour)))
		f.completedRecord(networkId, &walletId, "rc6-early", at(from.Add(30*time.Minute)))
		f.completedRecord(networkId, &walletId, "rc6-recent", at(server.NowUtc()))
		f.completedRecord(networkId, &walletId, "rc6-listed", at(from.Add(3*time.Hour)))
		f.list(snReconcileTestMaticWallet, f.transfer("rc6-listed", "COMPLETE", address, from.Add(3*time.Hour)))

		report := f.dry()
		f.expect(report, map[server.Id]string{keyOnly: "unresolved: listing_incomplete", paid: snReconcilePaid})
		if report.Listing.Complete || !slices.Equal(report.Listing.MissingIds, []string{"rc6-missing"}) || len(report.Listing.MismatchedIds) != 0 {
			t.Fatalf("listing = %+v", report.Listing)
		}
	})
}

// RC-7, A3: the orphan window opens a day before the row's earliest contract
// closed.
func TestSnReconcileOrphanTransfers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		base := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		closed := snReconcileTestClosed
		day := snReconcileTestDay
		keyOnly := func(networkId server.Id, walletId server.Id, created time.Time) server.Id {
			paymentId := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
			f.cancelWithKey(paymentId)
			return paymentId
		}
		// The network's payout wallet, in another case, 23 hours before the close.
		networkA, walletA, addressA := f.network("MATIC")
		a := keyOnly(networkA, walletA, base)
		// The pinned wallet belongs to another network.
		networkB, _, _ := f.network("MATIC")
		networkX := f.bareNetwork()
		b := keyOnly(networkB, f.wallet(networkX, "MATIC", "0xPinnedElsewhere", true), base.Add(day))
		// A retired wallet of the network.
		networkC, walletC, _ := f.network("MATIC")
		f.wallet(networkC, "MATIC", "0xRetired", false)
		c := keyOnly(networkC, walletC, base.Add(2*day))
		// The transfer is older than the earliest close less the window.
		networkD, walletD, addressD := f.network("MATIC")
		d := keyOnly(networkD, walletD, base.Add(3*day))
		// Failed transfers are never orphans.
		networkE, walletE, addressE := f.network("MATIC")
		e := keyOnly(networkE, walletE, base.Add(4*day))
		// A payment record or a receipt id names the transfer.
		networkF, walletF, addressF := f.network("MATIC")
		named := keyOnly(networkF, walletF, base.Add(5*day))
		networkG, walletG, addressG := f.network("MATIC")
		receipted := keyOnly(networkG, walletG, base.Add(6*day))
		f.completedRecord(networkF, &walletF, "by-record", nil)
		f.exec(`UPDATE account_payment SET completed=true,payment_receipt='{"id":"by-receipt"}' WHERE payment_id=$1`, f.payment(networkG, &walletG, base, 0))
		// Only an INBOUND deposit is never an orphan; an empty or unknown type is.
		networkH, walletH, addressH := f.network("MATIC")
		deposited := keyOnly(networkH, walletH, base.Add(7*day))
		networkI, walletI, addressI := f.network("MATIC")
		untyped := keyOnly(networkI, walletI, base.Add(8*day))
		networkJ, walletJ, addressJ := f.network("MATIC")
		lowerCase := keyOnly(networkJ, walletJ, base.Add(9*day))
		typed := func(transaction CircleTransaction, transactionType string) CircleTransaction {
			transaction.TransactionType = transactionType
			return transaction
		}
		f.list(snReconcileTestMaticWallet,
			f.transfer("orphan-a", "COMPLETE", strings.ToUpper(addressA), closed.Add(-23*time.Hour)),
			f.transfer("orphan-b", "SENT", "0xpinnedelsewhere", closed),
			f.transfer("orphan-c", "INITIATED", "0xretired", closed.Add(day)),
			f.transfer("orphan-d", "COMPLETE", addressD, closed.Add(-25*time.Hour)),
			f.transfer("failed-e", "FAILED", addressE, closed),
			f.transfer("denied-e", "denied", addressE, closed),
			f.transfer("cancelled-e", "CANCELLED", addressE, closed),
			f.transfer("by-record", "COMPLETE", addressF, closed),
			f.transfer("by-receipt", "COMPLETE", addressG, closed),
			f.transfer("orphan-z", "COMPLETE", "0xnobody", closed),
			typed(f.transfer("deposit-h", "COMPLETE", addressH, closed), "INBOUND"),
			typed(f.transfer("orphan-i", "COMPLETE", addressI, closed), ""),
			typed(f.transfer("orphan-j", "COMPLETE", addressJ, closed), "inbound"))

		report := f.dry()
		f.expect(report, map[server.Id]string{a: "unresolved: orphan_transfer", b: "unresolved: orphan_transfer", c: "unresolved: orphan_transfer",
			d: snReconcilePayable, e: snReconcilePayable, named: snReconcilePayable, receipted: snReconcilePayable,
			deposited: snReconcilePayable, untyped: "unresolved: orphan_transfer", lowerCase: "unresolved: orphan_transfer"})
		for paymentId, orphanId := range map[server.Id]string{a: "orphan-a", b: "orphan-b", c: "orphan-c", untyped: "orphan-i", lowerCase: "orphan-j"} {
			if ids := snReconcileTestRow(t, report, paymentId).OrphanIds; !slices.Equal(ids, []string{orphanId}) {
				t.Fatal("blocking orphans", paymentId, ids, orphanId)
			}
		}
		wantNetworks := map[string][]server.Id{"orphan-a": {networkA}, "orphan-b": {networkX}, "orphan-c": {networkC}, "orphan-d": {networkD}, "orphan-z": nil,
			"orphan-i": {networkI}, "orphan-j": {networkJ}}
		wantTypes := map[string]string{"orphan-i": "", "orphan-j": "inbound"}
		ids := []string{}
		for _, orphan := range report.Orphans {
			ids = append(ids, orphan.Id)
			wantType, ok := wantTypes[orphan.Id]
			if !ok {
				wantType = "OUTBOUND"
			}
			if !slices.Equal(orphan.NetworkIds, wantNetworks[orphan.Id]) || orphan.WalletId != snReconcileTestMaticWallet || orphan.TransactionType != wantType ||
				!slices.Equal(orphan.Amounts, []string{"0.99"}) || orphan.CreateDate == "" || orphan.Destination == "" || orphan.Unattributed != (orphan.Id == "orphan-z") {
				t.Fatalf("orphan = %+v", orphan)
			}
		}
		if !slices.Equal(ids, []string{"orphan-a", "orphan-b", "orphan-c", "orphan-d", "orphan-z", "orphan-i", "orphan-j"}) {
			t.Fatal("orphans", ids)
		}
	})
}

// RC-7: an orphan candidate's unreadable createDate fails the command; a failed
// transfer's is never read.
func TestSnReconcileOrphanCreateDateUnreadableFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.cancelWithKey(f.legacy(networkId, &walletId, snReconcileTestCutoff, model.UsdToNanoCents(1)))
		failed := f.transfer("failed-undated", "FAILED", "0xelsewhere", snReconcileTestCutoff)
		failed.CreateDate = "yesterday"
		f.list(snReconcileTestMaticWallet, failed)
		f.dry()
		undated := f.transfer("undated", "COMPLETE", "0xelsewhere", snReconcileTestCutoff)
		undated.CreateDate = "yesterday"
		f.list(snReconcileTestMaticWallet, undated)
		if report, err := SnReconcile(f.session, false, ""); report != nil || err == nil || !strings.Contains(err.Error(), "undated") {
			t.Fatal("an unreadable createDate was not refused", report, err)
		}
	})
}

// RC-8, A5: a failed census is `census_mismatch` while the row holds sweeps,
// and `replanned` without them only with a later early holder. Only a loaded
// policy bounds the close time.
func TestSnReconcileCensusFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkA, walletA, _ := f.network("MATIC")
		mismatch := f.payment(networkA, &walletA, created, 1, f.contract(networkA, snReconcileTestClosed, model.UsdToNanoCents(1)))
		f.cancelWithKey(mismatch)
		networkB, walletB, _ := f.network("MATIC")
		replanned := f.legacy(networkB, &walletB, created, model.UsdToNanoCents(1))
		f.cancelWithRecord(replanned, "FAILED", "")
		f.moveSweeps(replanned, f.payment(networkB, &walletB, created.Add(31*snReconcileTestDay), 0))
		networkC, walletC, _ := f.network("MATIC")
		lateClose := f.payment(networkC, &walletC, created, 0, f.contract(networkC, snReconcileTestCutoff.Add(time.Hour), model.UsdToNanoCents(1)))
		f.cancelWithKey(lateClose)

		f.expect(f.dry(), map[server.Id]string{mismatch: "unresolved: census_mismatch", replanned: snReconcileReplanned, lateClose: snReconcilePayable})
		controllerPayoutSchedule(t, snReconcileTestCutoff)
		f.expect(f.dry(), map[server.Id]string{mismatch: "unresolved: census_mismatch", replanned: snReconcileReplanned, lateClose: "unresolved: census_mismatch"})
	})
}

// RC-8: the census needs the policy; an unreadable policy fails the command.
func TestSnReconcilePolicyUnavailableFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte("schema: unsupported\n")))
		if report, err := SnReconcile(f.session, false, ""); report != nil || err == nil {
			t.Fatal("an unreadable policy was not refused", report, err)
		}
	})
}

// RC-9: review comes first, before Circle's own evidence and the listing.
func TestSnReconcileReviewRequiredFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkA, walletA, _ := f.network("MATIC")
		paid := f.legacy(networkA, &walletA, created, model.UsdToNanoCents(1))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")
		networkB, walletB, _ := f.network("MATIC")
		unretained := f.legacy(networkB, &walletB, created, model.UsdToNanoCents(1))
		f.reserveUnjournaled(unretained)
		networkC, walletC, addressC := f.network("MATIC")
		blocked := f.legacy(networkC, &walletC, created, model.UsdToNanoCents(1))
		f.cancelWithKey(blocked)
		f.review(paid, unretained, blocked)
		f.list(snReconcileTestMaticWallet, f.transfer("would-block", "COMPLETE", addressC, created))
		f.expect(f.dry(), map[server.Id]string{paid: "unresolved: review_required", unretained: "unresolved: review_required", blocked: "unresolved: review_required"})
	})
}

// RC-10
func TestSnReconcileNoPayoutWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		keyOnly := func(networkId server.Id, walletId *server.Id) server.Id {
			paymentId := f.legacy(networkId, walletId, created, model.UsdToNanoCents(1))
			f.cancelWithKey(paymentId)
			return paymentId
		}
		withoutRow := f.bareNetwork()
		withoutRowWallet := f.wallet(withoutRow, "MATIC", "0xno-payout-row", true)
		missing := keyOnly(withoutRow, &withoutRowWallet)
		inactiveNetwork, inactiveWallet, _ := f.network("MATIC")
		f.exec(`UPDATE account_wallet SET active=false WHERE wallet_id=$1`, inactiveWallet)
		inactive := keyOnly(inactiveNetwork, &inactiveWallet)
		ethNetwork, ethWallet, _ := f.network("ETH")
		unsupported := keyOnly(ethNetwork, &ethWallet)
		foreignNetwork := f.bareNetwork()
		_, otherWallet, _ := f.network("MATIC")
		f.exec(`INSERT INTO payout_wallet(network_id,wallet_id) VALUES($1,$2)`, foreignNetwork, otherWallet)
		foreign := keyOnly(foreignNetwork, nil)
		solanaNetwork, solanaWallet, _ := f.network(" solana ")
		spelled := keyOnly(solanaNetwork, &solanaWallet)

		report := f.dry()
		f.expect(report, map[server.Id]string{missing: "unresolved: no_payout_wallet", inactive: "unresolved: no_payout_wallet",
			unsupported: "unresolved: no_payout_wallet", foreign: "unresolved: no_payout_wallet", spelled: snReconcilePayable})
		if chain := snReconcileTestRow(t, report, spelled).Chain; chain != "SOL" {
			t.Fatal("chain", chain)
		}
	})
}

// RC-11: the sender's own fee and threshold.
func TestSnReconcileBelowFee(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		keyOnly := func(revenue model.NanoCents) server.Id {
			networkId, walletId, _ := f.network("MATIC")
			paymentId := f.legacy(networkId, &walletId, created, revenue)
			f.cancelWithKey(paymentId)
			return paymentId
		}
		cent := keyOnly(model.UsdToNanoCents(0.01))
		above := keyOnly(model.UsdToNanoCents(0.01) + 1)
		zero := keyOnly(0)
		report := f.dry()
		f.expect(report, map[server.Id]string{cent: "unresolved: below_fee", above: snReconcilePayable, zero: "unresolved: below_fee"})
		if item := snReconcileTestRow(t, report, above); item.sendNanoCents != 1 || item.SendUsd != model.NanoCentsToUsd(model.UsdToNanoCents(0.01)+1)-0.01 {
			t.Fatalf("smallest payable = %+v", item)
		}
	})
}

// RC-12
func TestSnReconcilePayableAmounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		maticNetwork, maticWallet, _ := f.network("MATIC")
		matic := f.legacy(maticNetwork, &maticWallet, created, model.UsdToNanoCents(10))
		f.cancelWithRecord(matic, "FAILED", "")
		solNetwork, solWallet, _ := f.network("SOL")
		sol := f.legacy(solNetwork, &solWallet, created, model.UsdToNanoCents(2.5))
		f.reserveUnjournaled(sol)
		paid := f.legacy(maticNetwork, &maticWallet, created, model.UsdToNanoCents(4))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")

		report := f.dry()
		f.expect(report, map[server.Id]string{matic: snReconcilePayable, sol: "unresolved: upstream_scope", paid: snReconcilePaid})
		for paymentId, want := range map[server.Id]struct {
			owed  model.NanoCents
			send  float64
			chain string
		}{matic: {model.UsdToNanoCents(10), 10 - 0.01, "MATIC"}, sol: {0, 0, ""}, paid: {0, 0, ""}} {
			item := snReconcileTestRow(t, report, paymentId)
			if item.OwedNanoCents != want.owed || item.SendUsd != want.send || item.Chain != want.chain {
				t.Fatalf("payable %s = %+v", paymentId, item)
			}
		}
		maticFunding, solFunding := snReconcileTestFunding(t, report, "MATIC"), snReconcileTestFunding(t, report, "SOL")
		if maticFunding.PayableCount != 1 || maticFunding.SendNanoCents != model.UsdToNanoCents(9.99) || maticFunding.WalletId != snReconcileTestMaticWallet ||
			solFunding.PayableCount != 0 || solFunding.SendNanoCents != 0 || solFunding.WalletId != snReconcileTestSolWallet ||
			maticFunding.BalanceNanoCents != model.UsdToNanoCents(1000) || maticFunding.ShortfallNanoCents != 0 || solFunding.ShortfallNanoCents != 0 ||
			maticFunding.RevivedCount != 0 || solFunding.RevivedNanoCents != 0 {
			t.Fatalf("funding = %+v %+v", maticFunding, solFunding)
		}
	})
}

// RC-13
func TestSnReconcileFundingShortfallRefusesSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		first := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(10))
		f.cancelWithKey(first)
		second := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(5))
		f.cancelWithKey(second)
		balance := snReconcileTestMaticWallet + "/" + snReconcileTestMaticUsdc
		f.ledger.balances[balance] = model.UsdToNanoCents(14)
		before := f.tables()

		report := f.dry()
		if funding := snReconcileTestFunding(t, report, "MATIC"); funding.SendNanoCents != model.UsdToNanoCents(14.98) || funding.ShortfallNanoCents != model.UsdToNanoCents(0.98) {
			t.Fatalf("dry-run funding = %+v", funding)
		}
		report, err := SnReconcile(f.session, true, report.Digest)
		if !errors.Is(err, ErrSnReconcileUnfunded) || report == nil || report.Mode != "send" || len(report.Revived) != 0 || len(report.Skipped) != 0 || report.Relisting != nil ||
			snReconcileTestFunding(t, report, "MATIC").ShortfallNanoCents != model.UsdToNanoCents(0.98) || snReconcileTestFunding(t, report, "SOL").ShortfallNanoCents != 0 {
			t.Fatal("an unfunded send was not refused with its report", report, err)
		}
		if f.tables() != before || len(f.posts) != 0 {
			t.Fatal("an unfunded send wrote")
		}
		f.ledger.balances[balance] = model.UsdToNanoCents(14.98)
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{first, second}) {
			t.Fatal("an exactly funded send was refused", report.Revived)
		}
	})
}

// RC-13: a balance read error fails the command before any write.
func TestSnReconcileBalanceReadErrorFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.cancelWithKey(f.legacy(networkId, &walletId, snReconcileTestCutoff, model.UsdToNanoCents(1)))
		f.ledger.balanceErr = errors.New("synthetic balance read failure")
		before := f.tables()
		if report, err := SnReconcile(f.session, true, ""); report != nil || !errors.Is(err, f.ledger.balanceErr) {
			t.Fatal("a balance read error did not fail the command", report, err)
		}
		if f.tables() != before {
			t.Fatal("a failed balance read wrote")
		}
	})
}

// RC-14: each payable row is revived and scheduled in its own transaction,
// with an audit event naming the run.
func TestSnReconcileSendRevivesAndSchedules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		failed := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(3))
		record := f.cancelWithRecord(failed, "FAILED", "")
		unretained := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(2))
		f.reserveUnjournaled(unretained)

		before := f.payments()
		report := f.send()
		// RC-24: an RC-2 row is reported, never revived.
		if !slices.Equal(report.Revived, []server.Id{failed}) || !slices.Equal(f.scheduled(), []server.Id{failed}) || len(report.Skipped) != 0 ||
			snReconcileTestRow(t, report, unretained).Reason != snReconcileUpstreamScope || f.payments()[unretained] != before[unretained] {
			t.Fatal("payable rows were not revived and scheduled", report.Revived, f.scheduled())
		}
		var details string
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT event_details FROM audit_account_payment WHERE payment_id=$1 AND event_type='sn_reconcile_revived'`, failed).Scan(&details))
		})
		var revival struct {
			RunId    server.Id                 `json:"run_id"`
			Evidence model.SnReconcileEvidence `json:"circle_evidence"`
			Record   string                    `json:"payment_record"`
		}
		if err := json.Unmarshal([]byte(details), &revival); err != nil || revival.RunId != report.RunId || revival.Record != record ||
			revival.Evidence != (model.SnReconcileEvidence{TransactionId: record, State: "FAILED"}) {
			t.Fatal("revival audit", details, err)
		}
		for _, paymentId := range report.Revived {
			payment, err := model.GetPayment(f.ctx, paymentId)
			if err != nil || payment.Canceled || payment.CircleIdempotencyKey != nil || payment.PaymentRecord != nil || f.auditCount(paymentId, "sn_reconcile_revived") != 1 {
				t.Fatal("revived row", payment, err)
			}
		}
	})
}

// RC-15
func TestSnReconcileSkipsChangedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		recorded := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(recorded)
		inexact := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(inexact)
		revivable := f.legacy(networkId, &walletId, created.Add(2*time.Hour), model.UsdToNanoCents(1))
		f.cancelWithRecord(revivable, "FAILED", "")
		digest := f.dry().Digest
		var changed map[server.Id]string
		// Another operator acts between the census and the revive.
		f.ledger.beforeBalance = func() {
			f.exec(`UPDATE account_payment SET payment_record='another-operator' WHERE payment_id=$1`, recorded)
			f.moveSweeps(inexact, f.payment(networkId, &walletId, created.Add(31*snReconcileTestDay), 0))
			changed = f.payments()
		}

		report := f.sendWith(digest)
		f.expect(report, map[server.Id]string{recorded: snReconcilePayable, inexact: snReconcilePayable, revivable: snReconcilePayable})
		if !slices.Equal(report.Revived, []server.Id{revivable}) || !slices.Equal(snReconcileTestSkips(report), []string{recorded.String() + " changed ", inexact.String() + " changed "}) ||
			!slices.Equal(f.scheduled(), []server.Id{revivable}) {
			t.Fatal("changed rows were not skipped while the run continued", report.Revived, snReconcileTestSkips(report))
		}
		after := f.payments()
		for _, paymentId := range []server.Id{recorded, inexact} {
			if after[paymentId] != changed[paymentId] || f.auditCount(paymentId, "sn_reconcile_revived") != 0 {
				t.Fatal("a skipped row was written", paymentId)
			}
		}
		if !strings.Contains(report.Text(), "skipped: 2\n  "+recorded.String()+" changed\n  "+inexact.String()+" changed\n") {
			t.Fatal("skipped rows missing from the report", report.Text())
		}
	})
}

// RC-16
func TestSnReconcileRevivedRowPaysOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		controllerPayoutSchedule(t, snReconcileTestCutoff)
		networkId, walletId, address := f.network("MATIC")
		paymentId := f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(10))
		f.cancelWithRecord(paymentId, "FAILED", "")
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{paymentId}) {
			t.Fatal("not revived", report.Revived)
		}
		if err := model.RequireProviderUsdcPayment(f.ctx, paymentId); err != nil {
			t.Fatal("the sender's admission refused the revived row", err)
		}
		plan, err := model.CreatePaymentPlan(f.ctx, snReconcileTestRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := plan.NetworkPayments[networkId]; ok {
			t.Fatal("a later plan moved the revived row's sweeps")
		}
		if report := f.send(); len(report.Rows) != 0 || len(report.Revived) != 0 {
			t.Fatal("a second run found the revived row stuck", report.Rows)
		}
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, f.session)
		if err != nil || result.Complete || result.Canceled || len(f.posts) != 1 || f.posts[0].amount != 9.99 || f.posts[0].destination != address || f.posts[0].network != "MATIC" {
			t.Fatal("the sender did not pay the revived row once", result, err, f.posts)
		}
		if report := f.send(); len(report.Rows) != 0 || len(f.posts) != 1 {
			t.Fatal("a paid revived row became stuck again", report.Rows)
		}
	})
}

// RC-17, A2: §6 lists each network's rows holding early earnings, with its
// RC-1 rows by class. Networks with a paid RC-1 row or an orphan come first.
func TestSnReconcileDuplicatesReport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		t0 := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		later := t0.Add(31 * snReconcileTestDay)
		// A pending row holding sweeps from an earlier epoch.
		holder := func(networkId server.Id, walletId server.Id, revenues ...model.NanoCents) server.Id {
			contracts := []server.Id{}
			for _, revenue := range revenues {
				contracts = append(contracts, f.contract(networkId, snReconcileTestClosed, revenue))
			}
			paymentId := f.payment(networkId, &walletId, later, 0, contracts...)
			f.epoch(paymentId, later)
			return paymentId
		}
		quietNetwork, quietWallet, _ := f.network("MATIC")
		inFlight := f.legacy(quietNetwork, &quietWallet, t0, model.UsdToNanoCents(1))
		f.cancelWithRecord(inFlight, "SENT", "0xsent")
		quietHolder := holder(quietNetwork, quietWallet, model.UsdToNanoCents(1))
		paidNetwork, paidWallet, _ := f.network("MATIC")
		paid := f.legacy(paidNetwork, &paidWallet, t0, model.UsdToNanoCents(4))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")
		paidReplanned := f.legacy(paidNetwork, &paidWallet, t0.Add(time.Hour), model.UsdToNanoCents(6))
		f.cancelWithRecord(paidReplanned, "COMPLETE", "0xpaid-replanned")
		paidHolder := holder(paidNetwork, paidWallet, model.UsdToNanoCents(2), model.UsdToNanoCents(3))
		f.moveSweeps(paidReplanned, paidHolder)
		orphanNetwork, orphanWallet, orphanAddress := f.network("MATIC")
		orphanHolder := holder(orphanNetwork, orphanWallet, model.UsdToNanoCents(1))
		f.list(snReconcileTestMaticWallet, f.transfer("orphan", "COMPLETE", orphanAddress, t0))
		// A network without early earnings has no entry.
		otherNetwork, otherWallet, _ := f.network("MATIC")
		f.cancelWithRecord(f.legacy(otherNetwork, &otherWallet, t0, model.UsdToNanoCents(1)), "COMPLETE", "0xother")

		report := f.dry()
		networks := []server.Id{}
		for _, duplicates := range report.Duplicates {
			networks = append(networks, duplicates.NetworkId)
		}
		if !slices.Equal(networks, append(snReconcileTestSorted(paidNetwork, orphanNetwork), quietNetwork)) {
			t.Fatal("duplicate networks", networks)
		}
		for _, duplicates := range report.Duplicates {
			canceled := []string{}
			for _, item := range duplicates.Canceled {
				canceled = append(canceled, item.PaymentId.String()+" "+snReconcileClassText(item.Class, item.Reason))
			}
			holders := []string{}
			for _, holder := range duplicates.Holders {
				holders = append(holders, fmt.Sprintf("%s %s %d %d", holder.PaymentId, holder.State, holder.EarlyRevenue, holder.EarlyByteCount))
			}
			switch duplicates.NetworkId {
			case paidNetwork:
				if !slices.Equal(canceled, []string{paid.String() + " paid", paidReplanned.String() + " paid_replanned"}) ||
					!slices.Equal(holders, []string{fmt.Sprintf("%s planned %d 300", paidHolder, model.UsdToNanoCents(11))}) ||
					duplicates.EarlyRevenueNanoCents != model.UsdToNanoCents(11) || duplicates.EarlyByteCount != 300 || len(duplicates.CanceledByClass) != 2 ||
					*duplicates.CanceledByClass[0] != (SnReconcileClassTotal{Class: snReconcilePaid, Count: 1, PayoutNanoCents: model.UsdToNanoCents(4)}) ||
					*duplicates.CanceledByClass[1] != (SnReconcileClassTotal{Class: snReconcilePaidReplanned, Count: 1, PayoutNanoCents: model.UsdToNanoCents(6)}) {
					t.Fatal("paid network duplicates", canceled, holders, duplicates)
				}
			case orphanNetwork:
				if len(canceled) != 0 || len(duplicates.CanceledByClass) != 0 || !slices.Equal(holders, []string{fmt.Sprintf("%s planned %d 100", orphanHolder, model.UsdToNanoCents(1))}) {
					t.Fatal("orphan network duplicates", canceled, holders)
				}
			default:
				if !slices.Equal(canceled, []string{inFlight.String() + " unresolved: in_flight"}) || !slices.Equal(holders, []string{fmt.Sprintf("%s planned %d 100", quietHolder, model.UsdToNanoCents(1))}) ||
					*duplicates.CanceledByClass[0] != (SnReconcileClassTotal{Class: snReconcileUnresolved, Reason: snReconcileInFlight, Count: 1, PayoutNanoCents: model.UsdToNanoCents(1)}) {
					t.Fatal("quiet network duplicates", canceled, holders)
				}
			}
		}
	})
}

// A2: a safely canceled row holding early earnings is listed, as is a pending one.
func TestSnReconcileDuplicatesIncludeSafelyCanceledHolders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		t0 := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		paid := f.legacy(networkId, &walletId, t0, model.UsdToNanoCents(4))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")
		canceled := f.legacy(networkId, &walletId, t0.Add(31*snReconcileTestDay), model.UsdToNanoCents(2))
		f.epoch(canceled, t0.Add(31*snReconcileTestDay))
		f.exec(`UPDATE account_payment SET canceled=true,cancel_time=$2 WHERE payment_id=$1`, canceled, t0.Add(62*snReconcileTestDay))
		pending := f.legacy(networkId, &walletId, t0.Add(63*snReconcileTestDay), model.UsdToNanoCents(3))
		f.epoch(pending, t0.Add(63*snReconcileTestDay))

		report := f.dry()
		f.expect(report, map[server.Id]string{paid: snReconcilePaid})
		if len(report.Duplicates) != 1 || len(report.Duplicates[0].Holders) != 2 {
			t.Fatal("holders", report.Duplicates)
		}
		holders := report.Duplicates[0].Holders
		if holders[0].PaymentId != canceled || holders[0].State != "safely_canceled" || holders[0].EarlyRevenue != model.UsdToNanoCents(2) ||
			holders[1].PaymentId != pending || holders[1].State != "planned" || holders[1].EarlyRevenue != model.UsdToNanoCents(3) ||
			report.Duplicates[0].EarlyRevenueNanoCents != model.UsdToNanoCents(5) {
			t.Fatal("a safely canceled holder is missing", holders)
		}
	})
}

// RC-18
func TestSnReconcileDryRunWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		keyOnly := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(keyOnly)
		failed := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithRecord(failed, "FAILED", "")
		unretained := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.reserveUnjournaled(unretained)
		holder := f.legacy(networkId, &walletId, created.Add(31*snReconcileTestDay), model.UsdToNanoCents(1))
		f.epoch(holder, created.Add(31*snReconcileTestDay))
		f.list(snReconcileTestMaticWallet, f.transfer("unattributed", "COMPLETE", "0xnobody", created))
		before := f.tables()
		report := f.dry()
		f.expect(report, map[server.Id]string{keyOnly: snReconcilePayable, failed: snReconcilePayable, unretained: "unresolved: upstream_scope"})
		if f.tables() != before || len(f.posts) != 0 || len(f.scheduled()) != 0 || report.Mode != "dry_run" || len(report.Revived) != 0 || len(report.Skipped) != 0 ||
			report.Relisting != nil || len(report.Duplicates) != 1 {
			t.Fatal("the dry run wrote, scheduled or posted")
		}
	})
}

// RC-19: one row per class and reason, --send with the dry run's digest, then
// the sender for the revived rows. `listing_incomplete` applies to every row
// of a run that reaches RC-7, so it cannot share a run with payable rows; RC-6
// covers it.
func TestSnReconcileEndToEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		controllerPayoutSchedule(t, snReconcileTestCutoff)
		f.ledger.developerWallets = []CircleWallet{{ID: "sn-reconcile-developer-wallet", Address: "0xdeveloper"}}
		base := snReconcileTestCutoff.Add(-35 * snReconcileTestDay)
		at := func(hours int) time.Time {
			return base.Add(time.Duration(hours) * time.Hour)
		}
		want := map[server.Id]string{}

		// Canceled holding only a key; pays on MATIC.
		maticNetwork, maticWallet, maticAddress := f.network("MATIC")
		payableKey := f.legacy(maticNetwork, &maticWallet, at(0), model.UsdToNanoCents(12))
		f.cancelWithKey(payableKey)
		want[payableKey] = snReconcilePayable
		// A failed record pinned to a retired MATIC wallet; pays the current SOL wallet.
		solNetwork, _, solAddress := f.network("SOL")
		retired := f.wallet(solNetwork, "MATIC", "0xretired-matic", false)
		payableFailed := f.legacy(solNetwork, &retired, at(1), model.UsdToNanoCents(7))
		f.cancelWithRecord(payableFailed, "FAILED", "")
		want[payableFailed] = snReconcilePayable
		// An unjournaled key that was never canceled: reported, never revived (RC-24).
		unretainedNetwork, unretainedWallet, _ := f.network("MATIC")
		unretained := f.legacy(unretainedNetwork, &unretainedWallet, at(2), model.UsdToNanoCents(3))
		f.reserveUnjournaled(unretained)
		want[unretained] = "unresolved: upstream_scope"
		next := func() (server.Id, server.Id, string) {
			return f.network("MATIC")
		}
		networkId, walletId, _ := next()
		paid := f.legacy(networkId, &walletId, at(3), model.UsdToNanoCents(4))
		f.cancelWithRecord(paid, "COMPLETE", "0xpaid")
		want[paid] = snReconcilePaid
		// Paid by Circle and re-planned; a later stuck row holds early earnings.
		duplicateNetwork, duplicateWallet, _ := next()
		paidReplanned := f.legacy(duplicateNetwork, &duplicateWallet, at(4), model.UsdToNanoCents(5))
		f.cancelWithRecord(paidReplanned, "COMPLETE", "0xpaid-replanned")
		queued := f.payment(duplicateNetwork, &duplicateWallet, at(4+31*24), 0)
		f.moveSweeps(paidReplanned, queued)
		duplicate := f.legacy(duplicateNetwork, &duplicateWallet, at(4+32*24), model.UsdToNanoCents(2))
		f.epoch(duplicate, at(4+32*24))
		f.reserveUnjournaled(duplicate)
		want[paidReplanned], want[duplicate] = snReconcilePaidReplanned, "unresolved: duplicate_risk"
		// A failed attempt whose sweeps the next plan after its cancellation holds.
		replannedNetwork, replannedWallet, _ := next()
		replanned := f.legacy(replannedNetwork, &replannedWallet, at(5), model.UsdToNanoCents(1))
		f.cancelWithRecord(replanned, "FAILED", "")
		f.moveSweeps(replanned, f.payment(replannedNetwork, &replannedWallet, at(5+31*24), 0))
		want[replanned] = snReconcileReplanned
		networkId, walletId, _ = next()
		review := f.legacy(networkId, &walletId, at(7), model.UsdToNanoCents(1))
		f.cancelWithKey(review)
		f.review(review)
		want[review] = "unresolved: review_required"
		networkId, walletId, _ = next()
		contradictory := f.legacy(networkId, &walletId, at(8), model.UsdToNanoCents(1))
		f.cancelWithRecord(contradictory, "FAILED", "0xmoved-anyway")
		want[contradictory] = "unresolved: contradictory"
		networkId, walletId, _ = next()
		inFlight := f.legacy(networkId, &walletId, at(9), model.UsdToNanoCents(1))
		f.cancelWithRecord(inFlight, "SENT", "0xsent")
		want[inFlight] = "unresolved: in_flight"
		networkId, walletId, _ = next()
		unavailable := f.legacy(networkId, &walletId, at(10), model.UsdToNanoCents(1))
		delete(f.records, f.cancelWithRecord(unavailable, "FAILED", ""))
		want[unavailable] = "unresolved: record_unavailable"
		networkId, walletId, orphanAddress := next()
		orphaned := f.legacy(networkId, &walletId, at(11), model.UsdToNanoCents(6))
		f.cancelWithKey(orphaned)
		f.list(snReconcileTestMaticWallet, f.transfer("hidden-transfer", "COMPLETE", orphanAddress, at(12)))
		want[orphaned] = "unresolved: orphan_transfer"
		networkId, walletId, _ = next()
		mismatch := f.payment(networkId, &walletId, at(13), 1, f.contract(networkId, snReconcileTestClosed, model.UsdToNanoCents(1)))
		f.cancelWithKey(mismatch)
		want[mismatch] = "unresolved: census_mismatch"
		networkId, walletId, _ = next()
		reaped := f.payment(networkId, &walletId, at(14), model.UsdToNanoCents(2))
		f.cancelWithKey(reaped)
		want[reaped] = "unresolved: sweeps_missing"
		walletless := f.legacy(f.bareNetwork(), nil, at(15), model.UsdToNanoCents(1))
		f.cancelWithKey(walletless)
		want[walletless] = "unresolved: no_payout_wallet"
		networkId, walletId, _ = next()
		belowFee := f.legacy(networkId, &walletId, at(16), model.UsdToNanoCents(0.01))
		f.cancelWithKey(belowFee)
		want[belowFee] = "unresolved: below_fee"
		// Out of scope: a journaled attempt, a completed payment, a safely canceled one.
		networkId, walletId, address := next()
		journaled := f.legacy(networkId, &walletId, at(17), model.UsdToNanoCents(1))
		journalKey := server.NewId()
		f.exec(`UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, journaled, journalKey)
		f.exec(`INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`, journalKey, journaled)
		completed := f.legacy(networkId, &walletId, at(18), model.UsdToNanoCents(1))
		f.exec(`UPDATE account_payment SET completed=true,complete_time=$2,payment_record='completed-record',payment_time=$2,payment_receipt=$3,tx_hash='0xcompleted' WHERE payment_id=$1`,
			completed, at(19), `{"data":{"transaction":{"id":"completed-record","walletId":"`+snReconcileTestMaticWallet+`","state":"COMPLETE"}}}`)
		f.exec(`UPDATE account_payment SET canceled=true,cancel_time=$2 WHERE payment_id=$1`, f.legacy(networkId, &walletId, at(20), model.UsdToNanoCents(1)), at(21))
		f.list(snReconcileTestMaticWallet, f.transfer("completed-record", "COMPLETE", address, at(19)), f.transfer("failed-transfer", "FAILED", address, at(20)),
			f.transfer("unknown-destination", "COMPLETE", "0xunknown", at(21)), f.transfer("internal", "COMPLETE", "0xDEVELOPER", at(22)))

		beforeRows := f.payments()
		beforeTables := f.tables()
		digest := f.dry().Digest
		report := f.sendWith(digest)
		t.Logf("sn-reconcile --send report:\n%s", report.Text())
		f.expect(report, want)
		revived := []server.Id{payableKey, payableFailed}
		if report.Digest != digest || !slices.Equal(report.Revived, revived) || len(report.Skipped) != 0 || !slices.Equal(f.scheduled(), snReconcileTestSorted(revived...)) {
			t.Fatal("exactly the payable rows were not revived and scheduled", report.Revived, report.Skipped, f.scheduled())
		}
		unattributed := []string{}
		for _, orphan := range report.Orphans {
			if orphan.Unattributed {
				unattributed = append(unattributed, orphan.Id)
			}
		}
		if !slices.Equal(unattributed, []string{"unknown-destination"}) || !slices.Contains(report.Listing.Wallets, "sn-reconcile-developer-wallet") || report.Relisting == nil {
			t.Fatal("unattributed orphans or developer wallets", unattributed, report.Listing.Wallets)
		}
		duplicateNetworks := []server.Id{}
		for _, duplicates := range report.Duplicates {
			duplicateNetworks = append(duplicateNetworks, duplicates.NetworkId)
		}
		if !slices.Equal(duplicateNetworks, []server.Id{duplicateNetwork, replannedNetwork}) || report.Duplicates[0].Holders[0].PaymentId != queued {
			t.Fatal("duplicates", duplicateNetworks)
		}
		if beforeTables == f.tables() {
			t.Fatal("the send wrote nothing")
		}

		// The sender pays each revived row once, for its send amount, to its current wallet.
		destinations := map[server.Id][2]string{payableKey: {maticAddress, "MATIC"}, payableFailed: {solAddress, "SOL"}}
		for _, paymentId := range revived {
			posts := len(f.posts)
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, f.session)
			if err != nil || result.Complete || result.Canceled || len(f.posts) != posts+1 {
				t.Fatal("the sender did not submit the revived row", paymentId, result, err)
			}
			post, item := f.posts[posts], snReconcileTestRow(t, report, paymentId)
			if post.amount != item.SendUsd || post.destination != destinations[paymentId][0] || post.network != destinations[paymentId][1] || item.Chain != post.network {
				t.Fatalf("post for %s = %+v, row %+v", paymentId, post, item)
			}
		}
		for _, paymentId := range revived {
			if result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, f.session); err != nil || result.Complete || len(f.posts) != len(revived) {
				t.Fatal("a retry posted again", paymentId, result, err)
			}
		}

		// No other row changed.
		after := f.payments()
		for paymentId, row := range beforeRows {
			if !slices.Contains(revived, paymentId) && after[paymentId] != row {
				t.Fatal("an unrevived row changed", paymentId)
			}
		}
		// A second run finds nothing payable and posts nothing.
		second := f.send()
		for _, item := range second.Rows {
			if item.Class == snReconcilePayable || slices.Contains(revived, item.PaymentId) {
				t.Fatal("a second run found", item.PaymentId, snReconcileClassText(item.Class, item.Reason))
			}
		}
		if len(second.Rows) != len(want)-len(revived) || len(second.Revived) != 0 || len(f.posts) != len(revived) {
			t.Fatal("second run", len(second.Rows), second.Revived, len(f.posts))
		}
	})
}

// RC-20
func TestSnReconcileReportText(t *testing.T) {
	id := func(digit string) server.Id {
		return server.RequireParseId(strings.Repeat(digit, 8) + "-" + strings.Repeat(digit, 4) + "-" + strings.Repeat(digit, 4) + "-" + strings.Repeat(digit, 4) + "-" + strings.Repeat(digit, 12))
	}
	at := time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)
	from, start := at.Add(-90*snReconcileTestDay), at.Add(time.Second)
	relistFrom, relistStart := start.Add(-time.Hour), at.Add(time.Minute)
	empty := &SnReconcileReport{RunId: id("1"), Time: at, Mode: "dry_run", Digest: "d1", Listing: &SnReconcileListing{Complete: true}}
	if got, want := empty.Text(), "sn-reconcile run 11111111-1111-1111-1111-111111111111 at 2026-10-12T16:00:00Z, mode dry_run, policy none\n"+
		"digest d1\nlisting: none, no stuck rows\norphans: 0\nclasses:\nfunding:\nrows: 0\nduplicates: 0 network(s)\n"; got != want {
		t.Fatalf("empty report:\n%s\nwant:\n%s", got, want)
	}
	payable := &SnReconcileRowReport{PaymentId: id("3"), NetworkId: id("4"), Kind: model.SnReconcileCanceledWithEvidence, Class: snReconcilePayable,
		PayoutNanoCents: model.UsdToNanoCents(10), OwedNanoCents: model.UsdToNanoCents(10), SendUsd: 9.99, Chain: "MATIC",
		Evidence: &model.SnReconcileEvidence{TransactionId: "tx-1", State: "FAILED"}}
	orphaned := &SnReconcileRowReport{PaymentId: id("5"), NetworkId: id("4"), Kind: model.SnReconcileUnretainedKey, Class: snReconcileUnresolved,
		Reason: snReconcileOrphanTransfer, PayoutNanoCents: model.UsdToNanoCents(2), OrphanIds: []string{"orphan-1", "orphan-2"}}
	paid := &SnReconcileRowReport{PaymentId: id("6"), NetworkId: id("7"), Kind: model.SnReconcileCanceledWithEvidence, Class: snReconcilePaidReplanned,
		PayoutNanoCents: model.UsdToNanoCents(5), Evidence: &model.SnReconcileEvidence{TransactionId: "tx-2", State: "COMPLETE", TxHash: "0xpaid"}}
	report := &SnReconcileReport{
		RunId: id("1"), Time: at, Mode: "send", PolicySha256: "abc123", Digest: "d2",
		Listing: &SnReconcileListing{Wallets: []string{"wallet-a", "wallet-b"}, From: &from, Start: &start, TransactionCount: 3,
			MissingIds: []string{"record-x"}, MismatchedIds: []string{"record-y"}},
		Orphans: []*SnReconcileOrphan{
			{Id: "orphan-1", WalletId: "wallet-a", TransactionType: "OUTBOUND", State: "COMPLETE", Amounts: []string{"1.50"}, CreateDate: "2026-09-01T00:00:00Z", Destination: "0xdest", NetworkIds: []server.Id{id("4"), id("7")}},
			{Id: "orphan-2", WalletId: "wallet-b", TransactionType: "OUTBOUND", State: "SENT", Amounts: []string{"1", "2"}, CreateDate: "2026-09-02T00:00:00Z", Destination: "0xother", Unattributed: true},
		},
		Classes: []*SnReconcileClassTotal{{Class: snReconcilePaidReplanned, Count: 1, PayoutNanoCents: model.UsdToNanoCents(5)},
			{Class: snReconcilePayable, Count: 1, PayoutNanoCents: model.UsdToNanoCents(10)},
			{Class: snReconcileUnresolved, Reason: snReconcileOrphanTransfer, Count: 1, PayoutNanoCents: model.UsdToNanoCents(2)}},
		Funding: []*SnReconcileFunding{{Chain: "MATIC", WalletId: "wallet-a", PayableCount: 1, SendNanoCents: model.UsdToNanoCents(9.99), RevivedCount: 1,
			RevivedNanoCents: model.UsdToNanoCents(1.99), BalanceNanoCents: model.UsdToNanoCents(5), ShortfallNanoCents: model.UsdToNanoCents(6.98)}},
		Rows: []*SnReconcileRowReport{payable, orphaned, paid},
		Duplicates: []*SnReconcileDuplicates{{NetworkId: id("7"), Canceled: []*SnReconcileRowReport{paid},
			CanceledByClass: []*SnReconcileClassTotal{{Class: snReconcilePaidReplanned, Count: 1, PayoutNanoCents: model.UsdToNanoCents(5)}},
			Holders: []*model.SnReconcileHolder{{PaymentId: id("8"), PaymentPlanId: id("9"), NetworkId: id("7"), State: "safely_canceled", Payout: model.UsdToNanoCents(6),
				PayoutByteCount: 300, SubsidyPayout: model.UsdToNanoCents(1), EarlyRevenue: model.UsdToNanoCents(3), EarlyByteCount: 100, EarlyBonus: model.UsdToNanoCents(2)}},
			EarlyRevenueNanoCents: model.UsdToNanoCents(3), EarlyByteCount: 100}},
		Relisting: &SnReconcileListing{Wallets: []string{"wallet-a"}, From: &relistFrom, Start: &relistStart, TransactionCount: 1, Complete: true},
		Revived:   []server.Id{id("3")},
		Skipped:   []*SnReconcileSkip{{PaymentId: id("2"), Reason: snReconcileChanged}, {PaymentId: id("5"), Reason: snReconcileOrphanTransfer, OrphanIds: []string{"orphan-3"}}},
	}
	want := `sn-reconcile run 11111111-1111-1111-1111-111111111111 at 2026-10-12T16:00:00Z, mode send, policy abc123
digest d2
listing: from 2026-07-14T16:00:00Z, started 2026-10-12T16:00:01Z, 2 wallet(s), 3 transaction(s), INCOMPLETE, 1 record(s) missing, 1 record(s) at another address
  wallet wallet-a
  wallet wallet-b
  missing record-x
  mismatched record-y
orphans: 2
  orphan-1 OUTBOUND COMPLETE 1.50 created 2026-09-01T00:00:00Z from wallet wallet-a to 0xdest, networks 44444444-4444-4444-4444-444444444444,77777777-7777-7777-7777-777777777777
  orphan-2 OUTBOUND SENT 1,2 created 2026-09-02T00:00:00Z from wallet wallet-b to 0xother, networks none, UNATTRIBUTED
classes:
  paid_replanned                       1 5.000000 USD
  payable                              1 10.000000 USD
  unresolved: orphan_transfer          1 2.000000 USD
funding:
  MATIC wallet wallet-a: 1 payable, send 9.990000 USD, 1 revived pending, 1.990000 USD, balance 5.000000 USD, shortfall 6.980000 USD
rows: 3
  33333333-3333-3333-3333-333333333333 network 44444444-4444-4444-4444-444444444444 canceled_with_evidence payable, owed 10.000000 USD, send 9.990000 USD on MATIC, circle tx-1 FAILED -
  55555555-5555-5555-5555-555555555555 network 44444444-4444-4444-4444-444444444444 unretained_key unresolved: orphan_transfer, owed 0.000000 USD, orphans orphan-1,orphan-2
  66666666-6666-6666-6666-666666666666 network 77777777-7777-7777-7777-777777777777 canceled_with_evidence paid_replanned, owed 0.000000 USD, circle tx-2 COMPLETE 0xpaid
duplicates: 1 network(s)
  network 77777777-7777-7777-7777-777777777777: early revenue 3.000000 USD, early bytes 100
    canceled paid_replanned: 1 row(s), payout 5.000000 USD
    canceled 66666666-6666-6666-6666-666666666666 paid_replanned, payout 5.000000 USD
    holder 88888888-8888-8888-8888-888888888888 plan 99999999-9999-9999-9999-999999999999 safely_canceled, early revenue 3.000000 USD, early bytes 100, early bonus 2.000000 USD, bytes 300, subsidy 1.000000 USD, reliability 0.000000 USD
relisting: from 2026-10-12T15:00:01Z, started 2026-10-12T16:01:00Z, 1 wallet(s), 1 transaction(s), complete
  wallet wallet-a
revived: 1
  33333333-3333-3333-3333-333333333333
skipped: 2
  22222222-2222-2222-2222-222222222222 changed
  55555555-5555-5555-5555-555555555555 orphan_transfer, orphans orphan-3
`
	if got := report.Text(); got != want {
		t.Fatalf("report:\n%s\nwant:\n%s", got, want)
	}
	// The JSON document carries the same content.
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run_id", "time", "mode", "policy_sha256", "digest", "listing", "orphans", "classes", "funding", "rows", "duplicates", "relisting", "revived", "skipped"} {
		if _, ok := document[key]; !ok {
			t.Fatal("JSON report lacks", key)
		}
	}
	row := document["rows"].([]any)[0].(map[string]any)
	evidence := row["circle_evidence"].(map[string]any)
	listing := document["listing"].(map[string]any)
	holder := document["duplicates"].([]any)[0].(map[string]any)["holders"].([]any)[0].(map[string]any)
	if row["class"] != "payable" || row["owed_nano_cents"] != float64(model.UsdToNanoCents(10)) || row["send_usd"] != 9.99 || row["chain"] != "MATIC" ||
		evidence["transaction_id"] != "tx-1" || listing["complete"] != false || listing["missing_ids"].([]any)[0] != "record-x" || listing["mismatched_ids"].([]any)[0] != "record-y" ||
		document["orphans"].([]any)[1].(map[string]any)["unattributed"] != true || holder["state"] != "safely_canceled" || holder["early_bonus_nano_cents"] != float64(model.UsdToNanoCents(2)) ||
		document["skipped"].([]any)[1].(map[string]any)["orphan_ids"].([]any)[0] != "orphan-3" || document["digest"] != "d2" {
		t.Fatal("JSON report", string(data))
	}
}

// RC-21, A2: a stuck row holding early earnings, sweeps or corrections, is
// never paid, RC-2 included.
func TestSnReconcileDuplicateRisk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		t0 := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		closed := snReconcileTestClosed
		networkId, walletId, _ := f.network("MATIC")
		stuck := func(created time.Time, epoch *time.Time, revenue model.NanoCents, unretained bool) server.Id {
			paymentId := f.legacy(networkId, &walletId, created, revenue)
			if epoch != nil {
				f.epoch(paymentId, *epoch)
			}
			if unretained {
				f.reserveUnjournaled(paymentId)
			} else {
				f.cancelWithKey(paymentId)
			}
			return paymentId
		}
		at := func(value time.Time) *time.Time {
			return &value
		}
		explained := stuck(t0, nil, model.UsdToNanoCents(1), false)
		atStart := stuck(t0.Add(time.Hour), at(closed), model.UsdToNanoCents(1), false)
		zeroRevenue := stuck(t0.Add(2*time.Hour), at(closed.Add(time.Hour)), 0, true)
		unretained := stuck(t0.Add(3*time.Hour), at(closed.Add(time.Hour)), model.UsdToNanoCents(1), true)
		// A correction moved in from an earlier row; its own is not early.
		movedBonus := f.legacy(networkId, &walletId, t0.Add(4*time.Hour), model.UsdToNanoCents(1))
		origin := f.payment(networkId, &walletId, t0.Add(-snReconcileTestDay), 0)
		f.moveBonus(origin, movedBonus, model.UsdToNanoCents(2))
		f.cancelWithKey(movedBonus)
		ownBonus := f.legacy(networkId, &walletId, t0.Add(5*time.Hour), model.UsdToNanoCents(1))
		f.exec(`INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
			SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'own correction',$3,'' FROM account_payment WHERE payment_id=$4`, server.NewId(), model.UsdToNanoCents(1), closed, ownBonus)
		f.exec(`UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, ownBonus, model.UsdToNanoCents(1))
		f.cancelWithKey(ownBonus)
		// A plan without an epoch explains none of its row's sweeps. Created last,
		// it raises no earlier row's bound.
		withoutEpoch := stuck(t0.Add(10*time.Hour), nil, model.UsdToNanoCents(1), false)
		f.exec(`DELETE FROM subsidy_payment WHERE payment_plan_id=(SELECT payment_plan_id FROM account_payment WHERE payment_id=$1)`, withoutEpoch)

		f.expect(f.dry(), map[server.Id]string{explained: snReconcilePayable, atStart: "unresolved: duplicate_risk", zeroRevenue: "unresolved: duplicate_risk",
			unretained: "unresolved: duplicate_risk", movedBonus: "unresolved: duplicate_risk", ownBonus: snReconcilePayable, withoutEpoch: "unresolved: duplicate_risk"})
	})
}

// A1: a request journaled under a row's key after the census, by any writer,
// leaves the row untouched and reports it as changed.
func TestSnReconcileReviveSkipsKeyJournaledAfterCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		unretained := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.reserveUnjournaled(unretained)
		canceled := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(canceled)
		revivable := f.legacy(networkId, &walletId, created.Add(2*time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(revivable)
		digest := f.dry().Digest
		f.ledger.beforeBalance = func() {
			for _, paymentId := range []server.Id{unretained, canceled} {
				payment, err := model.GetPayment(f.ctx, paymentId)
				if err != nil {
					t.Fatal(err)
				}
				f.exec(`INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`, *payment.CircleIdempotencyKey, paymentId)
			}
		}
		before := f.payments()
		report := f.sendWith(digest)
		// The RC-2 row was never payable (RC-24); the RC-1 key-only row is skipped.
		if !slices.Equal(report.Revived, []server.Id{revivable}) || !slices.Equal(snReconcileTestSkips(report), []string{canceled.String() + " changed "}) ||
			!slices.Equal(f.scheduled(), []server.Id{revivable}) {
			t.Fatal("a journaled key was revived", report.Revived, snReconcileTestSkips(report))
		}
		after := f.payments()
		for _, paymentId := range []server.Id{unretained, canceled} {
			if after[paymentId] != before[paymentId] {
				t.Fatal("a journaled row was written", paymentId)
			}
		}
	})
}

// A1: --send lists every wallet again from an hour before the census listing;
// a transfer that appeared since then skips the row it may pay.
func TestSnReconcileSendRelistsAndSkipsNewOrphan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkA, walletA, addressA := f.network("MATIC")
		blocked := f.legacy(networkA, &walletA, created, model.UsdToNanoCents(1))
		f.cancelWithKey(blocked)
		networkB, walletB, _ := f.network("MATIC")
		revivable := f.legacy(networkB, &walletB, created, model.UsdToNanoCents(1))
		f.cancelWithKey(revivable)
		digest := f.dry().Digest
		f.ledger.beforeBalance = func() {
			f.list(snReconcileTestMaticWallet, f.transfer("new-orphan", "INITIATED", addressA, server.NowUtc()))
		}
		f.ledger.listed, f.ledger.froms = nil, nil
		report := f.sendWith(digest)
		if !slices.Equal(report.Revived, []server.Id{revivable}) || !slices.Equal(snReconcileTestSkips(report), []string{blocked.String() + " orphan_transfer new-orphan"}) ||
			!slices.Equal(f.scheduled(), []server.Id{revivable}) {
			t.Fatal("the re-listing did not skip the newly blocked row", report.Revived, snReconcileTestSkips(report))
		}
		relistFrom := report.Listing.Start.Add(-time.Hour)
		if report.Relisting == nil || !report.Relisting.From.Equal(relistFrom) || !report.Relisting.Complete || !slices.Equal(report.Relisting.Wallets, report.Listing.Wallets) ||
			len(f.ledger.froms) != 2*len(report.Listing.Wallets) || !f.ledger.froms[len(f.ledger.froms)-1].Equal(relistFrom) ||
			!slices.ContainsFunc(report.Orphans, func(orphan *SnReconcileOrphan) bool { return orphan.Id == "new-orphan" }) {
			t.Fatal("relisting", report.Relisting, f.ledger.froms)
		}
		if snReconcileTestRow(t, report, blocked).Class != snReconcilePayable {
			t.Fatal("the re-listing changed the census")
		}
	})
}

// A1: a re-listing that fails RC-6 skips every payable row.
func TestSnReconcileSendRelistIncompleteSkipsAll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		first := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(first)
		second := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(second)
		f.completedRecord(networkId, &walletId, "moved-record", nil)
		digest := f.dry().Digest
		f.ledger.beforeBalance = func() {
			f.list(snReconcileTestMaticWallet, f.transfer("moved-record", "COMPLETE", "0xsomewhere-else", server.NowUtc()))
		}
		before := f.tables()
		report := f.sendWith(digest)
		if len(report.Revived) != 0 || !slices.Equal(snReconcileTestSkips(report), []string{first.String() + " listing_incomplete ", second.String() + " listing_incomplete "}) ||
			report.Relisting.Complete || !slices.Equal(report.Relisting.MismatchedIds, []string{"moved-record"}) || f.tables() != before {
			t.Fatal("an incomplete re-listing revived", report.Revived, snReconcileTestSkips(report), report.Relisting)
		}
	})
}

// A2: a restored row (not canceled, cancel_time NULL) cannot be found as a
// source, yet the stuck row holding its re-planned sweep is not paid.
func TestSnReconcileRestoredPartialReplanIsBlocked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		t0 := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		kept, moved := f.contract(networkId, snReconcileTestClosed, model.UsdToNanoCents(2)), f.contract(networkId, snReconcileTestClosed, model.UsdToNanoCents(3))
		// Paid by Circle, then partly re-planned and restored by restore_canceled_circle_retries.
		restored := f.payment(networkId, &walletId, t0, model.UsdToNanoCents(3), kept)
		f.exec(`UPDATE account_payment SET circle_idempotency_key=$2,payment_record='restored-record',cancel_time=NULL WHERE payment_id=$1`, restored, server.NewId())
		holder := f.payment(networkId, &walletId, t0.Add(31*snReconcileTestDay), 0, moved)
		f.cancelWithKey(holder)
		// Under the source rule it was paid: the restored row is no RC-1 row.
		f.expect(f.dry(), map[server.Id]string{holder: snReconcilePayable})
		// Its plan's epoch starts after the moved sweep closed.
		f.epoch(holder, t0.Add(31*snReconcileTestDay))
		f.expect(f.dry(), map[server.Id]string{holder: "unresolved: duplicate_risk"})
	})
}

// A3: an orphan created after the row's earliest sweep closed, though long
// before the row was created, may have paid those sweeps.
func TestSnReconcileOrphanBeforeRowBlocksReplannedSweeps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-10 * snReconcileTestDay)
		closed := snReconcileTestClosed
		networkY, walletY, addressY := f.network("MATIC")
		inWindow := f.legacy(networkY, &walletY, created, model.UsdToNanoCents(1))
		f.cancelWithKey(inWindow)
		networkZ, walletZ, addressZ := f.network("MATIC")
		beforeWindow := f.legacy(networkZ, &walletZ, created, model.UsdToNanoCents(1))
		f.cancelWithKey(beforeWindow)
		f.list(snReconcileTestMaticWallet, f.transfer("earlier-payment", "COMPLETE", addressY, closed.Add(snReconcileTestDay)),
			f.transfer("before-window", "COMPLETE", addressZ, closed.Add(-25*time.Hour)))
		report := f.dry()
		f.expect(report, map[server.Id]string{inWindow: "unresolved: orphan_transfer", beforeWindow: snReconcilePayable})
		if !report.Listing.From.Equal(closed.Add(-snReconcileTestDay)) || !slices.Equal(snReconcileTestRow(t, report, inWindow).OrphanIds, []string{"earlier-payment"}) {
			t.Fatal("orphan window", report.Listing.From)
		}
	})
}

// A4, A7: a transfer to an address no network registered and that is none of
// our developer wallets is unattributed, listed and bound into the digest.
func TestSnReconcileUnattributedOrphanEntersDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		f.ledger.developerWallets = []CircleWallet{{ID: "developer-wallet", Address: " 0xDeveloper\t"}}
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		payable := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(payable)
		f.list(snReconcileTestMaticWallet, f.transfer("to-developer", "COMPLETE", "0xdeveloper", created))
		clean := f.dry()
		f.list(snReconcileTestMaticWallet, f.transfer("to-nobody", "COMPLETE", "0xnobody", created))
		report := f.dry()
		f.expect(report, map[server.Id]string{payable: snReconcilePayable})
		if report.Digest == clean.Digest || !slices.Contains(report.Listing.Wallets, "developer-wallet") || len(report.Orphans) != 2 ||
			report.Orphans[0].Unattributed || !report.Orphans[1].Unattributed || !strings.Contains(report.Text(), "to 0xnobody, networks none, UNATTRIBUTED\n") {
			t.Fatal("unattributed orphan", report.Digest, clean.Digest, report.Orphans)
		}
		if _, err := SnReconcile(f.session, true, clean.Digest); !errors.Is(err, ErrSnReconcileDigestMismatch) {
			t.Fatal("a new unattributed orphan did not change what --send accepts", err)
		}
		if report := f.sendWith(report.Digest); !slices.Equal(report.Revived, []server.Id{payable}) {
			t.Fatal("not revived", report.Revived)
		}
	})
}

// A4: destinations compare trimmed of ASCII whitespace and case-insensitively,
// and the listing is incomplete unless every recorded transfer reaches its
// row's wallet that way.
func TestSnReconcileDestinationNormalisationChecked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		spacedNetwork := f.bareNetwork()
		spacedWallet := f.wallet(spacedNetwork, "MATIC", "\t0xAbCdEf\n", true)
		f.exec(`INSERT INTO payout_wallet(network_id,wallet_id) VALUES($1,$2)`, spacedNetwork, spacedWallet)
		blocked := f.legacy(spacedNetwork, &spacedWallet, created, model.UsdToNanoCents(1))
		f.cancelWithKey(blocked)
		recordedNetwork := f.bareNetwork()
		recordedWallet := f.wallet(recordedNetwork, "MATIC", " 0xRecorded \t", true)
		f.exec(`INSERT INTO payout_wallet(network_id,wallet_id) VALUES($1,$2)`, recordedNetwork, recordedWallet)
		payable := f.legacy(recordedNetwork, &recordedWallet, created, model.UsdToNanoCents(1))
		f.cancelWithKey(payable)
		f.completedRecord(recordedNetwork, &recordedWallet, "recorded-ok", nil)
		f.list(snReconcileTestMaticWallet, f.transfer("trimmed", "COMPLETE", "0xABCDEF", created), f.transfer("recorded-ok", "COMPLETE", "0xrecorded\r", created))

		report := f.dry()
		f.expect(report, map[server.Id]string{blocked: "unresolved: orphan_transfer", payable: snReconcilePayable})
		if !report.Listing.Complete || !slices.Equal(report.Orphans[0].NetworkIds, []server.Id{spacedNetwork}) {
			t.Fatal("normalised addresses", report.Listing, report.Orphans)
		}
		// A recorded transfer that reaches another address, or a row without a
		// wallet, cannot prove the address rule.
		f.completedRecord(recordedNetwork, &recordedWallet, "recorded-elsewhere", nil)
		f.completedRecord(recordedNetwork, nil, "recorded-walletless", nil)
		f.list(snReconcileTestMaticWallet, f.transfer("recorded-elsewhere", "COMPLETE", "0xother", created), f.transfer("recorded-walletless", "COMPLETE", "0xrecorded", created))
		report = f.dry()
		f.expect(report, map[server.Id]string{blocked: "unresolved: listing_incomplete", payable: "unresolved: listing_incomplete"})
		if report.Listing.Complete || !slices.Equal(report.Listing.MismatchedIds, []string{"recorded-elsewhere", "recorded-walletless"}) {
			t.Fatal("mismatched records", report.Listing)
		}
	})
}

// A5: a stuck row without sweeps is `replanned` only as RC-1 with a later
// early holder; otherwise its sweeps may have been reaped and it may be owed.
// The later rows here were created after the fix, so A8 does not apply.
func TestSnReconcileReapedRowIsNotReplanned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		later := model.SnReconcileReplanFixTime.Add(time.Second)
		reaped := func() (server.Id, server.Id, server.Id) {
			networkId, walletId, _ := f.network("MATIC")
			return f.payment(networkId, &walletId, created, model.UsdToNanoCents(2)), networkId, walletId
		}
		laterHolder := func(networkId server.Id, walletId server.Id, closed time.Time) {
			f.epoch(f.payment(networkId, &walletId, later, 0, f.contract(networkId, closed, model.UsdToNanoCents(2))), later)
		}
		replanned, networkA, walletA := reaped()
		f.cancelWithRecord(replanned, "FAILED", "")
		laterHolder(networkA, walletA, snReconcileTestClosed)
		withoutEvidence, _, _ := reaped()
		f.cancelWithKey(withoutEvidence)
		unretained, networkC, walletC := reaped()
		f.reserveUnjournaled(unretained)
		laterHolder(networkC, walletC, snReconcileTestClosed)
		closedAfter, networkD, walletD := reaped()
		f.cancelWithKey(closedAfter)
		laterHolder(networkD, walletD, created.Add(time.Hour))

		f.expect(f.dry(), map[server.Id]string{replanned: snReconcileReplanned, withoutEvidence: "unresolved: sweeps_missing",
			unretained: "unresolved: sweeps_missing", closedAfter: "unresolved: sweeps_missing"})
	})
}

// A6: a revived row without subsidy that the daily hung-payment sweep cancels
// before its first send is still paid exactly once, by the re-plan.
func TestSnReconcileRevivedRowHungCancelPaysOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		controllerPayoutSchedule(t, snReconcileTestCutoff)
		networkId, walletId, address := f.network("MATIC")
		paymentId := f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(10))
		f.cancelWithKey(paymentId)
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{paymentId}) {
			t.Fatal("not revived", report.Revived)
		}
		if canceled := model.CancelHungAccountPayments(f.ctx, server.NowUtc()); canceled != 1 {
			t.Fatal("the hung-payment sweep did not cancel the unsent revived row", canceled)
		}
		plan, err := model.CreatePaymentPlan(f.ctx, snReconcileTestRevenueConfig(), false, 0)
		if err != nil || plan.NetworkPayments[networkId] == nil || plan.NetworkPayments[networkId].Payout != model.UsdToNanoCents(10) {
			t.Fatal("the planner did not re-plan the canceled row's sweeps", plan, err)
		}
		for _, id := range []server.Id{paymentId, plan.NetworkPayments[networkId].PaymentId} {
			if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: id}, f.session); err != nil {
				t.Fatal(err)
			}
		}
		if len(f.posts) != 1 || f.posts[0].amount != 9.99 || f.posts[0].destination != address {
			t.Fatal("the obligation was not paid exactly once", f.posts)
		}
		if report := f.dry(); len(report.Rows) != 0 {
			t.Fatal("a hung-canceled revival became stuck", report.Rows)
		}
	})
}

// A7: --send refuses unless the digest of its own census equals --expect.
func TestSnReconcileSendRequiresMatchingDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		first := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(1))
		f.cancelWithKey(first)
		digest := f.dry().Digest
		before := f.tables()
		flipped := map[bool]string{true: "1", false: "0"}[digest[0] == '0'] + digest[1:]
		for _, expect := range []string{"", flipped, strings.ToUpper(digest) + "0"} {
			report, err := SnReconcile(f.session, true, expect)
			if !errors.Is(err, ErrSnReconcileDigestMismatch) || report == nil || report.Digest != digest || len(report.Revived) != 0 || report.Relisting != nil {
				t.Fatal("a wrong digest was accepted", expect, err)
			}
		}
		if f.tables() != before {
			t.Fatal("a refused send wrote")
		}
		// A payable row that appeared since the dry run changes the digest.
		second := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(second)
		if _, err := SnReconcile(f.session, true, digest); !errors.Is(err, ErrSnReconcileDigestMismatch) {
			t.Fatal("a changed census was sent", err)
		}
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{first, second}) {
			t.Fatal("the matching digest was refused", report.Revived)
		}
	})
}

// A7: funding also counts rows earlier runs revived that are not yet completed.
func TestSnReconcileFundingCountsEarlierRevivals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		first := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(10))
		f.cancelWithKey(first)
		f.ledger.balances[snReconcileTestMaticWallet+"/"+snReconcileTestMaticUsdc] = model.UsdToNanoCents(10)
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{first}) {
			t.Fatal("not revived", report.Revived)
		}
		second := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(5))
		f.cancelWithKey(second)
		report := f.dry()
		if funding := snReconcileTestFunding(t, report, "MATIC"); funding.PayableCount != 1 || funding.RevivedCount != 1 ||
			funding.RevivedNanoCents != model.UsdToNanoCents(9.99) || funding.ShortfallNanoCents != model.UsdToNanoCents(4.98) {
			t.Fatalf("funding = %+v", funding)
		}
		if _, err := SnReconcile(f.session, true, report.Digest); !errors.Is(err, ErrSnReconcileUnfunded) {
			t.Fatal("an earlier revival was not funded", err)
		}
		f.exec(`UPDATE account_payment SET completed=true WHERE payment_id=$1`, first)
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{second}) || snReconcileTestFunding(t, report, "MATIC").RevivedCount != 0 {
			t.Fatal("a completed revival still counted", report.Revived)
		}
	})
}

// A1: a re-listing that cannot be read fails the command before any revive.
func TestSnReconcileSendRelistFailureRevivesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.cancelWithKey(f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(1)))
		before := f.tables()
		listErr := errors.New("synthetic re-listing failure")
		for name, fail := range map[string]func(){
			"listing": func() { f.ledger.listErr = listErr },
			"createDate": func() {
				undated := f.transfer("undated", "COMPLETE", "0xelsewhere", server.NowUtc())
				undated.CreateDate = "now"
				f.list(snReconcileTestMaticWallet, undated)
			},
		} {
			digest := f.dry().Digest
			f.ledger.beforeBalance = fail
			report, err := SnReconcile(f.session, true, digest)
			if report != nil || err == nil || (name == "listing" && !errors.Is(err, listErr)) || (name == "createDate" && !strings.Contains(err.Error(), "undated")) {
				t.Fatal("a failed re-listing did not fail the command", name, report, err)
			}
			f.ledger.listErr = nil
			f.ledger.transactions = map[string][]CircleTransaction{}
		}
		if f.tables() != before {
			t.Fatal("a failed re-listing wrote")
		}
	})
}

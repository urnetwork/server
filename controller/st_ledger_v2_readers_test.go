package controller

// st_ledger_v2_readers_test.go — the earnings readers of settlement v2 epochs
// (VERSION2-FINAL.md §8.4) against the in-memory chain and builder with the
// real database: SV-5.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// ledgerV2Emails records each earnings email as "epoch user share alpha".
type ledgerV2Emails struct {
	lock sync.Mutex
	sent []string
}

func (self *ledgerV2Emails) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	earnings := template.(*EpochEarningsTemplate)
	self.lock.Lock()
	defer self.lock.Unlock()
	self.sent = append(self.sent, fmt.Sprintf("%d %s %d %s", earnings.Epoch, userAuth, earnings.ShareBps, earnings.UnclaimedRao))
	return nil
}

// await returns the emails, sorted, once n have been sent.
func (self *ledgerV2Emails) await(t testing.TB, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; {
		self.lock.Lock()
		sent := slices.Clone(self.sent)
		self.lock.Unlock()
		if n <= len(sent) || deadline.Before(time.Now()) {
			slices.Sort(sent)
			return sent
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func captureLedgerV2Emails(t testing.TB) *ledgerV2Emails {
	emails := &ledgerV2Emails{}
	previous := GetAWSMessageSender()
	SetMessageSender(emails)
	t.Cleanup(func() { SetMessageSender(previous) })
	// task scheduling hashes the local client address with the vault pepper
	t.Cleanup(server.Vault.PushSimpleResource("client.yml", []byte("client_ip_hash_pepper: synthetic-ledger-v2-readers-test-only\n")))
	return emails
}

// The pending leaderboard rebuilds; the trigger keeps at most one.
func ledgerV2LeaderboardRebuilds(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(RebuildPointsLeaderboard).TargetFunctionName()).Scan(&count))
	})
	return count
}

func ledgerV2Resolution(clientId server.Id, networkId server.Id, coldkey [32]byte) *model.StPayoutWalletResolution {
	return &model.StPayoutWalletResolution{ClientId: clientId, NetworkId: networkId, Mode: protocol.EarningWalletModeProvider, Coldkey: coldkey,
		ConsentHash: [32]byte{1}, ConsentGeneration: 1, HeadHash: [32]byte{1}, HeadGeneration: 1}
}

// ledgerV2ReaderNetworks are two networks with an admin to email.
type ledgerV2ReaderNetworks struct {
	n1, n2         server.Id
	user1, user2   string
	emails         *ledgerV2Emails
	recordedEmails []string
}

// recordLedgerV2ThreeEpochRoot closes epochs 5 and 6 without a root, so
// pool 1's first root, for epoch 7, covers epochs 5 to 7: A and B share
// epoch 5's 1,000 rao, B and C epoch 6's 3,000, and A has epoch 7's 500. In
// those epochs N1's clients selected A, C and A, and N2's B. The root is
// finalized, mirrored and recorded, and the earnings emails it fires are
// awaited.
func recordLedgerV2ThreeEpochRoot(t testing.TB, f *ledgerV2Fixture) *ledgerV2ReaderNetworks {
	t.Helper()
	networks := &ledgerV2ReaderNetworks{n1: server.NewId(), n2: server.NewId(), emails: captureLedgerV2Emails(t)}
	networks.user1 = model.Testing_CreateNetwork(f.ctx, networks.n1, "ledgerv2-n1", server.NewId())
	networks.user2 = model.Testing_CreateNetwork(f.ctx, networks.n2, "ledgerv2-n2", server.NewId())
	key := f.cfg.DeploymentKey()
	a, b, c := ledgerV2Coldkey(1), ledgerV2Coldkey(2), ledgerV2Coldkey(3)
	f.chain.emission[5], f.chain.emission[6], f.chain.emission[7] = 1000, 3000, 500
	f.builder.earners[5], f.builder.earners[6], f.builder.earners[7] = [][32]byte{a, b}, [][32]byte{b, c}, [][32]byte{a}
	for _, epoch := range []uint64{5, 6} {
		f.chain.head = f.chain.end(epoch) + 10
		if _, err := f.chain.LedgerV2Close(f.ctx, epoch, 1); err != nil {
			t.Fatal(err)
		}
	}
	f.chain.head = f.chain.end(7) + 10
	requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
	if input := f.builder.inputs[0]; input.Previous != nil || len(input.Epochs) != 3 || input.Epochs[0].Epoch != 5 || input.Epochs[2].Epoch != 7 {
		t.Fatalf("root input %+v", input)
	}
	p := f.chain.pending
	f.chain.approvals = 1
	f.chain.head = p.CommittedBlock + f.chain.challenge
	requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
	root := &model.StLedgerV2Root{NoId: 1, Seq: p.Seq, Epoch: 7, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, LedgerHash: p.LedgerHash}
	model.AddStLedgerV2Commit(f.ctx, key, root, p.CommittedBlock, 0)
	zero := uint64(0)
	root.RootIndex = &zero
	model.SetStLedgerV2RootFinalized(f.ctx, key, root, f.chain.head)

	c1, c2, c3 := server.Id{0x11}, server.Id{0x12}, server.Id{0x13}
	for epoch, resolutions := range map[uint64][]*model.StPayoutWalletResolution{
		5: {ledgerV2Resolution(c1, networks.n1, a), ledgerV2Resolution(c2, networks.n2, b)},
		6: {ledgerV2Resolution(c2, networks.n2, b), ledgerV2Resolution(c3, networks.n1, c)},
		7: {ledgerV2Resolution(c1, networks.n1, a)},
	} {
		if err := model.AddStPayoutWalletResolutions(f.ctx, key, epoch, 1, resolutions); err != nil {
			t.Fatal(err)
		}
	}
	for epoch := uint64(5); epoch <= 7; epoch++ {
		model.UpsertStEpoch(f.ctx, key, &model.StEpoch{Epoch: epoch, StartBlock: f.chain.end(epoch - 1), CommitDeadlineBlock: f.chain.end(epoch) + 1_200, FinalizeBlock: f.chain.end(epoch) + 2_400, Status: model.StEpochStatusOpen})
	}
	if rebuilds := ledgerV2LeaderboardRebuilds(t, f.ctx); rebuilds != 0 {
		t.Fatalf("%d leaderboard rebuilds before the root is recorded", rebuilds)
	}
	requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 7, p.Seq, 0), model.StPublishStatusConfirmed, false, "root 0 recorded")
	networks.recordedEmails = networks.emails.await(t, 5)
	return networks
}

// SV-5: when a finalized root that covers three epochs is recorded, the
// leaderboard rebuild fires once and each covered epoch's earnings email goes
// out once, with each network's share and alpha from that epoch's ledger
// amounts, not from share_bps.
func TestStLedgerV2RecordedRootFiresTheEarningsReaders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		networks := recordLedgerV2ThreeEpochRoot(t, f)
		key := f.cfg.DeploymentKey()
		want := []string{
			fmt.Sprintf("5 %s 5000 500", networks.user1),
			fmt.Sprintf("5 %s 5000 500", networks.user2),
			fmt.Sprintf("6 %s 5000 1500", networks.user1),
			fmt.Sprintf("6 %s 5000 1500", networks.user2),
			fmt.Sprintf("7 %s 10000 500", networks.user1),
		}
		slices.Sort(want)
		if !reflect.DeepEqual(networks.recordedEmails, want) {
			t.Fatalf("emails %q\nwant   %q", networks.recordedEmails, want)
		}
		if rebuilds := ledgerV2LeaderboardRebuilds(t, f.ctx); rebuilds != 1 {
			t.Fatalf("%d leaderboard rebuilds", rebuilds)
		}
		for epoch := uint64(5); epoch <= 7; epoch++ {
			if model.ClaimStEpochNotification(f.ctx, key, epoch) {
				t.Fatalf("epoch %d's earnings notification was not claimed", epoch)
			}
		}
		if !model.ClaimStEpochNotification(f.ctx, key, 8) {
			t.Fatal("an epoch the root does not cover was notified")
		}

		// The templates of a covered epoch carry the ledger amounts.
		templates := stEpochEarningsTemplates(f.ctx, key, 6, model.GetStEpoch(f.ctx, key, 6))
		if len(templates) != 2 || templates[networks.n1].ShareBps != 5000 || templates[networks.n1].UnclaimedRao.Cmp(big.NewInt(1500)) != 0 || templates[networks.n2].UnclaimedRao.Cmp(big.NewInt(1500)) != 0 {
			t.Fatalf("epoch 6 templates %+v", templates)
		}
	})
}

// SV-5: v1's finalization of a v2 epoch mirrors the status but neither sends
// the epoch's email nor rebuilds the leaderboard, which wait for its v2 root;
// a v1 epoch's finalization still does both, as it does when the v1 cutoff
// cannot be read and without settlement v2.
func TestStV1FinalizationOfAV2EpochNotifiesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		emails := captureLedgerV2Emails(t)
		key := f.cfg.DeploymentKey()
		model.UpsertStEpoch(f.ctx, key, &model.StEpoch{Epoch: 6, Status: model.StEpochStatusCommitted})

		stMarkEpochFinalized(f.ctx, key, 6)
		if row := model.GetStEpoch(f.ctx, key, 6); row == nil || row.Status != model.StEpochStatusFinalized {
			t.Fatalf("epoch 6 row %+v", row)
		}
		if rebuilds := ledgerV2LeaderboardRebuilds(t, f.ctx); rebuilds != 0 {
			t.Fatalf("%d leaderboard rebuilds for a v2 epoch", rebuilds)
		}
		if !model.ClaimStEpochNotification(f.ctx, key, 6) {
			t.Fatal("v1's finalization claimed a v2 epoch's notification")
		}

		stMarkEpochFinalized(f.ctx, key, 4)
		if model.ClaimStEpochNotification(f.ctx, key, 4) {
			t.Fatal("a v1 epoch's notification was not claimed")
		}
		if rebuilds := ledgerV2LeaderboardRebuilds(t, f.ctx); rebuilds != 1 {
			t.Fatalf("%d leaderboard rebuilds for a v1 epoch", rebuilds)
		}

		// The recorded activation answers the cutoff without vault2 (SV-9), so
		// the cutoff is unknown only for a deployment without one.
		if !model.ClearStLedgerV2Activation(f.ctx, key, model.GetStLedgerV2Activation(f.ctx, key)) {
			t.Fatal("the fixture's activation was not recorded")
		}
		resetLedgerV2Caches()
		f.chain.failNext("pool", errors.New("rpc down"))
		stMarkEpochFinalized(f.ctx, key, 8)
		if model.ClaimStEpochNotification(f.ctx, key, 8) {
			t.Fatal("an unknown cutoff did not keep v1's notification")
		}

		cfg := ledgerV2TestConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(cfg)
		stMarkEpochFinalized(f.ctx, key, 9)
		if model.ClaimStEpochNotification(f.ctx, key, 9) {
			t.Fatal("without settlement v2 an epoch's notification was not claimed")
		}
		if sent := emails.await(t, 0); len(sent) != 0 {
			t.Fatalf("emails %q", sent)
		}
	})
}

// SV-5 (v1 epochs keep v1): a v1 epoch's email takes each network's share
// from its committed leaves and its alpha from the pool total × share, and a
// network with a share but no wallet still gets the email.
func TestStEpochEarningsKeepV1ForV1Epochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		n1, n2 := server.NewId(), server.NewId()
		model.SetStPayoutLeaves(f.ctx, key, 4, 1, []*model.StPayoutLeaf{
			{Epoch: 4, NoId: 1, NetworkId: n1, Coldkey: ledgerV2Coldkey(1), ShareBps: 2500, LeafIndex: 0},
			{Epoch: 4, NoId: 1, NetworkId: n2, Coldkey: ledgerV2Coldkey(2), ShareBps: 7500, LeafIndex: 1},
		})
		row := &model.StEpoch{Epoch: 4, Status: model.StEpochStatusFinalized}
		// without an epoch state the window is estimated and nothing is cached
		stub := newStubStClient(nil)
		stub.pool.PoolTotalRao = big.NewInt(10_000_000)
		SetStClient(stub)
		t.Cleanup(func() { SetStClient(nil) })
		templates := stEpochEarningsTemplates(f.ctx, key, 4, row)
		if len(templates) != 2 || templates[n1].ShareBps != 2500 || templates[n1].UnclaimedRao.Cmp(big.NewInt(2_500_000)) != 0 || templates[n2].ShareBps != 7500 || templates[n2].UnclaimedRao.Cmp(big.NewInt(7_500_000)) != 0 {
			t.Fatalf("v1 templates %+v", templates)
		}
		// Without the pool total the share stays and the alpha is absent.
		stub.pool.PoolTotalRao = big.NewInt(0)
		templates = stEpochEarningsTemplates(f.ctx, key, 4, row)
		if len(templates) != 2 || templates[n1].ShareBps != 2500 || templates[n1].UnclaimedRao != nil {
			t.Fatalf("v1 templates without the pool total %+v", templates)
		}
	})
}

// SV-5: GET /account/epochs lists a v2 epoch once a finalized root covering it
// is recorded, with the network's amount of that epoch from the ledger and
// its share of the epoch's amounts ("0" for an epoch that carried), and lists
// v1 epochs as before, without an amount. A v1 finalization of a v2 epoch
// adds no second entry, and without settlement v2 the list is v1's.
func TestStLedgerV2AccountEpochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		networks := recordLedgerV2ThreeEpochRoot(t, f)
		key := f.cfg.DeploymentKey()
		for _, epoch := range []uint64{3, 4} {
			model.UpsertStEpoch(f.ctx, key, &model.StEpoch{Epoch: epoch, Status: model.StEpochStatusFinalized})
		}
		model.SetStPayoutLeaves(f.ctx, key, 4, 1, []*model.StPayoutLeaf{
			{Epoch: 4, NoId: 1, NetworkId: networks.n1, Coldkey: ledgerV2Coldkey(1), ShareBps: 2500, LeafIndex: 0},
			{Epoch: 4, NoId: 1, NetworkId: networks.n2, Coldkey: ledgerV2Coldkey(2), ShareBps: 7500, LeafIndex: 1},
		})
		model.SetStEpochStatus(f.ctx, key, 6, model.StEpochStatusFinalized)

		// Root 1, for epoch 9, covers epoch 8, which had no eligible usage and
		// carried, and epoch 9, whose amount went to D, selected by N1.
		d := ledgerV2Coldkey(4)
		for _, epoch := range []uint64{8, 9} {
			if err := model.RecordStLedgerV2EpochRows(f.ctx, key, 1, epoch, []string{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := model.AddStPayoutWalletResolutions(f.ctx, key, 9, 1, []*model.StPayoutWalletResolution{ledgerV2Resolution(server.Id{0x14}, networks.n1, d)}); err != nil {
			t.Fatal(err)
		}
		second := &model.StLedgerV2Root{NoId: 1, Seq: 9, Epoch: 9, Root: [32]byte{9}, Total: *uint256.NewInt(5200), LeafCount: 4, LedgerHash: [32]byte{9}}
		model.AddStLedgerV2Commit(f.ctx, key, second, f.chain.end(9)+10, 0)
		one := uint64(1)
		second.RootIndex = &one
		model.SetStLedgerV2RootFinalized(f.ctx, key, second, f.chain.end(10))
		if err := model.RecordStLedgerV2Root(f.ctx, key, 1, 9, 3, [][32]byte{d}, []*model.StLedgerV2Amount{{Epoch: 9, Coldkey: d, Amount: *uint256.NewInt(700)}}); err != nil {
			t.Fatal(err)
		}

		read := func(networkId server.Id, limit int) []AccountEpoch {
			t.Helper()
			result, err := AccountEpochs(&AccountEpochsArgs{Limit: limit}, session.Testing_CreateClientSession(f.ctx, &session.ByJwt{NetworkId: networkId}))
			if err != nil {
				t.Fatal(err)
			}
			return result.Epochs
		}
		shown := func(epochs []AccountEpoch) []string {
			out := []string{}
			for _, epoch := range epochs {
				out = append(out, fmt.Sprintf("%d %d %q", epoch.Epoch, epoch.ShareBps, epoch.AmountRao))
			}
			return out
		}
		for _, test := range []struct {
			networkId server.Id
			limit     int
			want      []string
		}{
			{networks.n1, 0, []string{`9 10000 "700"`, `8 0 "0"`, `7 10000 "500"`, `6 5000 "1500"`, `5 5000 "500"`, `4 2500 ""`, `3 0 ""`}},
			{networks.n2, 0, []string{`9 0 "0"`, `8 0 "0"`, `7 0 "0"`, `6 5000 "1500"`, `5 5000 "500"`, `4 7500 ""`, `3 0 ""`}},
			{networks.n1, 2, []string{`9 10000 "700"`, `8 0 "0"`}},
		} {
			if got := shown(read(test.networkId, test.limit)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("network %s limit %d: %q\nwant %q", test.networkId, test.limit, got, test.want)
			}
		}

		encoded, err := json.Marshal(read(networks.n1, 0))
		if err != nil {
			t.Fatal(err)
		}
		var decoded []map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, epoch := range decoded {
			_, amount := epoch["amount_rao"]
			if v2 := 5 <= epoch["epoch"].(float64); amount != v2 {
				t.Fatalf("epoch %v amount_rao present %t: %s", epoch["epoch"], amount, encoded)
			}
		}

		cfg := ledgerV2TestConfig()
		cfg.SettlementV2, cfg.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(cfg)
		if got := shown(read(networks.n1, 0)); !reflect.DeepEqual(got, []string{`6 0 ""`, `4 2500 ""`, `3 0 ""`}) {
			t.Fatalf("without settlement v2: %q", got)
		}
	})
}

// SV-5: an earnings email with a v2 amount offers the claim even when the
// amount's share of the epoch rounds to zero basis points.
func TestEpochEarningsTemplateWithAV2Amount(t *testing.T) {
	template := &EpochEarningsTemplate{Epoch: 7, Points: 1, HasWallet: true, UnclaimedRao: big.NewInt(1_500_000_000), EpochEnd: server.NowUtc()}
	if !template.HasUnclaimed() || template.AlphaText() != "1.5000 SN25α" || template.SharePercent() != "0.00%" {
		t.Fatalf("%t %s %s", template.HasUnclaimed(), template.AlphaText(), template.SharePercent())
	}
	_, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{bodyHtml, bodyText} {
		if !strings.Contains(body, "Unclaimed: ") || !strings.Contains(body, "1.5000 SN25α") || !strings.Contains(body, "Claim your SN25α") {
			t.Fatalf("v2 amount email %s", body)
		}
	}
	for _, template := range []*EpochEarningsTemplate{
		{HasWallet: true, UnclaimedRao: big.NewInt(0)},
		{HasWallet: true},
		{UnclaimedRao: big.NewInt(1)},
	} {
		if template.HasUnclaimed() {
			t.Fatalf("%+v has no claim", template)
		}
	}
}

// SV-5: a v2 epoch's share is the amount's share of the epoch's amounts in
// basis points, rounded down, and zero for an epoch without amounts.
func TestStLedgerV2ShareBps(t *testing.T) {
	for _, test := range []struct {
		amount, total uint64
		want          int
	}{
		{500, 1000, 5000}, {1, 3, 3333}, {1, 10_001, 0}, {1000, 1000, 10_000}, {0, 600, 0}, {0, 0, 0},
	} {
		if got := stLedgerV2ShareBps(uint256.NewInt(test.amount), uint256.NewInt(test.total)); got != test.want {
			t.Fatalf("%d of %d: %d bps, want %d", test.amount, test.total, got, test.want)
		}
	}
}

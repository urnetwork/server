package controller

// The epoch earnings email's sends (stSendEpochEarnings) on Postgres: an epoch whose earnings come from its payout
// leaves, as a v1 epoch's do, over a stub chain, so each earning network's email is built and sent to the addresses
// stEpochEarningsRecipients reads for it. team_earnings_mail_test.go runs the same with Teams beside personal networks
// (QD-1222); here, as in a binary that does not link MG6, every network is personal.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One earnings email as it was sent.
type epochEarningsTestSend struct {
	userAuth string
	template *EpochEarningsTemplate
}

// Records each earnings email in send order. A send to an address in failUserAuths fails, and onSend, when set, runs
// after each send.
type epochEarningsTestSender struct {
	stateLock     sync.Mutex
	sends         []epochEarningsTestSend
	failUserAuths map[string]bool
	onSend        func()
}

func (self *epochEarningsTestSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.failUserAuths[userAuth] {
			return errors.New("synthetic send failure")
		}
		self.sends = append(self.sends, epochEarningsTestSend{userAuth: userAuth, template: template.(*EpochEarningsTemplate)})
		return nil
	}()
	if self.onSend != nil {
		self.onSend()
	}
	return err
}

// The sends so far, in send order; later calls return only later sends.
func (self *epochEarningsTestSender) taken() []epochEarningsTestSend {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sends := self.sends
	self.sends = nil
	return sends
}

// The addresses of the sends, in send order.
func epochEarningsTestUserAuthsOf(sends []epochEarningsTestSend) []string {
	userAuths := []string{}
	for _, send := range sends {
		userAuths = append(userAuths, send.userAuth)
	}
	return userAuths
}

// An st configuration whose epochs settle as v1's do, a stub chain whose pool holds 10,000,000 rao, and settings that
// send without waiting.
type epochEarningsTestFixture struct {
	ctx      context.Context
	cfg      *StConfig
	settings *stEpochEarningsSettings
}

func newEpochEarningsTestFixture(t testing.TB) *epochEarningsTestFixture {
	t.Helper()
	cfg := &StConfig{
		Profile:         "testnet",
		DeploymentId:    "epoch-earnings-test",
		ChainId:         945,
		Enabled:         true,
		NoId:            1,
		BlockSeconds:    12,
		ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
	}
	SetStConfig(cfg)
	stub := newStubStClient(nil)
	stub.pool.PoolTotalRao = big.NewInt(10_000_000)
	SetStClient(stub)
	snTopDbResetChainState()
	resetLedgerV2Caches()
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStClient(nil)
		snTopDbResetChainState()
		resetLedgerV2Caches()
	})
	settings := defaultStEpochEarningsSettings()
	settings.SendInterval = 0
	return &epochEarningsTestFixture{
		ctx:      context.Background(),
		cfg:      cfg,
		settings: settings,
	}
}

// Finalizes the epoch, each of the networks holding a payout leaf of its share in basis points.
func (self *epochEarningsTestFixture) finalize(epoch uint64, networkIdShareBps map[server.Id]int) {
	model.UpsertStEpoch(self.ctx, self.cfg.DeploymentKey(), &model.StEpoch{Epoch: epoch, Status: model.StEpochStatusFinalized})
	networkIds := []server.Id{}
	for networkId := range networkIdShareBps {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	leaves := []*model.StPayoutLeaf{}
	for i, networkId := range networkIds {
		leaves = append(leaves, &model.StPayoutLeaf{
			Epoch:     epoch,
			NoId:      self.cfg.NoId,
			NetworkId: networkId,
			Coldkey:   ledgerV2Coldkey(byte(i + 1)),
			ShareBps:  networkIdShareBps[networkId],
			LeafIndex: i,
		})
	}
	model.SetStPayoutLeaves(self.ctx, self.cfg.DeploymentKey(), epoch, self.cfg.NoId, leaves)
}

// Sends the epoch's earnings emails with the fixture's settings.
func (self *epochEarningsTestFixture) send(epoch uint64, sender MessageSender) (sent int, skipped int) {
	return stSendEpochEarnings(self.ctx, self.cfg.DeploymentKey(), epoch, sender, self.settings)
}

// The claimable alpha of a share of the stub pool.
func epochEarningsTestAlpha(shareBps int) *big.Int {
	return big.NewInt(int64(10_000_000 * shareBps / 10_000))
}

// A new personal network whose admin signs in with a password at a synthetic address of its own, its network_user's
// sign-in. Returns the network, its admin and the address.
func newEpochEarningsTestNetwork(t testing.TB, ctx context.Context, label string) (networkId server.Id, userId server.Id, address string) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	suffix := strings.ReplaceAll(networkId.String(), "-", "")[16:]
	address = fmt.Sprintf("%s-%s@epoch-earnings.example", label, suffix)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			networkId,
			"ee-"+label+"-"+suffix,
			userId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type, user_auth, verified) VALUES ($1, 'person', $2, $3, true)`,
			userId,
			model.AuthTypePassword,
			address,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified) VALUES ($1, $2, 'email', true)`,
			userId,
			address,
		))
	})
	return
}

// Each earning personal network's email goes to its admin's sign-in alone, as it always did: not to the Google
// address its admin also signs in with. A network whose admin has no sign-in is skipped, and one that earned nothing in
// the epoch gets none. Each email carries its network's share and alpha.
func TestEpochEarningsReachEachPersonalNetworksAdmin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newEpochEarningsTestFixture(t)
		ctx := f.ctx
		firstNetworkId, firstUserId, firstAddress := newEpochEarningsTestNetwork(t, ctx, "first")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth) VALUES ($1, $2, 'synthetic', $3)`,
				firstUserId,
				model.AuthTypeGoogle,
				"first-google-"+strings.ReplaceAll(firstNetworkId.String(), "-", "")[16:]+"@epoch-earnings.example",
			))
		})
		secondNetworkId, _, secondAddress := newEpochEarningsTestNetwork(t, ctx, "second")
		silentNetworkId, silentUserId, _ := newEpochEarningsTestNetwork(t, ctx, "silent")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_user_auth_password WHERE user_id = $1`, silentUserId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET user_auth = NULL WHERE user_id = $1`, silentUserId))
		})
		// it holds no leaf in the epoch
		_, _, idleAddress := newEpochEarningsTestNetwork(t, ctx, "idle")
		f.finalize(4, map[server.Id]int{
			firstNetworkId:  2500,
			secondNetworkId: 5000,
			silentNetworkId: 2500,
		})

		sender := &epochEarningsTestSender{}
		sent, skipped := f.send(4, sender)
		if sent != 2 || skipped != 1 {
			t.Fatalf("sent %d and skipped %d, want 2 sent and the network whose admin has no sign-in skipped", sent, skipped)
		}
		sends := sender.taken()
		gotAddresses := epochEarningsTestUserAuthsOf(sends)
		slices.Sort(gotAddresses)
		wantAddresses := []string{firstAddress, secondAddress}
		slices.Sort(wantAddresses)
		if !slices.Equal(gotAddresses, wantAddresses) {
			t.Fatalf("emailed %v, want each earning network's admin sign-in once, %v", gotAddresses, wantAddresses)
		}
		for _, send := range sends {
			wantShareBps := 2500
			if send.userAuth == secondAddress {
				wantShareBps = 5000
			}
			if send.template.Epoch != 4 || send.template.ShareBps != wantShareBps || send.template.UnclaimedRao.Cmp(epochEarningsTestAlpha(wantShareBps)) != 0 {
				t.Fatalf("the email to %s: epoch %d, share %d, alpha %s; want epoch 4, share %d", send.userAuth, send.template.Epoch, send.template.ShareBps, send.template.UnclaimedRao, wantShareBps)
			}
			if send.userAuth == idleAddress {
				t.Fatal("a network that earned nothing was emailed")
			}
		}
	})
}

// The recipients are read in pages of the settings' RecipientReadNetworkCount networks, every page's networks emailed
// once whatever the page size, a non-positive one reading all at once. A failed send counts as skipped and the run
// goes on to the rest.
func TestEpochEarningsReadsRecipientsInPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := defaultStEpochEarningsSettings()
		if settings.SendInterval != 80*time.Millisecond || settings.SendTimeout != 2*time.Hour || settings.RecipientReadNetworkCount != 1000 {
			t.Fatalf("default settings %+v, want an 80 ms interval, a 2 hour bound and pages of 1,000 networks", settings)
		}
		f := newEpochEarningsTestFixture(t)
		ctx := f.ctx
		networkIdShareBps := map[server.Id]int{}
		addresses := []string{}
		for i := range 3 {
			networkId, _, address := newEpochEarningsTestNetwork(t, ctx, fmt.Sprintf("paged%d", i))
			networkIdShareBps[networkId] = 1000
			addresses = append(addresses, address)
		}
		slices.Sort(addresses)
		f.finalize(5, networkIdShareBps)

		for _, readNetworkCount := range []int{1, 2, 3, 1000, 0, -1} {
			f.settings.RecipientReadNetworkCount = readNetworkCount
			sender := &epochEarningsTestSender{}
			sent, skipped := f.send(5, sender)
			got := epochEarningsTestUserAuthsOf(sender.taken())
			slices.Sort(got)
			if sent != 3 || skipped != 0 || !slices.Equal(got, addresses) {
				t.Fatalf("pages of %d: sent %d, skipped %d, to %v; want each of %v once", readNetworkCount, sent, skipped, got, addresses)
			}
		}

		f.settings.RecipientReadNetworkCount = 2
		sender := &epochEarningsTestSender{failUserAuths: map[string]bool{addresses[1]: true}}
		sent, skipped := f.send(5, sender)
		got := epochEarningsTestUserAuthsOf(sender.taken())
		slices.Sort(got)
		if sent != 2 || skipped != 1 || !slices.Equal(got, []string{addresses[0], addresses[2]}) {
			t.Fatalf("with one send failing: sent %d, skipped %d, to %v; want the other two sent and the failure skipped", sent, skipped, got)
		}
	})
}

// A run whose context ends stops after the email it is sending, waiting or not.
func TestEpochEarningsStopAtTheRunsEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newEpochEarningsTestFixture(t)
		networkIdShareBps := map[server.Id]int{}
		for i := range 3 {
			networkId, _, _ := newEpochEarningsTestNetwork(t, f.ctx, fmt.Sprintf("ended%d", i))
			networkIdShareBps[networkId] = 1000
		}
		f.finalize(6, networkIdShareBps)

		for _, sendInterval := range []time.Duration{time.Hour, 0} {
			ctx, cancel := context.WithCancel(f.ctx)
			sender := &epochEarningsTestSender{onSend: cancel}
			settings := *f.settings
			settings.SendInterval = sendInterval
			sent, skipped := stSendEpochEarnings(ctx, f.cfg.DeploymentKey(), 6, sender, &settings)
			cancel()
			if sends := sender.taken(); sent != 1 || skipped != 0 || len(sends) != 1 {
				t.Fatalf("interval %s: sent %d, skipped %d, %d emails; want the run to stop after its first", sendInterval, sent, skipped, len(sends))
			}
		}
	})
}

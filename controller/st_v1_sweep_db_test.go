// The phase C sweep chain's admission and turns against the database: the
// recorded v2 activation and the publish rows that conclude the chain.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// sweepTurnClient serves one chain snapshot and records the sends.
type sweepTurnClient struct {
	sweepFakeClient
	chain  *StV1SweepChain
	latest uint64
}

func (self *sweepTurnClient) V1SweepChain(context.Context, uint64, uint64, uint64) (*StV1SweepChain, error) {
	copied := *self.chain
	return &copied, nil
}

func (self *sweepTurnClient) V1SweepLatestBlock(context.Context) (uint64, error) {
	return self.latest, nil
}

// The fixture's pool registers firstEpoch 5; the sweep epoch is 5.
func newSweepFixture(t testing.TB) (*ledgerV2Fixture, *StConfig) {
	f, _ := newLedgerV2ActivationFixture(t)
	cfg := *f.cfg
	cfg.V1SweepEpoch = 5
	f.restart(&cfg)
	return f, &cfg
}

// The sweep chain is admitted only for the configured epoch at or after the
// recorded first v2 epoch; the ordinary v1 driver still refuses that epoch.
func TestStV1SweepAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, cfg := newSweepFixture(t)
		if _, err := stV1SweepAdmission(f.ctx, cfg, 5); err == nil || !strings.Contains(err.Error(), "recorded v2 activation") {
			t.Fatalf("before the activation is recorded: %v", err)
		}
		observeLedgerV2Activation(t, f)
		activation, err := stV1SweepAdmission(f.ctx, cfg, 5)
		if err != nil || activation.FirstEpoch != 5 {
			t.Fatalf("the sweep epoch: %+v %v", activation, err)
		}
		if _, err := stV1SweepAdmission(f.ctx, cfg, 6); !errors.Is(err, ErrStV1EpochSettledByV2) {
			t.Fatalf("another epoch: %v", err)
		}
		early := *cfg
		early.V1SweepEpoch = 4
		if _, err := stV1SweepAdmission(f.ctx, &early, 4); err == nil || !strings.Contains(err.Error(), "precedes the pool's first v2 epoch") {
			t.Fatalf("a sweep below the first v2 epoch: %v", err)
		}
		none := *cfg
		none.V1SweepEpoch = 0
		if _, err := stV1SweepAdmission(f.ctx, &none, 5); !errors.Is(err, errStV1SweepNotConfigured) {
			t.Fatalf("no sweep configured: %v", err)
		}
		if allowed, err := StV1EpochAllowed(f.ctx, 5); err != nil || allowed {
			t.Fatalf("the ordinary driver admitted the sweep epoch: %t %v", allowed, err)
		}
	})
}

// A turn closes the configured epoch from the deposit signer and records it;
// a refusal concludes the chain, after which it is no longer due and a turn
// sends nothing.
func TestStV1SweepTurnClosesThenConcludesOnRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, cfg := newSweepFixture(t)
		observeLedgerV2Activation(t, f)
		alerts := []string{}
		priorAlert := stV1SweepAlertSink
		stV1SweepAlertSink = func(subject string, message string) { alerts = append(alerts, message) }
		defer func() { stV1SweepAlertSink = priorAlert }()

		chain := sweepTestChain()
		chain.Sweep.Epoch = 5
		client := &sweepTurnClient{chain: chain, latest: chain.HeadBlock + 2}
		SetStV1SweepClient(client)
		defer SetStV1SweepClient(nil)

		if epoch, due := StV1SweepDue(f.ctx); !due || epoch != 5 {
			t.Fatalf("due %d %t", epoch, due)
		}
		outcome, err := StV1SweepTurn(f.ctx)
		if err != nil || outcome.Status != model.StPublishStatusConfirmed || strings.Join(client.calls, ";") != "close 5 1 by deposit" {
			t.Fatalf("close turn: %+v %v calls %v", outcome, err, client.calls)
		}
		publishes := model.GetStPublishes(f.ctx, cfg.DeploymentKey(), 5)
		if len(publishes) != 1 || publishes[0].Kind != StPublishKindV1SweepClose || publishes[0].Status != model.StPublishStatusConfirmed {
			t.Fatalf("publishes %+v", publishes)
		}

		client.chain.Sweep.Root = [32]byte{2}
		outcome, err = StV1SweepTurn(f.ctx)
		if err != nil || outcome.Retry || len(alerts) != 1 || !strings.Contains(alerts[0], "not the single vault2 leaf") {
			t.Fatalf("refusal turn: %+v %v alerts %v", outcome, err, alerts)
		}
		if _, due := StV1SweepDue(f.ctx); due {
			t.Fatal("a refused sweep is still due")
		}
		calls := len(client.calls)
		outcome, err = StV1SweepTurn(f.ctx)
		if err != nil || outcome.Retry || !strings.Contains(outcome.Reason, "concluded") || len(client.calls) != calls {
			t.Fatalf("concluded turn: %+v %v calls %v", outcome, err, client.calls)
		}
	})
}

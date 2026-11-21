package sidecart3

// evs served at a T3 world's evs origin (world_networks.go): server/cli/evs's block, evshost.NewHost, built as the evs
// tier builds it (evs_tier.go), on the world's host on real time, with the seams its main gives it from the block's own
// process rather than a fault world's: a gate cache of its own over the log server's sequencer writes, the network's
// newest verified state as AuthState (evsservice.NewAclEvsAuthState with a rev/ reader) and GateViews
// (evsservice.NewAclGateViews), as evshost's own block builds them (host.go's newBlockEngine), and upload receipt keys
// the world makes, never vault's. Its instance format reaches it through the block's config standing in for evs.yml,
// as an operator's does (QD-854). The host's Quota, Push, ClockCheck, Budget, Abuse and Gates are the Net's fakes, as
// on a T3 Net's evs tier; its gate cache is one more the world's syncs wait on, as api's {acl_}v event moves a block's.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/oauth"
)

// What a world's evs runs with.
type T3EvsSettings struct {
	// The block's storage format, which its config names: format 1, as M2's rows and enginenet's world run it.
	InstanceFormat uint32
	// The engine's settings past their defaults, nil for none. Its InstanceFormat is the config's, whatever this sets.
	AdjustEngine func(settings *evsservice.EvsSettings)
	// How long before now the receipt key issues from: 48 h, so it is the current key from the start.
	ReceiptKeyAge time.Duration
	// How long, in real time, the block may take to read ready: 60 s, only a broken start's bound.
	ReadyTimeout time.Duration
}

func DefaultT3EvsSettings() *T3EvsSettings {
	return &T3EvsSettings{
		InstanceFormat: sidecar.EvsRepoFormat1,
		ReceiptKeyAge:  48 * time.Hour,
		ReadyTimeout:   60 * time.Second,
	}
}

// One evs block a world serves at its evs origin.
type T3Evs struct {
	world *T3World
	host  *evshost.Host
	gate  *t3WorldGate
}

// Starts evs on the world's host and serves it at the world's evs origin until the returned block's Close or the
// world's. settings nil takes the defaults.
func (self *T3World) ServeEvs(ctx context.Context, settings *T3EvsSettings) (served *T3Evs, returnErr error) {
	if settings == nil {
		settings = DefaultT3EvsSettings()
	}
	gate, err := self.addGate("evs")
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnErr != nil {
			self.removeGate(gate)
			gate.close()
		}
	}()
	revocationSettings := sidecar.DefaultRevocationSettings()
	revocationSettings.Clock = self.host.Clock
	checker := sidecar.NewRevocationChecker(self.host.Blobs, oauth.VerifyRevocation, revocationSettings)
	receiptKey := make([]byte, 32)
	if _, err := rand.Read(receiptKey); err != nil {
		return nil, err
	}
	receiptKeys, err := evsservice.NewReceiptKeys([]*evsservice.ReceiptKey{
		{KeyId: 1, Key: receiptKey, IssueFrom: self.host.ClockOrReal().Now().Add(-settings.ReceiptKeyAge)},
	}, nil)
	if err != nil {
		return nil, err
	}

	engineSettings := evsservice.DefaultEvsSettings()
	if settings.AdjustEngine != nil {
		settings.AdjustEngine(engineSettings)
	}
	// the format goes through the block's config alone, as on the evs tier
	engineSettings.InstanceFormat = 0
	hostSettings := evshost.DefaultHostSettings()
	hostSettings.Evs = engineSettings
	var evsHost *evshost.Host
	evshost.Testing_WithBlockConfig(evsTierBlockConfig(settings.InstanceFormat), func() {
		evsHost, err = evshost.NewHost(self.ctx, self.host, hostSettings, &evshost.HostDeps{
			AuthState:   evsservice.NewAclEvsAuthState(gate.cache, checker),
			GateViews:   evsservice.NewAclGateViews(gate.cache),
			ReceiptKeys: receiptKeys,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("the evs block: %w", err)
	}
	if evsHost.InstanceFormat() != settings.InstanceFormat {
		evsHost.Close()
		return nil, fmt.Errorf("the evs block runs as instance format %d under a config naming %d", evsHost.InstanceFormat(), settings.InstanceFormat)
	}
	if err := awaitT3EvsReady(ctx, evsHost, settings.ReadyTimeout); err != nil {
		evsHost.Close()
		return nil, err
	}
	if err := self.api.ServeService("evs", evsHost.Handler()); err != nil {
		evsHost.Close()
		return nil, err
	}
	served = &T3Evs{world: self, host: evsHost, gate: gate}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		served.closeServed()
		return nil, errors.New("the world has closed")
	}
	self.closes = append(self.closes, served.closeServed)
	return served, nil
}

// The block's first probe answered and it reads ready, as warp sends a block nothing until its /status reads ready
// (§19.4). timeout bounds the wait in real time only to fail it.
func awaitT3EvsReady(ctx context.Context, evsHost *evshost.Host, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		count, changed := evsHost.Engine().Probes()
		if 0 < count && evsHost.Ready() {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("the evs block never read ready, missing %v: %w", evsHost.Missing(), ctx.Err())
		}
	}
}

// The block, for a test that reads its engine.
func (self *T3Evs) Host() *evshost.Host {
	return self.host
}

// Stops serving the origin, then drains and closes the block and its gate cache.
func (self *T3Evs) closeServed() {
	self.world.api.ServeService("evs", nil)
	self.host.Drain()
	self.host.Close()
	self.world.removeGate(self.gate)
	self.gate.close()
}

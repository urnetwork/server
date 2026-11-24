package sidecart3

// gkm served at a T3 world's gkm origin (world_networks.go), which a world on its real keys needs beside evs
// (world_evs.go): sidecar's gkmservice engine on the world's host on real time, built as server/gkm's block builds it
// (gkm.NewHost over gkm.NewAuthState), with the seams its main gives it made here from the world's own parts rather than
// a block's: a gate cache of its own over the log server's sequencer writes, which the world's syncs wait on as api's
// {acl_}v event moves a block's, and gkmservice.AclGkmAuthState over it with a rev/ reader verifying under the world's
// svc- key, the host's Authenticator verifying service tokens under that key too (HostDeps.ServiceTokens' way). The
// host's Blobs are the Net's file clusters on the local stack's MinIO, and its Budget, Quota, Gates and Push the Net's
// fakes, as evs's are: gkm's hints go to the fake Push, which reaches none of the world's devices, so their resync
// stands in for the hints (T3WorldSettings.KeyResyncInterval). The engine logs through glog, as the block does.
//
// server/gkm's own T3 tests import this package, so the engine is built here from gkmservice, as gkm.NewHost builds
// it, rather than through server/gkm.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"

	"github.com/urnetwork/server/sidecarhost"
)

// What a world's gkm runs with.
type T3GkmSettings struct {
	// The engine's settings past their defaults, nil for none.
	AdjustEngine func(settings *gkmservice.GkmServiceSettings)
	// How long, in real time, the block may take to read ready: 60 s, only a broken start's bound.
	ReadyTimeout time.Duration
}

func DefaultT3GkmSettings() *T3GkmSettings {
	return &T3GkmSettings{
		ReadyTimeout: 60 * time.Second,
	}
}

// One gkm block a world serves at its gkm origin.
type T3Gkm struct {
	world  *T3World
	engine *gkmservice.GkmEngine
	gate   *t3WorldGate
}

// Starts gkm on the world's host and serves it at the world's gkm origin until the world's Close. settings nil takes
// the defaults. A world of hosts that serves its own gkm block at the origin (sidecare2e's World) calls none.
func (self *T3World) ServeGkm(ctx context.Context, settings *T3GkmSettings) (served *T3Gkm, returnErr error) {
	if settings == nil {
		settings = DefaultT3GkmSettings()
	}
	gate, err := self.addGate("gkm")
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
	revocations := sidecar.NewRevocationChecker(self.host.Blobs, sidecarhost.RevocationVerify(self.serviceTokens), revocationSettings)
	engineSettings := gkmservice.DefaultGkmServiceSettings()
	if settings.AdjustEngine != nil {
		settings.AdjustEngine(engineSettings)
	}
	engine, err := gkmservice.NewGkmEngine(self.ctx, self.host, engineSettings, &gkmservice.GkmEngineDeps{
		AuthState: gkmservice.NewAclGkmAuthState(gate.cache, revocations, self.host.Clock),
		Log:       t3GkmLogger{},
	})
	if err != nil {
		return nil, fmt.Errorf("the gkm block: %w", err)
	}
	if err := awaitT3GkmReady(ctx, engine, settings.ReadyTimeout); err != nil {
		engine.Close()
		return nil, err
	}
	if err := self.api.ServeService("gkm", engine.Handler()); err != nil {
		engine.Close()
		return nil, err
	}
	served = &T3Gkm{world: self, engine: engine, gate: gate}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closes = append(self.closes, served.closeServed)
		return true
	}()
	if !kept {
		// closed with no lock held, since closing takes the world's
		served.closeServed()
		return nil, errors.New("the world has closed")
	}
	return served, nil
}

// The engine's first probe answered and it reads ready, as warp sends a block nothing until its /status reads ready
// (§19.4). timeout bounds the wait in real time only to fail it.
func awaitT3GkmReady(ctx context.Context, engine *gkmservice.GkmEngine, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		count, changed := engine.Probes()
		if 0 < count && engine.Ready() {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("the gkm block never read ready, its first metadata cluster unanswered: %w", ctx.Err())
		}
	}
}

// The engine, for a test that reads it.
func (self *T3Gkm) Engine() *gkmservice.GkmEngine {
	return self.engine
}

// Stops serving the origin, then drains and closes the engine and its gate cache. Called with no lock of the world's
// held, once.
func (self *T3Gkm) closeServed() {
	self.world.api.ServeService("gkm", nil)
	self.engine.Drain()
	self.engine.Close()
	self.world.removeGate(self.gate)
	self.gate.close()
}

// Whether a handler serves service's origin now (T3Api.ServeService).
func (self *T3Api) servesService(service string) bool {
	host, err := self.serviceHost(service)
	if err != nil {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, served := self.serviceHostHandlers[host]
	return served
}

// glog as the world's gkm engine's sidecar.Logger, as server/gkm's block logs.
type t3GkmLogger struct{}

func (self t3GkmLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self t3GkmLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self t3GkmLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self t3GkmLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self t3GkmLogger) V(level int32) sidecar.Verbose {
	return t3GkmVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the engine's sidecar.Verbose.
type t3GkmVerbose glog.Verbose

func (self t3GkmVerbose) Enabled() bool {
	return bool(self)
}

func (self t3GkmVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self t3GkmVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}

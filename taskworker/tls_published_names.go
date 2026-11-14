package taskworker

// §21.6's published names in taskworker (QD-770, question tls-m5-16-published-names-enumeration): the source
// model.ListTlsPublishedNames asks for each network's grants, over this process's one gate cache (C-09), the cache
// gkm's reconciler and evs's lifecycle jobs read, which gkmReconcilerStateFromHost builds for each runtime with an evs
// host (gkm_reconciler.go). A network's grants are sidecar.AclState.PublishedNames of its newest verified state, so the
// audience and expiry the check reads are those the gates enforce, by one reading of the rules, each joined to the
// names the network serves it under (model.TlsPublishedNamesOf).
//
// The cache holds a network loaded only from its first {acl_}v event or ask (question
// gkm-auth-state-reconciler-networks), so after a restart a quiet network is not loaded. The source never answers
// no names for a state it has not read: it asks the cache to load the network and waits for the install, at most
// LoadWait, and a network still not loaded then fails the page, which the check retries. A process whose runtime holds
// no cache installs no source, and the list answers unavailable, which the check treats as a failed pass.
//
// Each pass that reaches a quiet network cold-loads it, so an hourly pass loads every network that has a log, one at a
// time, within the cache's ByteBudget and LoadConcurrency (§8.18's fan-out risk); the networks it loads are then
// gkm's reconciler's too.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A network the gate cache did not load within LoadWait: the page fails, and the check retries.
var errTlsPublishedNamesNotLoaded = errors.New("the gate cache holds no verified state of the network")

// What the source reads of the process's gate cache: sidecar.AclGateCache's methods.
type tlsPublishedNamesStates interface {
	// The network's verified state, with no I/O.
	State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus)
	// Asks for the network's load, without blocking.
	Changed(networkId sidecar.Id)
	// Runs callback after each new snapshot is installed, until the returned call.
	AddInstallCallback(callback sidecar.AclGateInstallFunction) (remove func())
}

// Tunables of the published-names source.
type tlsPublishedNamesSourceSettings struct {
	// How long the source waits for the cache to load a network it does not hold before it fails the page: the
	// cache's LoadTimeout and one FailureBackoff, so a load that runs to its deadline still lands, 35 s. Non-positive
	// waits none, so a network not loaded fails the page at once.
	LoadWait time.Duration
}

func defaultTlsPublishedNamesSourceSettings() *tlsPublishedNamesSourceSettings {
	gateSettings := sidecar.DefaultAclGateSettings()
	return &tlsPublishedNamesSourceSettings{
		LoadWait: gateSettings.LoadTimeout + gateSettings.FailureBackoff,
	}
}

// The source over one runtime's gate cache. Safe for concurrent use.
type tlsPublishedNamesSource struct {
	states   tlsPublishedNamesStates
	settings *tlsPublishedNamesSourceSettings
}

func newTlsPublishedNamesSource(states tlsPublishedNamesStates, settings *tlsPublishedNamesSourceSettings) *tlsPublishedNamesSource {
	if settings == nil {
		settings = defaultTlsPublishedNamesSourceSettings()
	}
	return &tlsPublishedNamesSource{states: states, settings: settings}
}

// See model.TlsPublishedNamesSource: the network's state's published names at now, under each name it serves them.
func (self *tlsPublishedNamesSource) names(ctx context.Context, networkId server.Id, now time.Time) ([]*model.TlsPublishedName, error) {
	snapshot, err := self.state(ctx, sidecar.Id(networkId))
	if err != nil {
		return nil, err
	}
	return model.TlsPublishedNamesOf(ctx, networkId, snapshot.Acl.PublishedNames(now)), nil
}

// The network's loaded state, asking the cache to load it when it holds none and waiting for the install.
func (self *tlsPublishedNamesSource) state(ctx context.Context, networkId sidecar.Id) (*sidecar.AclGateSnapshot, error) {
	loaded := func() *sidecar.AclGateSnapshot {
		snapshot, status := self.states.State(networkId)
		if status != sidecar.AclGateLoaded || snapshot == nil || snapshot.Acl == nil {
			return nil
		}
		return snapshot
	}
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	installed := make(chan struct{})
	var installOnce sync.Once
	// registered before the ask, so an install the ask starts is never missed
	remove := self.states.AddInstallCallback(func(installedNetworkId sidecar.Id, snapshot *sidecar.AclGateSnapshot) {
		if installedNetworkId == networkId {
			installOnce.Do(func() {
				close(installed)
			})
		}
	})
	defer remove()
	self.states.Changed(networkId)
	// an install between the first read and the registration is in the state now
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	if 0 < self.settings.LoadWait {
		select {
		case <-installed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(self.settings.LoadWait):
		}
	}
	if snapshot := loaded(); snapshot != nil {
		return snapshot, nil
	}
	return nil, fmt.Errorf("%w: %s", errTlsPublishedNamesNotLoaded, networkId)
}

// The source the newest runtime installed. Installs and removals take the lock, so a runtime's end removes only its
// own source, never a newer runtime's.
var tlsPublishedNamesInstall = struct {
	stateLock sync.Mutex
	current   *tlsPublishedNamesSource
}{}

// Installs a source over states as the process's (model.SetTlsPublishedNamesSource), and answers what removes it
// unless a newer runtime's has replaced it.
func installTlsPublishedNamesSource(states tlsPublishedNamesStates) (remove func()) {
	return installTlsPublishedNamesSourceWithSettings(states, nil)
}

// installTlsPublishedNamesSource with the source's settings, nil for the defaults.
func installTlsPublishedNamesSourceWithSettings(states tlsPublishedNamesStates, settings *tlsPublishedNamesSourceSettings) (remove func()) {
	source := newTlsPublishedNamesSource(states, settings)
	tlsPublishedNamesInstall.stateLock.Lock()
	defer tlsPublishedNamesInstall.stateLock.Unlock()
	tlsPublishedNamesInstall.current = source
	model.SetTlsPublishedNamesSource(source.names)
	return func() {
		tlsPublishedNamesInstall.stateLock.Lock()
		defer tlsPublishedNamesInstall.stateLock.Unlock()
		if tlsPublishedNamesInstall.current == source {
			tlsPublishedNamesInstall.current = nil
			model.SetTlsPublishedNamesSource(nil)
		}
	}
}

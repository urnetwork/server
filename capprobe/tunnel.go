// The production capability tunnel over providertunnel, and the
// platform-local resolver of a peer's identity key.
package capprobe

import (
	"context"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// A capability tunnel carries pings and one handshake, a few KiB, so it
// reserves what a blackhole check reserves.
const tunnelContractReservationByteCount = connect.ByteCount(1024 * 1024)

// The peers one tunnel's recorder holds: its provider, with room to spare.
const tunnelMaxObservedPeerCount = 8

// One provider's open capability tunnel and the recorder its clients record
// into, which belongs to this measurement alone.
type providerCapabilityTunnel struct {
	tunnel     *providertunnel.Tunnel
	recorder   *connect.PeerCapabilityRecorder
	providerId connect.Id
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) observation() (connect.PeerCapabilityObservation, bool) {
	return self.recorder.Get(self.providerId)
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) lost() <-chan struct{} {
	return self.tunnel.Lost().Done()
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) close() error {
	return self.tunnel.Close()
}

// The provider tunnel constructor, a package-level seam like fleetprobe's:
// immutable in production, replaced by tests that must not open a tunnel.
var openProviderTunnel = providertunnel.Open

// Opens tunnels from base, the pass's identity, endpoints, credentials and
// control, each with a recorder and a platform transport budget of its own.
// The tunnel's window pings its provider (no data-only probe), its contracts
// reserve little, and its teardown has the configured grace. The pass's
// tunnels share nothing else, and nothing with the URL probe's.
func newProviderCapabilityTunnelOpener(base providertunnel.Config, settings Settings) openCapabilityTunnel {
	return func(ctx context.Context, providerId server.Id) (capabilityTunnel, error) {
		recorder := connect.NewPeerCapabilityRecorder(tunnelMaxObservedPeerCount)
		cfg := base
		cfg.PlatformTransportBudget = newTunnelTransportBudget(base.PlatformTransportBudget)
		cfg.DataOnlyProbe = false
		cfg.ContractReservationByteCount = tunnelContractReservationByteCount
		cfg.CloseTimeout = settings.closeTimeout()
		cfg.CapabilityObservation = &providertunnel.CapabilityObservation{
			Recorder:                      recorder,
			NewPeerClientPublicKeyFetcher: newLocalPeerClientPublicKeyFetcher,
			// a measurement attempts its provider about once
			RedialMin: settings.measureTimeout(),
		}
		tunnel, err := openProviderTunnel(ctx, cfg, connect.Id(providerId))
		if err != nil {
			return nil, err
		}
		return &providerCapabilityTunnel{
			tunnel:     tunnel,
			recorder:   recorder,
			providerId: connect.Id(providerId),
		}, nil
	}
}

// One tunnel's platform transport budget: a new one with the limits of
// limitsFrom, or of connect's defaults without one, and never limitsFrom
// itself. Each tunnel has its own, as each URL probe's does
// (fleetprobe.providerTunnelOpener): a budget the pass's tunnels shared would
// hold its default sixteen carriers for them all, and above that many at a
// time a tunnel would wait for a carrier, its derived client minted and its
// measurement's time running.
func newTunnelTransportBudget(limitsFrom *connect.PlatformTransportBudget) *connect.PlatformTransportBudget {
	if limitsFrom == nil {
		limitsFrom = connect.DefaultPlatformTransportSettings().PlatformTransportBudget
	}
	limits := limitsFrom.Stats()
	return connect.NewPlatformTransportBudget(limits.TotalByteCount, limits.MaxTransportCount)
}

// The platform's read of a client's published identity key. A package-level
// seam like the URL probe's: immutable in production, replaced by the
// resolver's test.
var getClientPublicKey = model.GetClientPublicKey

// A session's resolver of its peer's published identity key, read from the
// platform's own store: the taskworker is the platform, so this is what the
// unauthenticated /key/<client_id> route would answer, without the request.
// A session runs it on a worker of its own, never on the packet path. A peer
// that never published a key reads as no key and no error; a failed read,
// raised or returned, is an error, which the session treats as no key yet.
func newLocalPeerClientPublicKeyFetcher(peerId connect.Id) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) (publicKey []byte, returnErr error) {
		server.HandleError(func() {
			publicKey, returnErr = getClientPublicKey(ctx, server.Id(peerId))
		}, func(err error) {
			publicKey, returnErr = nil, err
		})
		return
	}
}

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

// The peers one tunnel's observer records: its provider, with room to spare.
const tunnelMaxObservedPeerCount = 8

// One provider's open capability tunnel and the observer its clients record
// into, which belongs to this measurement alone.
type providerCapabilityTunnel struct {
	tunnel       *providertunnel.Tunnel
	observations *connect.PeerCapabilityObservations
	providerId   connect.Id
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) observation() (connect.PeerCapabilityObservation, bool) {
	return self.observations.Get(self.providerId)
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) lost() <-chan struct{} {
	return self.tunnel.Lost().Done()
}

// Implements capabilityTunnel.
func (self *providerCapabilityTunnel) close() error {
	return self.tunnel.Close()
}

// Opens tunnels from base, the pass's identity, endpoints, credentials,
// control and transport budget, each with an observer of its own. The
// tunnel's window pings its provider (no data-only probe), its contracts
// reserve little, and its teardown has the configured grace. The pass's
// tunnels share nothing else, and nothing with the URL probe's.
func newProviderCapabilityTunnelOpener(base providertunnel.Config, settings Settings) openCapabilityTunnel {
	return func(ctx context.Context, providerId server.Id) (capabilityTunnel, error) {
		observations := connect.NewPeerCapabilityObservations(tunnelMaxObservedPeerCount)
		cfg := base
		cfg.DataOnlyProbe = false
		cfg.ContractReservationByteCount = tunnelContractReservationByteCount
		cfg.CloseTimeout = settings.closeTimeout()
		cfg.CapabilityObservation = &providertunnel.CapabilityObservation{
			Observations:                  observations,
			NewPeerClientPublicKeyFetcher: newLocalPeerClientPublicKeyFetcher,
		}
		tunnel, err := providertunnel.Open(ctx, cfg, connect.Id(providerId))
		if err != nil {
			return nil, err
		}
		return &providerCapabilityTunnel{
			tunnel:       tunnel,
			observations: observations,
			providerId:   connect.Id(providerId),
		}, nil
	}
}

// A session's resolver of its peer's published identity key, read from the
// platform's own store: the taskworker is the platform, so this is what the
// unauthenticated /key/<client_id> route would answer, without the request.
// A session runs it on a worker of its own, never on the packet path; a
// failed read is an error, which the session treats as no key yet.
func newLocalPeerClientPublicKeyFetcher(peerId connect.Id) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) (publicKey []byte, returnErr error) {
		server.HandleError(func() {
			publicKey, returnErr = model.GetClientPublicKey(ctx, server.Id(peerId))
		}, func(err error) {
			publicKey, returnErr = nil, err
		})
		return
	}
}

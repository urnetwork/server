package alt

import (
	"context"
	"slices"
	"testing"
	"time"

	connectcore "github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

// Each listener names its carrier to the connect handler, so a resident knows
// whether its client came over plain QUIC or under the dns translation, and
// the mover moves only clients on direct carriers (connect/MOVE.md §5.5).
func TestConnectCarrierForListener(t *testing.T) {
	connectcore.AssertEqual(t, connectserver.ResidentCarrierH3, connectCarrierForListener(ListenerTransportH3))
	connectcore.AssertEqual(t, connectserver.ResidentCarrierH3Dns, connectCarrierForListener(ListenerTransportWhodis))
	connectcore.AssertEqual(t, connectserver.ResidentCarrierUnknown, connectCarrierForListener("other"))
}

// A platform transport through alt's H3 listener reaches its resident as H3,
// and one through the whodis listener as H3 under dns.
func TestAltTellsTheResidentEachListenersCarrier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, mode := range []struct {
			transportMode connectcore.TransportMode
			carrier       connectserver.ResidentCarrier
		}{
			{connectcore.TransportModeH3, connectserver.ResidentCarrierH3},
			{connectcore.TransportModeH3Dns, connectserver.ResidentCarrierH3Dns},
		} {
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				env := testing_newAltEnv(ctx, t, nil)
				defer env.Close()

				clientId, instanceId, byClientJwt := env.authClient()
				routeManager := connectcore.NewRouteManager(ctx, clientId.String())
				transport := env.newTransport(byClientJwt, instanceId, routeManager, mode.transportMode)
				defer transport.Close()

				carriers := func() []connectserver.ResidentCarrier {
					candidates := env.exchange.ResidentMoveCandidates()
					index := slices.IndexFunc(candidates, func(candidate connectserver.ResidentMoveCandidate) bool {
						return candidate.ClientId == clientId
					})
					if index < 0 {
						return nil
					}
					carriers := []connectserver.ResidentCarrier{}
					for _, edge := range candidates[index].Transports {
						carriers = append(carriers, edge.Carrier)
					}
					return carriers
				}
				deadline := time.Now().Add(testProgressTimeout)
				for len(carriers()) == 0 {
					if deadline.Before(time.Now()) {
						t.Fatalf("%s transport reached no resident", mode.transportMode)
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(50 * time.Millisecond):
					}
				}
				connectcore.AssertEqual(t, []connectserver.ResidentCarrier{mode.carrier}, carriers())
			}()
		}
	})
}

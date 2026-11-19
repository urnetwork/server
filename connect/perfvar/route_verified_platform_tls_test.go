// The fixture proof observes verified H1 and H3 handshakes on their simulated platform edge.
package perfvar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Inspect completed handshakes on the same simulated listeners the real clients use.
func TestRoutePlatformTlsVerifiesTheOwnedEdge(t *testing.T) {
	environmentSettings := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environmentSettings.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		environment := newRouteEnvironment(ctx, t, initialNetworkProfiles(3001)["clean-lan"])
		defer environment.close()
		// Capture the first completed verification on each forced carrier.
		type handshake struct {
			state   tls.ConnectionState
			roots   *x509.CertPool
			skipped bool
		}
		observations := map[clientconnect.TransportMode]chan handshake{
			clientconnect.TransportModeH1: make(chan handshake, 1),
			clientconnect.TransportModeH3: make(chan handshake, 1),
		}
		environment.observePlatformTls = func(mode clientconnect.TransportMode, config *tls.Config) {
			previous := config.VerifyConnection
			roots := config.RootCAs
			skipped := config.InsecureSkipVerify
			config.VerifyConnection = func(state tls.ConnectionState) error {
				if previous != nil {
					if err := previous(state); err != nil {
						return err
					}
				}
				select {
				case observations[mode] <- handshake{state: state, roots: roots, skipped: skipped}:
				default:
				}
				return nil
			}
		}

		otherConfig, _, err := newWorkloadTlsConfigs()
		if err != nil {
			t.Fatal(err)
		}
		otherCertificate, err := x509.ParseCertificate(otherConfig.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		otherRoots := x509.NewCertPool()
		otherRoots.AddCert(otherCertificate)
		for _, mode := range []clientconnect.TransportMode{clientconnect.TransportModeH1, clientconnect.TransportModeH3} {
			client := environment.newClient("verified-platform-"+string(mode), clientconnect.P2pDataPlaneModeFastOnly, nil, false)
			environment.connectPlatform(client, mode)
			if !waitForPlatform(ctx, client.transport) {
				t.Fatalf("%s platform did not authenticate", mode)
			}
			var observed handshake
			select {
			case observed = <-observations[mode]:
			case <-ctx.Done():
				t.Fatalf("%s handshake observation: %v", mode, ctx.Err())
			}
			if observed.skipped || observed.roots == nil || len(observed.state.VerifiedChains) != 1 || len(observed.state.PeerCertificates) != 1 {
				t.Errorf("%s platform handshake requires the verified edge certificate: skipped=%t chains=%d peer certificates=%d", mode, observed.skipped, len(observed.state.VerifiedChains), len(observed.state.PeerCertificates))
				continue
			}
			leaf := observed.state.PeerCertificates[0]
			onlyThisLeaf := x509.NewCertPool()
			onlyThisLeaf.AddCert(leaf)
			if !observed.roots.Equal(onlyThisLeaf) {
				t.Errorf("%s platform trusted a different root pool", mode)
			}
			serverName := environment.edgeAddress.String()
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: serverName, Roots: observed.roots}); err != nil {
				t.Errorf("%s edge certificate for %s: %v", mode, serverName, err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: serverName, Roots: otherRoots}); err == nil {
				t.Errorf("%s accepted an unrelated root", mode)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "wrong-edge.invalid", Roots: observed.roots}); err == nil {
				t.Errorf("%s accepted a different edge hostname", mode)
			}
		}
	})
}

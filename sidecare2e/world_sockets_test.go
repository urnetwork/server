//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's socket and TLS edge helpers (world_test.go): a device's socket component, as ur daemon starts it once its
// follower stands, §16.6's approval gate behind its Authorize, a network verified and its device's certificate row,
// and a visitor that reaches a published tls/ name through tlsedge's direct port as warp's lb hands one over, behind
// the lb's PROXY v1 header, completing TLS with the device's own key.

package sidecare2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// What the socket and TLS edge helpers run with (QD-702).
type WorldTlsSettings struct {
	// The visitor the lb's PROXY header names, a documentation address.
	VisitorAddress netip.AddrPort
	// How long one visit may take: its dial, the PROXY header, the handshake and one exchange.
	VisitTimeout time.Duration
	// How often a visit is tried again while its name is not served yet, and the bound on that wait: sockets takes a
	// registration at its verdict and tlsedge reaches it at its Find, with no hook a test reads.
	RetryInterval time.Duration
	Timeout       time.Duration
	// The paid balance a network is verified by: its bytes, its net revenue and its window.
	VerifiedByteCount           int64
	VerifiedNetRevenueNanoCents int64
	VerifiedDuration            time.Duration
	// The certificate row's validity on either side of now: inside a 45-day certificate's.
	CertificateValidity time.Duration
	// The most bytes a listener reads of one visit's request line.
	RequestMaxByteCount int
}

func DefaultWorldTlsSettings() *WorldTlsSettings {
	return &WorldTlsSettings{
		VisitorAddress:              netip.MustParseAddrPort("198.51.100.7:50443"),
		VisitTimeout:                10 * time.Second,
		RetryInterval:               250 * time.Millisecond,
		Timeout:                     90 * time.Second,
		VerifiedByteCount:           1024 * 1024 * 1024,
		VerifiedNetRevenueNanoCents: 1_000_000_000,
		VerifiedDuration:            24 * time.Hour,
		CertificateValidity:         24 * time.Hour,
		RequestMaxByteCount:         4096,
	}
}

// Starts device's socket component, as ur daemon does once its follower stands (cli's startUrConfigFollower):
// Sidecar.NewSocketComponent and AddComponent, what its Listen and Dial run on. The sidecar removes it at its close.
func (self *World) StartSockets(ctx context.Context, device *sidecart3.T3Device) error {
	component, err := device.Sidecar().NewSocketComponent(ctx)
	if err != nil {
		return fmt.Errorf("%s's socket component: %w", device.Name, err)
	}
	if _, err := device.Sidecar().AddComponent(component); err != nil {
		component.Close()
		return fmt.Errorf("%s's socket component: %w", device.Name, err)
	}
	return nil
}

// Installs §16.6's approval gate behind device's Authorize, as ur daemon does over its follower (cli's
// bridge_filing_config.go): its standing grants the follower's ACL engine, its states the follower's verified state
// and local policy, reconciled once, its inbox polled until the world closes.
func (self *World) InstallApprovalGate(ctx context.Context, device *sidecart3.T3Device) (*sidecar.ApprovalGate, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil || follower.AclEngine() == nil {
		return nil, fmt.Errorf("%s gates its actions by a follower that stands", device.Name)
	}
	client := follower.Client()
	gate, remove, err := device.Sidecar().InstallApprovalGate(&sidecar.ApprovalGateSources{
		Grants: follower.AclEngine(),
		States: func() (sidecar.ApprovalStates, error) {
			if follower.Client() != client || client.Fork() != nil {
				return nil, sidecar.ErrApprovalNoState
			}
			return sidecar.NewConfigApprovalStates(client.State(), false, follower.AclLocal())
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%s's approval gate: %w", device.Name, err)
	}
	if _, err := gate.Reconcile(ctx); err != nil {
		remove()
		return nil, fmt.Errorf("%s's approval gate: %w", device.Name, err)
	}
	runCtx, cancel := context.WithCancel(self.t3.Context())
	var lifetime sync.WaitGroup
	lifetime.Go(func() {
		gate.Inbox().Run(runCtx)
	})
	self.t3.OnClose(func() {
		cancel()
		lifetime.Wait()
		remove()
	})
	return gate, nil
}

// Makes network network_verified (§21.7) by a paid balance of its own, carrying Pro's entitlement when pro, so its
// document may hold §8.5's standing publishes (worldVerifyNetwork).
func (self *World) VerifyNetwork(ctx context.Context, network *WorldNetwork, pro bool, settings *WorldTlsSettings) error {
	if settings == nil {
		settings = DefaultWorldTlsSettings()
	}
	return worldVerifyNetwork(ctx, network.NetworkId, pro, settings.VerifiedByteCount, settings.VerifiedNetRevenueNanoCents, settings.VerifiedDuration)
}

// device's issued certificate row for its network's pair in zone, <label>.<root> and *.<label>.<root>, on its own TLS
// key (worldIssueTlsCertificateRow): what §13.9's tls/ listen asks the network's facts to hold for the sidecar that
// listens.
func (self *World) IssueTlsCertificateRow(ctx context.Context, device *sidecart3.T3Device, zone *sidecar.DnsZone, settings *WorldTlsSettings) error {
	if settings == nil {
		settings = DefaultWorldTlsSettings()
	}
	identity := device.Sidecar().Identity()
	key, err := device.Sidecar().TlsDeviceKey()
	if err != nil || identity == nil {
		return fmt.Errorf("%s's TLS key: %v", device.Name, err)
	}
	now := time.Now()
	root := strings.TrimSuffix(zone.Root, ".")
	return worldIssueTlsCertificateRow(ctx, device.Network.NetworkId, server.Id(identity.ClientId), server.Id(identity.SidecarId), key, root, zone.Label, now.Add(-settings.CertificateValidity), now.Add(settings.CertificateValidity))
}

// device's Listen on name with listenSettings, tried again every RetryInterval within Timeout while it answers the
// socket service unavailable: a socket component attaches to the running generation and reads its network's label
// after AddComponent, with no hook a test reads. Any other answer is the listen's.
func (self *World) Listen(ctx context.Context, device *sidecart3.T3Device, name string, listenSettings *sidecar.SocketListenSettings, settings *WorldTlsSettings) (*sidecar.SocketListener, error) {
	if settings == nil {
		settings = DefaultWorldTlsSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		listener, err := device.Sidecar().Listen(ctx, name, listenSettings)
		if err == nil || !errors.Is(err, sidecar.ErrSocketUnavailable) {
			return listener, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s's sockets stayed unavailable: %w (%w)", device.Name, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// The SHA-256 of device's TLS key's SubjectPublicKeyInfo: what a visitor that completed TLS with the device pins.
func worldTlsKeySpkiSha256(device *sidecart3.T3Device) ([]byte, error) {
	key, err := device.Sidecar().TlsDeviceKey()
	if err != nil {
		return nil, err
	}
	spkiSha256 := key.SpkiSha256()
	return spkiSha256[:], nil
}

// A listener served by worldServeTlsEcho: Wait ends once every connection's handling has, after the listener's close,
// and Ended answers why its accepts ended, nil while they go on, so a stalled visit can say the registration's end.
type worldTlsEcho struct {
	Wait func()

	stateLock sync.Mutex
	endErr    error
}

func (self *worldTlsEcho) end(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.endErr = err
}

func (self *worldTlsEcho) Ended() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.endErr
}

// Serves every connection listener accepts until it closes, each ended by TLS on device's own key, its self-signed
// leaf for serverName, then one request line read and answer(line) written back.
func worldServeTlsEcho(device *sidecart3.T3Device, listener *sidecar.SocketListener, serverName string, answer func(line []byte) []byte, settings *WorldTlsSettings) (*worldTlsEcho, error) {
	certificate, err := device.Sidecar().TlsDeviceCertificate([]string{serverName})
	if err != nil {
		return nil, fmt.Errorf("%s's certificate for %s: %w", device.Name, serverName, err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{*certificate}, MinVersion: tls.VersionTLS12}
	var handlers sync.WaitGroup
	handle := func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(settings.VisitTimeout))
		server := tls.Server(conn, config)
		defer server.Close()
		line, err := bufio.NewReaderSize(server, settings.RequestMaxByteCount).ReadSlice('\n')
		if err != nil {
			return
		}
		server.Write(answer(bytes.TrimSuffix(line, []byte("\n"))))
	}
	echo := &worldTlsEcho{Wait: handlers.Wait}
	handlers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				echo.end(err)
				return
			}
			handlers.Go(func() {
				handle(conn)
			})
		}
	})
	return echo, nil
}

// One visit to serverName at the world's lb interface's 443, as the lb hands it to tlsedge (World.DialLb, its PROXY
// v1 header naming the visitor), a TLS handshake for serverName whose leaf must be on the key spkiSha256 names, and
// request as one line, whose answer line it returns.
func (self *World) VisitTls(ctx context.Context, serverName string, spkiSha256 []byte, request []byte, settings *WorldTlsSettings) ([]byte, error) {
	if settings == nil {
		settings = DefaultWorldTlsSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.VisitTimeout)
	defer cancel()
	conn, err := self.DialLb(ctx, settings.VisitorAddress, netip.AddrPortFrom(self.settings.LbAddress, worldTlsPort))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		// the device's own key is what the visit proves it reached, pinned below; its leaf is self-signed
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			spki, err := x509.MarshalPKIXPublicKey(state.PeerCertificates[0].PublicKey)
			if err != nil {
				return err
			}
			if digest := sha256.Sum256(spki); !bytes.Equal(digest[:], spkiSha256) {
				return fmt.Errorf("the leaf is on a key not the listener's: %x", digest)
			}
			return nil
		},
	})
	if err := client.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("the handshake: %w", err)
	}
	if _, err := client.Write(append(bytes.Clone(request), '\n')); err != nil {
		return nil, err
	}
	answer, err := bufio.NewReaderSize(client, settings.RequestMaxByteCount).ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("the answer: %w", err)
	}
	return bytes.TrimSuffix(answer, []byte("\n")), nil
}

// VisitTls until its answer is want, tried again every RetryInterval within Timeout: the name's registration reaches
// sockets at its verdict and tlsedge at its Find, with no hook a test reads. The last error otherwise.
func (self *World) AwaitTlsVisit(ctx context.Context, serverName string, spkiSha256 []byte, request []byte, want []byte, settings *WorldTlsSettings) error {
	if settings == nil {
		settings = DefaultWorldTlsSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	tries := 0
	for {
		tries += 1
		answer, err := self.VisitTls(ctx, serverName, spkiSha256, request, settings)
		if err == nil && bytes.Equal(answer, want) {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("answered %q, not %q", answer, want)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s through tlsedge, %d visits: %w (%w)", serverName, tries, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// A stand-in for §16.6's gate behind one device's Authorize that passes each listen action alone (the World's
// header): the stock gate refuses Sidecar.Listen's action on a tls/ or smtp/ name as one with no effect, so the step
// that meets that refusal reports it and carries on through this one to the servers past it.
type worldListenGate struct{}

func (worldListenGate) Authorize(ctx context.Context, action *sidecar.SidecarAction, decision *bridge.Signed) error {
	if action == nil || action.Kind != sidecar.SidecarActionListen {
		return fmt.Errorf("the world's listen gate passes listens alone: %w", sidecar.ErrSidecarGateDenied)
	}
	return nil
}

// The port tlsedge's visitors reach, as the lb forwards it.
const worldTlsPort = 443

// A tls/ name's host as a visitor names it: host.<zone>, the zone with no final dot.
func worldTlsServerName(host string, zone string) string {
	return host + "." + strings.TrimSuffix(zone, ".")
}

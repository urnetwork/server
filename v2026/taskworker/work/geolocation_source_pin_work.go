package work

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// GeolocationSourcePinRefreshTimeout is how often every source host is
// re-observed. Six hours is far shorter than any certificate's life and far
// longer than the handful of TLS handshakes it costs, so a rotation is picked
// up within a quarter of a day without the job ever being a load consideration.
const GeolocationSourcePinRefreshTimeout = 6 * time.Hour

// geolocationSourceDialTimeout bounds one host's dial + handshake. A host that
// blackholes rather than refusing is a failure mode too, and an untimed dial
// would let one such host stall the whole pass -- which would leave the
// remaining hosts un-refreshed and look exactly like the silent staleness this
// job exists to prevent.
const geolocationSourceDialTimeout = 15 * time.Second

// geolocationSourceTarget is one host to observe and the address to reach it
// at. Production always sets Addr to Host:443 (see productionGeolocationSourceTargets).
//
// Addr is a parameter ONLY so tests can point a host at a local listener. Note
// what is and is not injectable here: an address, and nothing else. The dialer,
// the *tls.Config, and the root pool are all built inside this package by
// sourceTLSConfig and observeGeolocationSourcePin, and there is deliberately no
// way for a caller to supply a transport, a connection, a tls.Config or a
// RootCAs override. That is the line this whole feature rests on -- a pin is
// only ever recorded from a connection this package itself made and verified
// against the system roots. A caller-supplied transport could be a provider
// tunnel, and a caller-supplied root pool could be a provider's own CA; either
// would let the provider under test choose the pin that is supposed to catch
// it.
type geolocationSourceTarget struct {
	Host string
	Addr string
}

// productionGeolocationSourceTargets is the real target list: every known
// source host, dialed on 443 at its own name.
func productionGeolocationSourceTargets() []geolocationSourceTarget {
	targets := make([]geolocationSourceTarget, 0, len(model.GeolocationSourceHosts))
	for _, host := range model.GeolocationSourceHosts {
		targets = append(targets, geolocationSourceTarget{
			Host: host,
			Addr: net.JoinHostPort(host, "443"),
		})
	}
	return targets
}

// GeolocationSourcePinChange is one host's pin changing between two
// observations, carrying both the old and the new value.
//
// This is a return value rather than only a log line so the "rotation is
// visible" property is testable: a test can assert the change record carries
// what it replaced, which is the part that was missing during the outage. The
// job logs each record; nothing else consumes them.
//
// OldLeaf/OldIntermediate are empty for a host's first ever observation, which
// is a first sighting rather than a rotation and is logged as such.
type GeolocationSourcePinChange struct {
	Host             string
	OldLeaf          string
	NewLeaf          string
	OldIntermediate  string
	NewIntermediate  string
	FirstObservation bool
}

// sourceTLSConfig builds the tls.Config used for every observation.
//
// InsecureSkipVerify is false and ServerName is set, and both are
// load-bearing rather than boilerplate:
//
//   - InsecureSkipVerify false is what makes the observation mean anything. It
//     is also what populates ConnectionState().VerifiedChains at all: crypto/tls
//     leaves the verified chains empty when verification is skipped, so the
//     observation code below structurally cannot record a pin from an
//     unverified handshake -- it has nothing to read.
//   - ServerName pins the identity being verified to the host being recorded.
//     Without it, hostname verification would have nothing to check against and
//     any chain-valid certificate for any name would satisfy the handshake.
//
// No RootCAs is set, so the system root pool is used. There is no override and
// there must never be one, not even for tests: a settable root pool is exactly
// the injection point that would let something other than the public WebPKI
// decide what this server vouches for.
func sourceTLSConfig(host string) *tls.Config {
	return &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	}
}

// spkiPin is the base64 sha-256 of a certificate's subject public key info,
// byte-for-byte the same form the prober's providertunnel.SPKIPin computes and
// compares against. Hashing the key rather than the certificate is what lets a
// pin survive a renewal that keeps the same key.
func spkiPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// observeGeolocationSourcePin dials addr directly, completes a fully verified
// TLS handshake as host, and returns the leaf and issuing-intermediate SPKI
// pins from the VERIFIED chain.
//
// It reads ConnectionState().VerifiedChains, never PeerCertificates, and that
// is a security property rather than a preference. PeerCertificates is whatever
// the peer sent; crypto/tls only ever uses the entries past the leaf as a pool
// to build a path from, and never promises the path it built used them. So a
// peer may pad its Certificate message with any publicly downloadable
// certificate it likes, and a PeerCertificates[1]-based reading would happily
// record that inert padding as this host's intermediate pin. VerifiedChains
// contains only certificates on a path crypto/tls actually validated to a
// system root. The prober's checkPin matches against verified chains for the
// same reason (see providertunnel/pinning.go), so recording from anything else
// would also record a pin the prober could never match.
//
// Any failure -- dial, handshake, hostname mismatch, expiry, an untrusted
// issuer, or a chain too short to have an issuer in it -- returns an error and
// no pin. The caller must leave the stored row alone in that case.
func observeGeolocationSourcePin(ctx context.Context, host string, addr string) (leafSpki string, intermediateSpki string, err error) {
	dialer := &tls.Dialer{
		NetDialer: server.NewDialer(geolocationSourceDialTimeout),
		Config:    sourceTLSConfig(host),
	}

	dialCtx, cancel := context.WithTimeout(ctx, geolocationSourceDialTimeout)
	defer cancel()

	conn, err := dialer.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		// tls.Dialer always returns a *tls.Conn; this is here so a future
		// change cannot silently turn into a pin read from a plain connection.
		return "", "", fmt.Errorf("expected a tls connection to %s (%s), got %T", host, addr, conn)
	}

	chains := tlsConn.ConnectionState().VerifiedChains
	if len(chains) == 0 {
		// unreachable with InsecureSkipVerify false -- crypto/tls aborts the
		// handshake before this point when verification fails -- and it stays
		// unreachable only as long as sourceTLSConfig keeps verification on.
		// Failing here means a verification-disabled config could never write a
		// pin even if one were somehow introduced.
		return "", "", fmt.Errorf("no verified certificate chain for %s (%s)", host, addr)
	}
	chain := chains[0]
	if len(chain) < 2 {
		// a leaf with no issuer on the verified path: nothing to record as the
		// intermediate pin. Writing an empty intermediate would be worse than
		// writing nothing, since an empty pin matches no certificate at all.
		return "", "", fmt.Errorf("verified chain for %s (%s) has %d certificates, need at least a leaf and its issuer", host, addr, len(chain))
	}

	// chain[0] is the leaf and chain[1] is its issuer on the validated path.
	// For a two-certificate chain the issuer is the root itself, which is a
	// broader pin than an intermediate would be -- but no broader than the
	// trust the handshake already placed in that root, and the prober accepts a
	// match anywhere in the chain regardless.
	return spkiPin(chain[0]), spkiPin(chain[1]), nil
}

// refreshGeolocationSourcePins observes every target and stores what it saw.
//
// Per-host isolation is the point. A host that fails validation leaves its
// stored row exactly as it was and contributes an error; the loop continues to
// the next host. Nothing is deleted, nothing is blanked, and no host's failure
// can cost another host its pin -- the failure mode that started this was one
// stale pin quietly shrinking the usable source set below the consensus
// minimum, and a job that abandoned the remaining hosts on the first error
// would reproduce it exactly.
//
// The store call is only reached after a successful, verified observation, so
// the "leave the previous row untouched" guarantee is structural rather than a
// branch that has to be remembered.
func refreshGeolocationSourcePins(
	ctx context.Context,
	targets []geolocationSourceTarget,
) (changes []GeolocationSourcePinChange, errs []error) {
	for _, target := range targets {
		leafSpki, intermediateSpki, err := observeGeolocationSourcePin(ctx, target.Host, target.Addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("observe %s (%s): %w", target.Host, target.Addr, err))
			continue
		}

		previous := model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
			Host:             target.Host,
			LeafSpki:         leafSpki,
			IntermediateSpki: intermediateSpki,
			ObservedAt:       server.NowUtc(),
		})

		if previous == nil {
			changes = append(changes, GeolocationSourcePinChange{
				Host:             target.Host,
				NewLeaf:          leafSpki,
				NewIntermediate:  intermediateSpki,
				FirstObservation: true,
			})
			continue
		}
		if previous.LeafSpki != leafSpki || previous.IntermediateSpki != intermediateSpki {
			changes = append(changes, GeolocationSourcePinChange{
				Host:            target.Host,
				OldLeaf:         previous.LeafSpki,
				NewLeaf:         leafSpki,
				OldIntermediate: previous.IntermediateSpki,
				NewIntermediate: intermediateSpki,
			})
		}
	}

	return changes, errs
}

type RefreshGeolocationSourcePinsArgs struct{}

type RefreshGeolocationSourcePinsResult struct {
	// counts only; the detail is in the log, and a task result is not a place
	// anything should be reading pins back out of
	Observed int `json:"observed"`
	Changed  int `json:"changed"`
	Failed   int `json:"failed"`
}

// ScheduleRefreshGeolocationSourcePins schedules an observation now. Nothing
// in production calls it any more: the geolocation sources whose pins this
// job observed are gone (connect/GEOMAP.md D24), and it is kept, with the task
// below, only for the release that drains the rows it scheduled.
func ScheduleRefreshGeolocationSourcePins(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleRefreshGeolocationSourcePinsAt(clientSession, tx, server.NowUtc())
}

func scheduleRefreshGeolocationSourcePinsAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		RefreshGeolocationSourcePins,
		&RefreshGeolocationSourcePinsArgs{},
		clientSession,
		task.RunOnce("refresh_geolocation_source_pins"),
		task.RunAt(runAt),
	)
}

// RefreshGeolocationSourcePins is retired. It observed the certificate pins
// of the three ip-intelligence sources the prober used to geolocate through,
// and the prober consults none of them now: the exit is placed by the
// operator's own /ip echo and GeoLite2 (GEOMAP §11.3). A row this job
// scheduled before the release runs once more here, observes nothing, and
// schedules no successor.
//
// It is not repurposed to pin the api host the echo answers on, deliberately.
// A pin that lags a certificate rotation -- a new leaf from a different
// intermediate, which the api's issuer does routinely -- fails every warm-up
// in the fleet on TLS authentication, and a TLS-authentication failure is
// immediate dark evidence the batch guard keeps (§11.3): one rotation would
// darken the whole fleet until the next observation. Unpinned, the echo host
// is verified by WebPKI like every pooled destination, which a provider on the
// path cannot pass without a mis-issued certificate. The pin route keeps
// serving the rows already stored, for hosts no probe dials any more, which
// the prober drops before it opens a tunnel (fleetprobe.restrictPins); the
// route, this task and its table go together one release later.
func RefreshGeolocationSourcePins(
	_ *RefreshGeolocationSourcePinsArgs,
	_ *session.ClientSession,
) (*RefreshGeolocationSourcePinsResult, error) {
	glog.Infof("[gsp]the geolocation-source pin refresh is retired; this pending row drains without a successor\n")
	return &RefreshGeolocationSourcePinsResult{}, nil
}

func RefreshGeolocationSourcePinsPost(
	_ *RefreshGeolocationSourcePinsArgs,
	_ *RefreshGeolocationSourcePinsResult,
	_ *session.ClientSession,
	_ server.PgTx,
) error {
	return nil
}

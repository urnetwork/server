package work

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// TestSourceTLSConfigMandatesWebPKI is the cheap, direct assertion of the line
// this whole feature rests on. A pin may only ever be recorded from a
// connection that passed full WebPKI validation, and these three fields are
// what enforce that: verification on, an identity to verify against, and the
// system root pool rather than one someone handed us.
//
// The signature of observeGeolocationSourcePin carries the other half: it
// accepts a host and an address, and there is no parameter through which a
// caller could supply a transport, a connection, a tls.Config or a root pool.
// A provider tunnel therefore cannot be the path a pin is learned over, because
// there is no way to pass one in.
func TestSourceTLSConfigMandatesWebPKI(t *testing.T) {
	cfg := sourceTLSConfig("ipinfo.io")

	if cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify must be false: an unverified chain would let the host being probed choose its own pin")
	}
	connect.AssertEqual(t, cfg.ServerName, "ipinfo.io")
	if cfg.RootCAs != nil {
		t.Fatal("RootCAs must be nil so the system roots are used; an overridable root pool is the injection point this design exists to close")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatal("no custom peer verification: the standard WebPKI check is the whole point")
	}
}

// selfSignedTLSServer starts an httptest TLS server whose certificate is signed
// by its own throwaway CA, which is in no system root store. This is the
// hostile case: a server presenting a certificate that does not validate.
func selfSignedTLSServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	u, err := url.Parse(ts.URL)
	if err != nil {
		ts.Close()
		t.Fatalf("parse httptest url: %v", err)
	}
	return u.Host, ts.Close
}

// requireRealSourceReachable skips when this machine has no outbound path to a
// real source host. The tests that use it are the only ones that can
// demonstrate a SUCCESSFUL observation: a success requires a chain that
// validates against the system roots, and manufacturing one hermetically would
// mean injecting a root pool -- which is exactly what this design forbids. So
// the hermetic tests cover the hostile cases and these cover the good path.
//
// The gate is a plain TCP dial, deliberately NOT a call to
// observeGeolocationSourcePin. Gating on the function under test would make
// these tests SKIP rather than FAIL whenever observation itself broke, which is
// the vacuous-coverage trap: no network skips, broken observation fails.
func requireRealSourceReachable(t testing.TB, host string) (addr string) {
	t.Helper()
	addr = net.JoinHostPort(host, "443")
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Skipf("no outbound path to %s (%v); skipping the real-observation case", addr, err)
	}
	conn.Close()
	return addr
}

// TestRefreshGeolocationSourcePinsLeavesPriorRowUnchangedOnSelfSignedServer is
// the most important test here. A host that fails validation must leave what
// was already stored EXACTLY as it was and raise -- never overwrite a good pin
// with an unverified one, and never blank it either. Overwriting would let a
// hostile server choose the pin; blanking would shrink the usable source set,
// which is the failure that took the whole fleet's consensus offline.
func TestRefreshGeolocationSourcePinsLeavesPriorRowUnchangedOnSelfSignedServer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addr, stop := selfSignedTLSServer(t)
		defer stop()

		observedAt := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		good := &model.GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "good-leaf",
			IntermediateSpki: "good-intermediate",
			ObservedAt:       observedAt,
		}
		model.SetGeolocationSourcePin(ctx, good)

		changes, errs := refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: "ipinfo.io", Addr: addr},
		})

		if len(errs) != 1 {
			t.Fatalf("expected the self-signed server to be rejected, got errs=%v", errs)
		}
		connect.AssertEqual(t, len(changes), 0)

		after := model.GetGeolocationSourcePin(ctx, "ipinfo.io")
		if after == nil {
			t.Fatal("the previously stored pin was removed by a failed observation")
		}
		connect.AssertEqual(t, after.LeafSpki, good.LeafSpki)
		connect.AssertEqual(t, after.IntermediateSpki, good.IntermediateSpki)
		connect.AssertEqual(t, after.ObservedAt.UTC().Equal(observedAt.UTC()), true)
	})
}

// TestRefreshGeolocationSourcePinsLeavesPriorRowUnchangedOnHostnameMismatch is
// the same guarantee for the other half of WebPKI. Here the chain is genuinely
// valid and genuinely trusted -- it is a real source host's real certificate --
// but it is not for the name being asked for. Without ServerName set and
// verification on, this would be accepted and one host's certificate would
// become another host's pin.
func TestRefreshGeolocationSourcePinsLeavesPriorRowUnchangedOnHostnameMismatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		addr := requireRealSourceReachable(t, "ipinfo.io")

		observedAt := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
			Host:             "free.freeipapi.com",
			LeafSpki:         "good-leaf",
			IntermediateSpki: "good-intermediate",
			ObservedAt:       observedAt,
		})

		// ipinfo.io's real address, asked for under a name its certificate does
		// not cover
		changes, errs := refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: "free.freeipapi.com", Addr: addr},
		})

		if len(errs) != 1 {
			t.Fatalf("expected a hostname mismatch to be rejected, got errs=%v", errs)
		}
		connect.AssertEqual(t, len(changes), 0)

		after := model.GetGeolocationSourcePin(ctx, "free.freeipapi.com")
		if after == nil {
			t.Fatal("the previously stored pin was removed by a failed observation")
		}
		connect.AssertEqual(t, after.LeafSpki, "good-leaf")
		connect.AssertEqual(t, after.IntermediateSpki, "good-intermediate")
		connect.AssertEqual(t, after.ObservedAt.UTC().Equal(observedAt.UTC()), true)
	})
}

// TestRefreshGeolocationSourcePinsReportsOldAndNewOnRotation covers the
// "rotation must be visible" requirement. The change record carries both the
// value that was there and the value that replaced it, which is what the job
// logs. A record that carried only the new value would be no better than the
// silence that made the original outage undiagnosable.
func TestRefreshGeolocationSourcePinsReportsOldAndNewOnRotation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		host := "ipinfo.io"
		addr := requireRealSourceReachable(t, host)

		// stand in for a pin observed before the certificate rotated
		model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
			Host:             host,
			LeafSpki:         "stale-leaf",
			IntermediateSpki: "stale-intermediate",
			ObservedAt:       server.NowUtc().Add(-24 * time.Hour),
		})

		changes, errs := refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: host, Addr: addr},
		})
		connect.AssertEqual(t, len(errs), 0)
		if len(changes) != 1 {
			t.Fatalf("expected exactly one change record, got %+v", changes)
		}

		change := changes[0]
		connect.AssertEqual(t, change.Host, host)
		connect.AssertEqual(t, change.FirstObservation, false)
		connect.AssertEqual(t, change.OldLeaf, "stale-leaf")
		connect.AssertEqual(t, change.OldIntermediate, "stale-intermediate")
		if change.NewLeaf == "" || change.NewIntermediate == "" {
			t.Fatalf("expected observed leaf and intermediate pins, got %+v", change)
		}
		if change.NewLeaf == change.OldLeaf || change.NewIntermediate == change.OldIntermediate {
			t.Fatalf("expected the observed pins to differ from the stale ones, got %+v", change)
		}
		// what was logged is what was stored
		stored := model.GetGeolocationSourcePin(ctx, host)
		connect.AssertEqual(t, stored.LeafSpki, change.NewLeaf)
		connect.AssertEqual(t, stored.IntermediateSpki, change.NewIntermediate)

		// re-observing an unchanged certificate is not a rotation and must not
		// be reported as one, or the log stops meaning anything
		changes, errs = refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: host, Addr: addr},
		})
		connect.AssertEqual(t, len(errs), 0)
		connect.AssertEqual(t, len(changes), 0)
	})
}

// TestRefreshGeolocationSourcePinsContinuesPastAFailingHost: one bad host must
// never cost the others their observation. The original outage was two sources
// dropping out at once and leaving one against a minimum of two; a refresh that
// abandoned the remaining hosts after the first failure would manufacture that
// same shortfall from a single unreachable endpoint.
func TestRefreshGeolocationSourcePinsContinuesPastAFailingHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		goodHost := "ipinfo.io"
		goodAddr := requireRealSourceReachable(t, goodHost)
		badAddr, stop := selfSignedTLSServer(t)
		defer stop()

		// the failing host is FIRST, so a loop that stopped on the first error
		// would never reach the good one
		changes, errs := refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: "free.freeipapi.com", Addr: badAddr},
			{Host: goodHost, Addr: goodAddr},
		})

		connect.AssertEqual(t, len(errs), 1)
		if len(changes) != 1 {
			t.Fatalf("expected the good host to still be observed, got %+v", changes)
		}
		connect.AssertEqual(t, changes[0].Host, goodHost)
		connect.AssertEqual(t, changes[0].FirstObservation, true)

		if model.GetGeolocationSourcePin(ctx, goodHost) == nil {
			t.Fatal("the good host was not stored")
		}
		if model.GetGeolocationSourcePin(ctx, "free.freeipapi.com") != nil {
			t.Fatal("a failed observation must not write a row")
		}
	})
}

// TestProductionGeolocationSourceTargetsCoverEverySourceHost: the real target
// list is derived from model.GeolocationSourceHosts and dials each host on 443
// under its own name. Nothing else may become a target.
func TestProductionGeolocationSourceTargetsCoverEverySourceHost(t *testing.T) {
	targets := productionGeolocationSourceTargets()
	connect.AssertEqual(t, len(targets), len(model.GeolocationSourceHosts))
	for i, host := range model.GeolocationSourceHosts {
		connect.AssertEqual(t, targets[i].Host, host)
		connect.AssertEqual(t, targets[i].Addr, net.JoinHostPort(host, "443"))
	}
}

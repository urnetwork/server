//go:build sidecar_pending

package model

// certs' CAA checks' store on a private database (TLS-M6-04; §12.8 step 3):
// the pending strong-mode hosts listed and the weak and strict-mode MX ones
// never; an active host due once its newest custom chain reaches the recheck
// fraction, in its window only; a verdict applied as DnsCustomHostResultInTx
// applies it; and a host that went answering gone.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
)

// The hosts of a listing.
func testCaaHostNames(hosts []*certsservice.CertsCaaHost) []string {
	names := []string{}
	for _, host := range hosts {
		names = append(names, host.Host)
	}
	return names
}

// A custom row of host for a device of the network, its chain valid from
// notBefore for lifetime.
func testCaaCustomChain(ctx context.Context, networkId server.Id, host string, domain string, notBefore time.Time, lifetime time.Duration) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_cert (
					cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation,
					state, state_time, not_before, not_after, custom_host, custom_domain, chain
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			`,
			server.NewId(),
			networkId,
			server.NewId(),
			server.NewId(),
			[]byte{1},
			[]byte{2},
			[]byte{3},
			string(sidecar.TlsCertStateCustomIssued),
			notBefore,
			notBefore,
			notBefore.Add(lifetime),
			host,
			domain,
			[][]byte{{4}},
		))
	})
}

func TestCertsCaaStoreListsJudgesAndRechecks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !dnsCustomHostLinked() {
			return
		}
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		store := NewCertsCaaStore(nil)
		owner := "shop." + world.apex
		www := "www." + world.domain
		for _, host := range []string{world.domain, www} {
			if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: owner}); err != nil {
				t.Fatal(err)
			}
		}
		weak := "blog." + world.domain
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: weak, Owner: owner, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}

		// the strong-mode pending hosts, in host order, a page at a time; never the weak host
		pending, err := store.ListCaaPending(ctx, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{world.domain, www}; !slices.Equal(testCaaHostNames(pending), want) {
			t.Fatalf("pending %v, want %v", testCaaHostNames(pending), want)
		}
		// the owner as the canary route takes it, with no final dot
		if pending[0].Owner != strings.TrimSuffix(owner, ".") || pending[0].NetworkId != sidecar.Id(world.networkId) || !pending[0].Pending {
			t.Errorf("listed %+v", pending[0])
		}
		if page, err := store.ListCaaPending(ctx, world.domain, 1); err != nil || !slices.Equal(testCaaHostNames(page), []string{www}) {
			t.Errorf("the second page %v, %v", testCaaHostNames(page), err)
		}
		if host, err := store.GetCaaHost(ctx, weak); err != nil || host != nil {
			t.Errorf("the weak host: %+v, %v", host, err)
		}

		// a pass makes www active; a failure refuses the apex, its row gone
		if outcome, err := store.ApplyCaaVerdict(ctx, www, true); err != nil || outcome != certsservice.CertsCaaOutcomeActivated {
			t.Fatalf("www: %v, %v", outcome, err)
		}
		if outcome, err := store.ApplyCaaVerdict(ctx, world.domain, false); err != nil || outcome != certsservice.CertsCaaOutcomeRefused {
			t.Fatalf("the apex: %v, %v", outcome, err)
		}
		if outcome, err := store.ApplyCaaVerdict(ctx, world.domain, true); err != nil || outcome != certsservice.CertsCaaOutcomeGone {
			t.Fatalf("a refused host's second verdict: %v, %v", outcome, err)
		}
		if outcome, err := store.ApplyCaaVerdict(ctx, weak, false); err != nil || outcome != certsservice.CertsCaaOutcomeGone {
			t.Fatalf("a weak host's verdict: %v, %v", outcome, err)
		}
		if pending, err := store.ListCaaPending(ctx, "", 10); err != nil || len(pending) != 0 {
			t.Fatalf("pending after the verdicts: %v, %v", testCaaHostNames(pending), err)
		}
		host, err := store.GetCaaHost(ctx, www)
		if err != nil || host == nil || host.Pending {
			t.Fatalf("www stands as %+v, %v", host, err)
		}

		// due once its newest chain is half through its life, in that window alone
		notBefore := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
		lifetime := 45 * 24 * time.Hour
		testCaaCustomChain(ctx, world.networkId, www, world.domain, notBefore.Add(-lifetime), lifetime)
		testCaaCustomChain(ctx, world.networkId, www, world.domain, notBefore, lifetime)
		recheck := notBefore.Add(lifetime / 2)
		for _, c := range []struct {
			since time.Time
			until time.Time
			due   bool
		}{
			{since: time.Time{}, until: recheck, due: true},
			{since: recheck.Add(-time.Hour), until: recheck.Add(time.Hour), due: true},
			{since: recheck, until: recheck.Add(time.Hour), due: false},
			{since: time.Time{}, until: recheck.Add(-time.Second), due: false},
		} {
			due, err := store.ListCaaDue(ctx, 0.5, c.since, c.until, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(testCaaHostNames(due), www); got != c.due {
				t.Errorf("(%v, %v]: due %v, want %v", c.since, c.until, got, c.due)
			}
		}
		// the older chain's recheck counts for nothing once a newer one is registered
		older := notBefore.Add(-lifetime).Add(lifetime / 2)
		if due, err := store.ListCaaDue(ctx, 0.5, older.Add(-time.Hour), older.Add(time.Hour), "", 10); err != nil || 0 < len(due) {
			t.Errorf("the older chain: %v, %v", testCaaHostNames(due), err)
		}
		// a later failure exposes it, mapped still
		if outcome, err := store.ApplyCaaVerdict(ctx, www, false); err != nil || outcome != certsservice.CertsCaaOutcomeExposed {
			t.Fatalf("www's recheck: %v, %v", outcome, err)
		}
		if row := world.row(ctx, www); row.State != sidecar.DnsCustomHostStateActive || row.EffectiveCaaChainOk || !testDnsCustomHostMapped(ctx, www) {
			t.Fatalf("after the recheck www is %+v", row)
		}
	})
}

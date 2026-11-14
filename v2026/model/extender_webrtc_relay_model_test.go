package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The webrtc carrier's signaling relay keys (connect/EXTENDER.md S8,
// extender_webrtc_relay_model.go), and the first-contact rule that keeps an
// extender accepted on webrtc alone out of what a client reaches the operator
// by.

// Root cause: every exchange process found its residents' offers by matching
// a `{*}w_offer` pattern against the keyspace notification of every set on
// Main. Observable: an offer's wake goes to the one channel of the exchange
// process its client's resident registration names, and carries the client
// id alone.
func TestExtenderWebRtcOfferWakesNameTheResidentsProcess(t *testing.T) {
	clientId := server.NewId()
	resident := &NetworkClientResident{
		ClientId:        clientId,
		ResidentHost:    "edge-0",
		ResidentService: "connect",
		ResidentBlock:   "g3",
	}
	connect.AssertEqual(t, ExtenderWebRtcOfferChannelForResident(resident), "extender_webrtc_offer_edge-0_connect_g3")
	connect.AssertEqual(t, ExtenderWebRtcOfferChannel("edge-0", "connect", "g4") == ExtenderWebRtcOfferChannelForResident(resident), false)
	if parsed, ok := ParseExtenderWebRtcOfferWake(clientId.String()); !ok || parsed != clientId {
		t.Fatalf("wake parsed to %s %t", parsed, ok)
	}
	for _, payload := range []string{"", "not-an-id", extenderWebRtcOfferKey(clientId)} {
		if _, ok := ParseExtenderWebRtcOfferWake(payload); ok {
			t.Fatalf("the wake %q parsed", payload)
		}
	}
	// both keys of a client share its slot
	connect.AssertEqual(t, extenderWebRtcOfferKey(clientId)[:38], extenderWebRtcAnswerKey(clientId, server.NewId())[:38])
}

// A client's pending offers are bounded at the add, carry their ttl, and are
// taken once each; an offer its prober gave up on is removed.
func TestExtenderWebRtcOffersAreBoundedAndTakenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		newOffer := func() *ExtenderWebRtcOffer {
			return &ExtenderWebRtcOffer{
				RequestId:    server.NewId(),
				ExpireTimeMs: server.NowUtc().Add(5 * time.Second).UnixMilli(),
				Offer:        `{"type":"offer","sdp":"v=0"}`,
			}
		}
		first := newOffer()
		second := newOffer()
		firstMember, added := AddExtenderWebRtcOffer(ctx, clientId, first, 2, 30*time.Second)
		connect.AssertEqual(t, added, true)
		_, added = AddExtenderWebRtcOffer(ctx, clientId, second, 2, 30*time.Second)
		connect.AssertEqual(t, added, true)
		// a third finds the bound
		if _, added := AddExtenderWebRtcOffer(ctx, clientId, newOffer(), 2, 30*time.Second); added {
			t.Fatal("an offer past the pending bound was added")
		}
		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, extenderWebRtcOfferKey(clientId)).Result()
			server.Raise(err)
		})
		if ttl <= 0 || 30*time.Second < ttl {
			t.Fatalf("offers ttl %s, want (0, 30s]", ttl)
		}

		// a probe that gave up takes its offer back
		RemoveExtenderWebRtcOffer(ctx, clientId, firstMember)
		taken := TakeExtenderWebRtcOffers(ctx, clientId, 16)
		if len(taken) != 1 || taken[0].RequestId != second.RequestId || taken[0].ExpireTimeMs != second.ExpireTimeMs || taken[0].Offer != second.Offer {
			t.Fatalf("taken = %+v, want the second offer alone", taken)
		}
		if again := TakeExtenderWebRtcOffers(ctx, clientId, 16); len(again) != 0 {
			t.Fatalf("a second take found %d", len(again))
		}
		// another client's set is its own
		AddExtenderWebRtcOffer(ctx, clientId, newOffer(), 2, 30*time.Second)
		if other := TakeExtenderWebRtcOffers(ctx, server.NewId(), 16); len(other) != 0 {
			t.Fatalf("another client took %d offers", len(other))
		}
		if limited := TakeExtenderWebRtcOffers(ctx, clientId, 0); len(limited) != 0 {
			t.Fatal("a take of none took an offer")
		}
	})
}

// An answer is waited for once, under its request id, and expires with its
// ttl; a wait with no answer ends at its timeout.
func TestExtenderWebRtcAnswerIsTakenByItsProber(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		requestId := server.NewId()
		PutExtenderWebRtcAnswer(ctx, clientId, requestId, &ExtenderWebRtcAnswer{
			Answer: `{"type":"answer","sdp":"v=0"}`,
		}, 30*time.Second)
		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, extenderWebRtcAnswerKey(clientId, requestId)).Result()
			server.Raise(err)
		})
		if ttl <= 0 || 30*time.Second < ttl {
			t.Fatalf("answer ttl %s, want (0, 30s]", ttl)
		}
		// another request's wait does not take it
		if _, ok := WaitExtenderWebRtcAnswer(ctx, clientId, server.NewId(), time.Second); ok {
			t.Fatal("another request took the answer")
		}
		answer, ok := WaitExtenderWebRtcAnswer(ctx, clientId, requestId, time.Second)
		if !ok || answer.Answer != `{"type":"answer","sdp":"v=0"}` || answer.Error != "" {
			t.Fatalf("answer = %+v %t", answer, ok)
		}
		if _, ok := WaitExtenderWebRtcAnswer(ctx, clientId, requestId, time.Second); ok {
			t.Fatal("the answer was taken twice")
		}
		// a refusal comes back as such
		refusedId := server.NewId()
		PutExtenderWebRtcAnswer(ctx, clientId, refusedId, &ExtenderWebRtcAnswer{
			Error: ExtenderWebRtcRelayErrorBusy,
		}, 30*time.Second)
		refused, ok := WaitExtenderWebRtcAnswer(ctx, clientId, refusedId, time.Second)
		if !ok || refused.Error != ExtenderWebRtcRelayErrorBusy {
			t.Fatalf("refusal = %+v %t", refused, ok)
		}
	})
}

// An address can be a first contact when it serves a socket carrier; one
// accepted on webrtc alone cannot.
func TestExtenderAddressFirstContact(t *testing.T) {
	for _, c := range []struct {
		carriers []string
		want     bool
	}{
		{carriers: []string{connect.ExtenderCarrierTcp}, want: true},
		{carriers: []string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic}, want: true},
		{carriers: []string{connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc}, want: true},
		{carriers: []string{connect.ExtenderCarrierWebRtc}, want: false},
		{carriers: []string{}, want: false},
	} {
		if got := ExtenderAddressFirstContact(c.carriers); got != c.want {
			t.Fatalf("%v: first contact = %t, want %t", c.carriers, got, c.want)
		}
	}
}

// Root cause: the dns sets, the seed cohort and an activation's bootstrap are
// a client's first contact, and an extender accepted on webrtc alone is
// reached only through an exchange connection (S8). Observable: its address
// is in no dns set, an extender with a socket address and a webrtc-only one
// keeps only the socket address in dns, and a webrtc-only extender is never
// sampled for a bootstrap, while its uptime target carries its client id.
func TestWebRtcOnlyExtendersAreNoFirstContact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		baseTime := server.NowUtc().Add(-time.Hour)
		newExtender := func(i int, clientId server.Id, addresses ...*NetworkExtenderAddress) server.Id {
			extenderId := server.NewId()
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  extenderId,
					NetworkId:   networkId,
					ClientId:    clientId,
					PublicKey:   []byte(fmt.Sprintf("extender-public-key-webrtc-%05d", i)),
					CreateTime:  baseTime,
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: "US",
					Active:      true,
				},
				addresses,
			)
			return extenderId
		}
		address := func(ip string, carriers ...string) *NetworkExtenderAddress {
			addr := netip.MustParseAddr(ip)
			ipVersion := 4
			if addr.Is6() {
				ipVersion = 6
			}
			return &NetworkExtenderAddress{
				IpVersion:    ipVersion,
				Ip:           addr,
				Carriers:     carriers,
				ActivateTime: baseTime,
				Active:       true,
			}
		}
		webRtcClientId := server.NewId()
		webRtcOnlyId := newExtender(0, webRtcClientId, address("192.0.2.200", connect.ExtenderCarrierWebRtc))
		mixedId := newExtender(
			1,
			server.NewId(),
			address("192.0.2.201", connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc),
			address("2001:db8::201", connect.ExtenderCarrierWebRtc),
		)
		tcpId := newExtender(2, server.NewId(), address("192.0.2.202", connect.ExtenderCarrierTcp))

		dnsIps := map[string]server.Id{}
		for _, dnsAddress := range GetActiveNetworkExtenderDnsAddresses(ctx) {
			dnsIps[dnsAddress.Ip.String()] = dnsAddress.ExtenderId
		}
		if _, ok := dnsIps["192.0.2.200"]; ok {
			t.Fatal("a webrtc-only address is in the dns pool")
		}
		if _, ok := dnsIps["2001:db8::201"]; ok {
			t.Fatal("a webrtc-only address of a mixed extender is in the dns pool")
		}
		connect.AssertEqual(t, dnsIps["192.0.2.201"], mixedId)
		connect.AssertEqual(t, dnsIps["192.0.2.202"], tcpId)

		sampledIds := map[server.Id]int{}
		for _, sampled := range GetRandomActiveNetworkExtenders(ctx, 8, server.Id{}) {
			sampledIds[sampled.Extender.ExtenderId] = len(sampled.Addresses)
		}
		if _, ok := sampledIds[webRtcOnlyId]; ok {
			t.Fatal("a webrtc-only extender was sampled for a bootstrap")
		}
		// a sampled extender's record keeps every active address
		connect.AssertEqual(t, sampledIds[mixedId], 2)
		connect.AssertEqual(t, sampledIds[tcpId], 1)

		// the uptime task probes the webrtc-only address through its client
		found := false
		for _, target := range GetActiveNetworkExtenderProbeTargets(ctx) {
			if target.ExtenderId == webRtcOnlyId {
				found = true
				connect.AssertEqual(t, target.ClientId, webRtcClientId)
				connect.AssertEqual(t, target.Carriers, []string{connect.ExtenderCarrierWebRtc})
			}
		}
		if !found {
			t.Fatal("the webrtc-only address is not probed for uptime")
		}
	})
}

// Root cause: nothing counted the extenders the operator accepted on the
// webrtc carrier without tcp, so nothing could cap them (controller
// ExtenderActivateSettings.MaxWebRtcOnlyExtenders). Observable: the count is
// of active extenders with an active address that lists webrtc and not tcp --
// one listing webrtc beside quic counts, one listing tcp beside webrtc, a tcp
// one, an inactive address and an inactive extender do not -- each extender
// once however many such addresses it has, and the extender of the key left
// out is not counted.
func TestCountActiveWebRtcOnlyNetworkExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		baseTime := server.NowUtc().Add(-time.Hour)
		connect.AssertEqual(t, CountActiveWebRtcOnlyNetworkExtenders(ctx, nil), 0)

		newExtender := func(i int, active bool, addresses ...*NetworkExtenderAddress) []byte {
			publicKey := []byte(fmt.Sprintf("extender-public-key-webrtc-only-%05d", i))
			Testing_CreateNetworkExtender(
				ctx,
				&NetworkExtender{
					ExtenderId:  server.NewId(),
					NetworkId:   networkId,
					ClientId:    server.NewId(),
					PublicKey:   publicKey,
					CreateTime:  baseTime,
					TcpPort:     443,
					UdpPort:     443,
					DnsPort:     53,
					DnsTld:      connect.DefaultExtenderDnsTld,
					CountryCode: "US",
					Active:      active,
				},
				addresses,
			)
			return publicKey
		}
		address := func(ip string, active bool, carriers ...string) *NetworkExtenderAddress {
			addr := netip.MustParseAddr(ip)
			ipVersion := 4
			if addr.Is6() {
				ipVersion = 6
			}
			return &NetworkExtenderAddress{
				IpVersion:    ipVersion,
				Ip:           addr,
				Carriers:     carriers,
				ActivateTime: baseTime,
				Active:       active,
			}
		}
		// two webrtc-only addresses, counted once
		bothFamiliesKey := newExtender(
			0,
			true,
			address("192.0.2.210", true, connect.ExtenderCarrierWebRtc),
			address("2001:db8::210", true, connect.ExtenderCarrierWebRtc),
		)
		newExtender(1, true, address("192.0.2.211", true, connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic))
		newExtender(2, true, address("192.0.2.212", true, connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc))
		newExtender(3, true, address("192.0.2.213", true, connect.ExtenderCarrierTcp))
		newExtender(4, true, address("192.0.2.214", false, connect.ExtenderCarrierWebRtc))
		newExtender(5, false, address("192.0.2.215", true, connect.ExtenderCarrierWebRtc))

		connect.AssertEqual(t, CountActiveWebRtcOnlyNetworkExtenders(ctx, nil), 2)
		connect.AssertEqual(t, CountActiveWebRtcOnlyNetworkExtenders(ctx, bothFamiliesKey), 1)
		connect.AssertEqual(t, CountActiveWebRtcOnlyNetworkExtenders(ctx, []byte("extender-public-key-none")), 2)

		connect.AssertEqual(t, ExtenderAddressWebRtcOnly([]string{connect.ExtenderCarrierWebRtc}), true)
		connect.AssertEqual(t, ExtenderAddressWebRtcOnly([]string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic}), true)
		connect.AssertEqual(t, ExtenderAddressWebRtcOnly([]string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}), false)
		connect.AssertEqual(t, ExtenderAddressWebRtcOnly([]string{connect.ExtenderCarrierQuic}), false)
	})
}

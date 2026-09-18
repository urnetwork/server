package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Extender proximity (connect/DESIGNNOTES4.md): the hint, and the latency
// report against the real key store and the real signature check.

func TestExtenderHintPlacesTheCallerAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a public address the mmdb places
		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("8.8.8.8", "443"))
		result, err := ExtenderHint(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.ContinentCode, "NA")

		// loopback is nowhere, and nowhere is an empty hint, not an error
		clientSession.ClientAddress = net.JoinHostPort("127.0.0.1", "443")
		result, err = ExtenderHint(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.ContinentCode, "")

		// as is an address that cannot be read
		clientSession.ClientAddress = ""
		result, err = ExtenderHint(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.ContinentCode, "")
	})
}

// A provider with a registered client key, and the attestor its probes sign
// with.
func newTestLatencyProvider(t testing.TB, ctx context.Context) (server.Id, *connect.ExtenderProbeAttestor) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientId := server.NewId()
	model.SetClientPublicKey(ctx, clientId, publicKey)
	return clientId, &connect.ExtenderProbeAttestor{
		ClientId: connect.Id(clientId),
		Sign: func(data []byte) []byte {
			return ed25519.Sign(privateKey, data)
		},
	}
}

// An active extender owned by `clientId`, with a fresh identity key.
func newTestLatencyExtender(t testing.TB, ctx context.Context, networkId server.Id, clientId server.Id) *model.NetworkExtender {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	extender := &model.NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   networkId,
		ClientId:    clientId,
		PublicKey:   publicKey,
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     connect.ExtenderDnsPort,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}
	model.Testing_CreateNetworkExtender(ctx, extender, nil)
	return extender
}

// A signed attestation of the provider against the extender, as the report
// carries it.
func newTestLatencyAttestation(
	t testing.TB,
	attestor *connect.ExtenderProbeAttestor,
	extender *model.NetworkExtender,
	rttMs uint32,
) (*protocol.ExtenderProbeAttestation, *ExtenderLatencyAttestationArgs) {
	t.Helper()
	nonce, err := connect.NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	attestation := &protocol.ExtenderProbeAttestation{
		ProbeClientId:     attestor.ClientId.Bytes(),
		ExtenderPublicKey: extender.PublicKey,
		ProbeNonce:        nonce,
		RttMs:             rttMs,
		TimestampMs:       uint64(server.NowUtc().UnixMilli()),
	}
	if err := connect.SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	return attestation, testLatencyAttestationArgs(attestation)
}

func testLatencyAttestationArgs(attestation *protocol.ExtenderProbeAttestation) *ExtenderLatencyAttestationArgs {
	transport := connect.ExtenderLatencyAttestationFromProto(attestation)
	return &ExtenderLatencyAttestationArgs{
		ClientId:             transport.ClientId,
		ExtenderPublicKeyHex: transport.ExtenderPublicKeyHex,
		ProbeNonce:           transport.ProbeNonce,
		RttMs:                transport.RttMs,
		TimestampMs:          transport.TimestampMs,
		Signature:            transport.Signature,
	}
}

func TestExtenderLatencyReportStoresWhatVerifies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("127.0.0.1", "443"))
		extender := newTestLatencyExtender(t, ctx, clientSession.ByJwt.NetworkId, *clientSession.ByJwt.ClientId)
		providerClientId, attestor := newTestLatencyProvider(t, ctx)

		attestation, attestationArgs := newTestLatencyAttestation(t, attestor, extender, 37)
		result, err := ExtenderLatencyReport(&ExtenderLatencyReportArgs{
			Attestations: []*ExtenderLatencyAttestationArgs{attestationArgs},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 0)

		stored := model.GetNetworkExtenderLatencies(ctx, extender.ExtenderId, time.Time{})
		connect.AssertEqual(t, len(stored), 1)
		connect.AssertEqual(t, stored[0].ClientId, providerClientId)
		connect.AssertEqual(t, stored[0].RttMs, 37)
		connect.AssertEqual(t, stored[0].ProbeTime.UnixMilli(), int64(attestation.TimestampMs))
		if len(stored[0].ProbeNonce) != connect.ExtenderProbeNonceByteCount {
			t.Fatalf("stored nonce is %d bytes", len(stored[0].ProbeNonce))
		}

		// a replay of the same attestation is not a second sample
		result, err = ExtenderLatencyReport(&ExtenderLatencyReportArgs{
			Attestations: []*ExtenderLatencyAttestationArgs{attestationArgs},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, result.Rejected, 1)
		connect.AssertEqual(t, len(model.GetNetworkExtenderLatencies(ctx, extender.ExtenderId, time.Time{})), 1)

		// a fresh one is
		_, secondArgs := newTestLatencyAttestation(t, attestor, extender, 41)
		result, err = ExtenderLatencyReport(&ExtenderLatencyReportArgs{
			Attestations: []*ExtenderLatencyAttestationArgs{secondArgs},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, len(model.GetNetworkExtenderLatencies(ctx, extender.ExtenderId, time.Time{})), 2)

		// the sweep removes what is older than its cut
		connect.AssertEqual(t, model.RemoveOldNetworkExtenderLatencies(ctx, server.NowUtc().Add(-time.Hour)), 0)
		connect.AssertEqual(t, model.RemoveOldNetworkExtenderLatencies(ctx, server.NowUtc().Add(time.Hour)), 2)
		connect.AssertEqual(t, len(model.GetNetworkExtenderLatencies(ctx, extender.ExtenderId, time.Time{})), 0)
	})
}

// Every way a claim can fail the operator's check, each counted and none
// stored: the extender cannot change what the provider signed, cannot invent
// a provider, cannot report for an extender it does not run, and cannot
// attest to itself.
func TestExtenderLatencyReportRejectsWhatDoesNotVerify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("127.0.0.1", "443"))
		extender := newTestLatencyExtender(t, ctx, clientSession.ByJwt.NetworkId, *clientSession.ByJwt.ClientId)
		_, attestor := newTestLatencyProvider(t, ctx)

		// another client's extender
		otherSession := newTestExtenderSession(t, ctx, net.JoinHostPort("127.0.0.1", "443"))
		otherExtender := newTestLatencyExtender(t, ctx, otherSession.ByJwt.NetworkId, *otherSession.ByJwt.ClientId)
		// a provider with no registered key
		_, unregisteredPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		unregisteredAttestor := &connect.ExtenderProbeAttestor{
			ClientId: connect.NewId(),
			Sign: func(data []byte) []byte {
				return ed25519.Sign(unregisteredPrivateKey, data)
			},
		}
		// the extender's own client, with a registered key
		_, extenderPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		model.SetClientPublicKey(ctx, extender.ClientId, extenderPrivateKey.Public().(ed25519.PublicKey))
		selfAttestor := &connect.ExtenderProbeAttestor{
			ClientId: connect.Id(extender.ClientId),
			Sign: func(data []byte) []byte {
				return ed25519.Sign(extenderPrivateKey, data)
			},
		}

		cases := map[string]func() *ExtenderLatencyAttestationArgs{
			"altered rtt": func() *ExtenderLatencyAttestationArgs {
				attestation, _ := newTestLatencyAttestation(t, attestor, extender, 100)
				attestation.RttMs = 1
				return testLatencyAttestationArgs(attestation)
			},
			"altered timestamp": func() *ExtenderLatencyAttestationArgs {
				attestation, _ := newTestLatencyAttestation(t, attestor, extender, 100)
				attestation.TimestampMs += 1
				return testLatencyAttestationArgs(attestation)
			},
			"no signature": func() *ExtenderLatencyAttestationArgs {
				attestation, _ := newTestLatencyAttestation(t, attestor, extender, 100)
				attestation.Signature = nil
				return testLatencyAttestationArgs(attestation)
			},
			"unregistered provider": func() *ExtenderLatencyAttestationArgs {
				_, args := newTestLatencyAttestation(t, unregisteredAttestor, extender, 100)
				return args
			},
			"unknown extender": func() *ExtenderLatencyAttestationArgs {
				unknown := &model.NetworkExtender{PublicKey: []byte(extender.PublicKey)}
				unknown.PublicKey = append([]byte(nil), extender.PublicKey...)
				unknown.PublicKey[0] ^= 1
				_, args := newTestLatencyAttestation(t, attestor, unknown, 100)
				return args
			},
			"another client's extender": func() *ExtenderLatencyAttestationArgs {
				_, args := newTestLatencyAttestation(t, attestor, otherExtender, 100)
				return args
			},
			"self attestation": func() *ExtenderLatencyAttestationArgs {
				_, args := newTestLatencyAttestation(t, selfAttestor, extender, 100)
				return args
			},
			"unreadable client id": func() *ExtenderLatencyAttestationArgs {
				_, args := newTestLatencyAttestation(t, attestor, extender, 100)
				args.ClientId = "not-an-id"
				return args
			},
			"unreadable nonce": func() *ExtenderLatencyAttestationArgs {
				_, args := newTestLatencyAttestation(t, attestor, extender, 100)
				args.ProbeNonce = "!"
				return args
			},
		}
		for name, build := range cases {
			result, err := ExtenderLatencyReport(&ExtenderLatencyReportArgs{
				Attestations: []*ExtenderLatencyAttestationArgs{build()},
			}, clientSession)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if result.Error != "" || result.Accepted != 0 || result.Rejected != 1 {
				t.Fatalf("%s: accepted=%d rejected=%d error=%q", name, result.Accepted, result.Rejected, result.Error)
			}
		}
		connect.AssertEqual(t, len(model.GetNetworkExtenderLatencies(ctx, extender.ExtenderId, time.Time{})), 0)
		connect.AssertEqual(t, len(model.GetNetworkExtenderLatencies(ctx, otherExtender.ExtenderId, time.Time{})), 0)

		// a mixed report stores what verifies and counts the rest
		_, good := newTestLatencyAttestation(t, attestor, extender, 55)
		result, err := ExtenderLatencyReport(&ExtenderLatencyReportArgs{
			Attestations: []*ExtenderLatencyAttestationArgs{cases["altered rtt"](), good, nil},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 2)
	})
}

func TestExtenderLatencyReportBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("127.0.0.1", "443"))

		// an empty report is fine and does nothing
		result, err := ExtenderLatencyReport(&ExtenderLatencyReportArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 0)

		// too many is refused whole
		attestations := make([]*ExtenderLatencyAttestationArgs, ExtenderLatencyReportMaxCount+1)
		for i := range attestations {
			attestations[i] = &ExtenderLatencyAttestationArgs{}
		}
		result, err = ExtenderLatencyReport(&ExtenderLatencyReportArgs{Attestations: attestations}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("an oversized report was not refused")
		}

		// the rate limit holds the caller after its budget
		for i := 0; i < ExtenderLatencyReportRateLimit; i += 1 {
			result, err = ExtenderLatencyReport(&ExtenderLatencyReportArgs{
				Attestations: []*ExtenderLatencyAttestationArgs{{}},
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, "")
		}
		result, err = ExtenderLatencyReport(&ExtenderLatencyReportArgs{
			Attestations: []*ExtenderLatencyAttestationArgs{{}},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("the rate limit did not hold")
		}
	})
}

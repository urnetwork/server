package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The ping report (connect/GEOMAP.md §2.4, §2.5) against the real key store,
// the real extender table and the real signatures: every claim here is signed
// by its pinger's key and every co-signature by its target's, through the
// same connect helpers the pinger and the target use.

// An extender identity: its row, and the key it signs with.
type testPingExtender struct {
	extender   *model.NetworkExtender
	privateKey ed25519.PrivateKey
}

// An active extender owned by `clientId`, with a fresh identity key.
func newTestPingExtender(t testing.TB, ctx context.Context, clientId server.Id) *testPingExtender {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	extender := &model.NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   server.NewId(),
		ClientId:    clientId,
		PublicKey:   privateKey.Public().(ed25519.PublicKey),
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     connect.ExtenderDnsPort,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}
	model.Testing_CreateNetworkExtender(ctx, extender, nil)
	return &testPingExtender{
		extender:   extender,
		privateKey: privateKey,
	}
}

// The attestor of the extender as a pinger: its identity key, signing only the
// peer probe domain.
func (self *testPingExtender) attestor() *connect.ExtenderProbeAttestor {
	return connect.NewExtenderProbeExtenderAttestor(
		self.extender.PublicKey,
		connect.NewExtenderPeerProbeSigner(self.privateKey),
	)
}

// The extender's co-signature over a claim it accepted, as its verdict frame
// carries it.
func (self *testPingExtender) cosign(t testing.TB, attestation *protocol.ExtenderProbeAttestation) *protocol.ExtenderProbeVerdict {
	t.Helper()
	cosignature, err := connect.SignExtenderProbeVerdict(
		func(data []byte) []byte {
			return ed25519.Sign(self.privateKey, data)
		},
		attestation,
	)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.ExtenderProbeVerdict{
		Accepted:    true,
		Cosignature: cosignature,
	}
}

// A provider with a registered client key: the session it reports over, and
// the attestor its probes sign with.
func newTestPingProvider(t testing.TB, ctx context.Context) (*session.ClientSession, *connect.ExtenderProbeAttestor) {
	t.Helper()
	clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("192.0.2.1", "443"))
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	model.SetClientPublicKey(ctx, *clientSession.ByJwt.ClientId, publicKey)
	return clientSession, connect.NewExtenderProbeProviderAttestor(
		connect.Id(*clientSession.ByJwt.ClientId),
		func(data []byte) []byte {
			return ed25519.Sign(privateKey, data)
		},
	)
}

// A claim of the attestor against the target, signed, with the given round
// trip and timestamp.
func newTestPingAttestation(
	t testing.TB,
	attestor *connect.ExtenderProbeAttestor,
	targetPublicKey []byte,
	rttMs uint32,
	timestampMs uint64,
) *protocol.ExtenderProbeAttestation {
	t.Helper()
	nonce, err := connect.NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	attestation := &protocol.ExtenderProbeAttestation{
		ExtenderPublicKey: targetPublicKey,
		ProbeNonce:        nonce,
		RttMs:             rttMs,
		TimestampMs:       timestampMs,
	}
	switch attestor.Kind() {
	case connect.ExtenderPingerKindProvider:
		attestation.ProbeClientId = attestor.ClientId.Bytes()
	case connect.ExtenderPingerKindExtender:
		attestation.PingerExtenderPublicKey = attestor.ExtenderPublicKey
	}
	if err := connect.SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	return attestation
}

// The report entry a pinger posts for a claim and what became of it, built by
// the pinger's own transport so the json is exactly what the field sends.
func testPingArgs(
	attestation *protocol.ExtenderProbeAttestation,
	outcome connect.ExtenderPingOutcome,
	verdict *protocol.ExtenderProbeVerdict,
) *ExtenderPingArgs {
	report := connect.ExtenderPingReportFromProto(attestation, outcome, verdict)
	return &ExtenderPingArgs{
		PingerKind:                 string(report.PingerKind),
		PingerClientId:             report.PingerClientId,
		PingerExtenderPublicKeyHex: report.PingerExtenderPublicKeyHex,
		TargetExtenderPublicKeyHex: report.TargetExtenderPublicKeyHex,
		ProbeNonce:                 report.ProbeNonce,
		RttMs:                      report.RttMs,
		TimestampMs:                report.TimestampMs,
		Signature:                  report.Signature,
		Outcome:                    string(report.Outcome),
		Reason:                     report.Reason,
		Cosignature:                report.Cosignature,
	}
}

// The operator's clock now, in the milliseconds a claim's timestamp carries.
func testPingNowMs() uint64 {
	return uint64(server.NowUtc().UnixMilli())
}

// The one stored ping of a target, or a failure.
func requireOneTestPing(t testing.TB, ctx context.Context, targetExtenderId server.Id) *model.NetworkPing {
	t.Helper()
	pings := model.GetNetworkPings(ctx, targetExtenderId, time.Time{})
	if len(pings) != 1 {
		t.Fatalf("target has %d stored pings, want 1", len(pings))
	}
	return pings[0]
}

// A provider's co-signed ping is stored as a measurement: the pinger is the
// reporting client, the operator's verdict is co-signed because the
// co-signature verifies under the target's stored key, and both signatures
// are kept.
func TestExtenderPingReportStoresACosignedProviderPing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())

		attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 37, testPingNowMs())
		verdict := target.cosign(t, attestation)
		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(attestation, connect.ExtenderPingCosigned, verdict)},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 0)

		ping := requireOneTestPing(t, ctx, target.extender.ExtenderId)
		connect.AssertEqual(t, ping.PingerKind, model.NetworkPingPingerKindProvider)
		connect.AssertEqual(t, ping.PingerId, *clientSession.ByJwt.ClientId)
		connect.AssertEqual(t, ping.RttMs, 37)
		connect.AssertEqual(t, ping.ProbeTime.UnixMilli(), int64(attestation.TimestampMs))
		connect.AssertEqual(t, ping.Cosign, model.NetworkPingCosignCosigned)
		connect.AssertEqual(t, ping.CosignReason, 0)
		connect.AssertEqual(t, ping.HopCount, 0)
		connect.AssertEqual(t, hex.EncodeToString(ping.ProbeNonce), hex.EncodeToString(attestation.ProbeNonce))
		connect.AssertEqual(t, hex.EncodeToString(ping.PingerSignature), hex.EncodeToString(attestation.Signature))
		connect.AssertEqual(t, hex.EncodeToString(ping.Cosignature), hex.EncodeToString(verdict.Cosignature))
		// the stored pair re-verifies from the row alone, which is what lets a
		// dispute be shown rather than argued
		connect.AssertEqual(t, connect.VerifyExtenderProbeVerdict(
			ed25519.PublicKey(target.extender.PublicKey),
			attestation,
			&protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: ping.Cosignature},
		), true)

		// a co-signed direct ping is a solver term
		terms := []*model.NetworkPingTerm{}
		model.GetNetworkPingTerms(ctx, time.Time{}, func(term *model.NetworkPingTerm) {
			terms = append(terms, term)
		})
		connect.AssertEqual(t, len(terms), 1)
		connect.AssertEqual(t, terms[0].PingerId, *clientSession.ByJwt.ClientId)
		connect.AssertEqual(t, terms[0].TargetExtenderId, target.extender.ExtenderId)
		connect.AssertEqual(t, terms[0].RttMs, 37)
	})
}

// An extender reports its own pings of its peers over its activation
// credential, and they are stored against its extender id.
func TestExtenderPingReportStoresACosignedExtenderPing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("192.0.2.1", "443"))
		pinger := newTestPingExtender(t, ctx, *clientSession.ByJwt.ClientId)
		target := newTestPingExtender(t, ctx, server.NewId())

		attestation := newTestPingAttestation(t, pinger.attestor(), target.extender.PublicKey, 12, testPingNowMs())
		args := testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))
		connect.AssertEqual(t, args.PingerKind, string(connect.ExtenderPingerKindExtender))
		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{args},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 0)

		ping := requireOneTestPing(t, ctx, target.extender.ExtenderId)
		connect.AssertEqual(t, ping.PingerKind, model.NetworkPingPingerKindExtender)
		connect.AssertEqual(t, ping.PingerId, pinger.extender.ExtenderId)
		connect.AssertEqual(t, ping.Cosign, model.NetworkPingCosignCosigned)
		connect.AssertEqual(t, ping.RttMs, 12)
	})
}

// The verdict is recomputed, never taken from the report: a claim of
// co-signed whose co-signature does not verify is stored refused with the
// bad signature reason; a refusal keeps the pinger's reason; a missing verdict
// is unknown with no reason. None of them is a solver term.
func TestExtenderPingReportRecomputesTheVerdict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())
		impostor := newTestPingExtender(t, ctx, server.NewId())

		// co-signed by another extender's key: it verifies under nobody the
		// report can name
		forged := newTestPingAttestation(t, attestor, target.extender.PublicKey, 40, testPingNowMs())
		forgedArgs := testPingArgs(forged, connect.ExtenderPingCosigned, impostor.cosign(t, forged))
		// the target's real co-signature, moved onto another claim
		moved := newTestPingAttestation(t, attestor, target.extender.PublicKey, 41, testPingNowMs())
		other := newTestPingAttestation(t, attestor, target.extender.PublicKey, 42, testPingNowMs())
		movedArgs := testPingArgs(moved, connect.ExtenderPingCosigned, target.cosign(t, other))
		// a refusal, with the target's reason
		refused := newTestPingAttestation(t, attestor, target.extender.PublicKey, 43, testPingNowMs())
		refusedArgs := testPingArgs(refused, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{
			Reason: connect.ExtenderProbeVerdictReasonRttBelowObserved,
		})
		// no verdict at all
		silent := newTestPingAttestation(t, attestor, target.extender.PublicKey, 44, testPingNowMs())
		silentArgs := testPingArgs(silent, connect.ExtenderPingUnknown, nil)

		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{forgedArgs, movedArgs, refusedArgs, silentArgs},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 4)
		connect.AssertEqual(t, result.Rejected, 0)

		rttPings := map[int]*model.NetworkPing{}
		for _, ping := range model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{}) {
			rttPings[ping.RttMs] = ping
		}
		connect.AssertEqual(t, len(rttPings), 4)
		for _, rttMs := range []int{40, 41} {
			connect.AssertEqual(t, rttPings[rttMs].Cosign, model.NetworkPingCosignRejected)
			connect.AssertEqual(t, rttPings[rttMs].CosignReason, int(connect.ExtenderProbeVerdictReasonBadSignature))
			connect.AssertEqual(t, len(rttPings[rttMs].Cosignature), 0)
		}
		connect.AssertEqual(t, rttPings[43].Cosign, model.NetworkPingCosignRejected)
		connect.AssertEqual(t, rttPings[43].CosignReason, int(connect.ExtenderProbeVerdictReasonRttBelowObserved))
		connect.AssertEqual(t, rttPings[44].Cosign, model.NetworkPingCosignUnknown)
		connect.AssertEqual(t, rttPings[44].CosignReason, 0)

		terms := 0
		model.GetNetworkPingTerms(ctx, time.Time{}, func(term *model.NetworkPingTerm) {
			terms += 1
		})
		connect.AssertEqual(t, terms, 0)
		refusals := []*model.NetworkPingRefusal{}
		model.GetNetworkPingRefusals(ctx, time.Time{}, func(refusal *model.NetworkPingRefusal) {
			refusals = append(refusals, refusal)
		})
		connect.AssertEqual(t, len(refusals), 3)
	})
}

// A relayed ping (GEOMAP §2.9) is stored with its depth and counted, but is
// never a solver term: its round trip includes the detour through the front.
func TestExtenderPingReportStoresTheHopCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())

		attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 90, testPingNowMs())
		args := testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))
		args.HopCount = 2
		// a depth no chain has, which the column could not hold
		deep := newTestPingAttestation(t, attestor, target.extender.PublicKey, 91, testPingNowMs())
		deepArgs := testPingArgs(deep, connect.ExtenderPingUnknown, nil)
		deepArgs.HopCount = math.MaxInt16 + 1

		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{args, deepArgs},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 1)

		ping := requireOneTestPing(t, ctx, target.extender.ExtenderId)
		connect.AssertEqual(t, ping.HopCount, 2)
		connect.AssertEqual(t, ping.Cosign, model.NetworkPingCosignCosigned)
		terms := 0
		model.GetNetworkPingTerms(ctx, time.Time{}, func(term *model.NetworkPingTerm) {
			terms += 1
		})
		connect.AssertEqual(t, terms, 0)
		// the dashboard still counts it, as relayed
		counts := model.CountExtenderPings(ctx, server.NowUtc())
		connect.AssertEqual(t, counts.Pings24h, int64(1))
		for _, outcome := range counts.Outcomes24h {
			want := int64(0)
			if outcome.PingerKind == model.NetworkPingPingerKindProvider &&
				outcome.Cosign == model.NetworkPingCosignCosigned &&
				outcome.Relayed {
				want = 1
			}
			connect.AssertEqual(t, outcome.Pings, want)
		}
	})
}

// Every claim must name the reporting client as its pinger and a target that
// is not its own; each failure is counted and none stored.
func TestExtenderPingReportRejectsWhatIsNotTheCallers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		reporterClientId := *clientSession.ByJwt.ClientId
		target := newTestPingExtender(t, ctx, server.NewId())
		// the reporting client's own extender, and an extender of another
		// client the caller would like to speak for
		ownExtender := newTestPingExtender(t, ctx, reporterClientId)
		foreignExtender := newTestPingExtender(t, ctx, server.NewId())
		// another provider, registered, whose pings the caller is not
		otherSession, otherAttestor := newTestPingProvider(t, ctx)

		nameArgsBuilders := map[string]func() *ExtenderPingArgs{
			"another provider's ping": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, otherAttestor, target.extender.PublicKey, 50, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))
			},
			"another client's extender's ping": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, foreignExtender.attestor(), target.extender.PublicKey, 50, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))
			},
			"a provider pinging its own extender": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, attestor, ownExtender.extender.PublicKey, 50, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingCosigned, ownExtender.cosign(t, attestation))
			},
			"an extender pinging itself": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, ownExtender.attestor(), ownExtender.extender.PublicKey, 50, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingCosigned, ownExtender.cosign(t, attestation))
			},
			"an unknown target": func() *ExtenderPingArgs {
				unknownKey, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				attestation := newTestPingAttestation(t, attestor, unknownKey, 50, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
		}
		for name, build := range nameArgsBuilders {
			result, err := ExtenderPingReport(&ExtenderPingReportArgs{
				Pings: []*ExtenderPingArgs{build()},
			}, clientSession)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if result.Error != "" || result.Accepted != 0 || result.Rejected != 1 {
				t.Fatalf("%s: accepted=%d rejected=%d error=%q", name, result.Accepted, result.Rejected, result.Error)
			}
		}
		for _, extender := range []*testPingExtender{target, ownExtender, foreignExtender} {
			connect.AssertEqual(t, len(model.GetNetworkPings(ctx, extender.extender.ExtenderId, time.Time{})), 0)
		}

		// the same claims are accepted from the client that is their pinger
		attestation := newTestPingAttestation(t, otherAttestor, target.extender.PublicKey, 50, testPingNowMs())
		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))},
		}, otherSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
	})
}

// A claim the pinger did not sign as it arrives is rejected: an altered
// number, a missing or foreign signature, a malformed field, a timestamp out
// of the window. And a claim already stored is a replay, not a sample.
func TestExtenderPingReportRejectsWhatDoesNotVerify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())
		_, strangerPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		// the caller's client id under a key that is not the registered one
		stranger := connect.NewExtenderProbeProviderAttestor(
			attestor.ClientId,
			func(data []byte) []byte {
				return ed25519.Sign(strangerPrivateKey, data)
			},
		)

		fresh := func(rttMs uint32) *protocol.ExtenderProbeAttestation {
			return newTestPingAttestation(t, attestor, target.extender.PublicKey, rttMs, testPingNowMs())
		}
		nameArgsBuilders := map[string]func() *ExtenderPingArgs{
			"altered rtt": func() *ExtenderPingArgs {
				attestation := fresh(100)
				args := testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
				args.RttMs = 1
				return args
			},
			"altered timestamp": func() *ExtenderPingArgs {
				attestation := fresh(100)
				args := testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
				args.TimestampMs += 1
				return args
			},
			"no signature": func() *ExtenderPingArgs {
				attestation := fresh(100)
				attestation.Signature = nil
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"signed by another key": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, stranger, target.extender.PublicKey, 100, testPingNowMs())
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"a timestamp two days old": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 100, uint64(server.NowUtc().Add(-48*time.Hour).UnixMilli()))
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"a timestamp two days ahead": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 100, uint64(server.NowUtc().Add(48*time.Hour).UnixMilli()))
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"no timestamp": func() *ExtenderPingArgs {
				attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 100, 0)
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"unstorable rtt": func() *ExtenderPingArgs {
				attestation := fresh(math.MaxUint32)
				return testPingArgs(attestation, connect.ExtenderPingUnknown, nil)
			},
			"unknown outcome": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingUnknown, nil)
				args.Outcome = "accepted"
				return args
			},
			"unreadable cosignature": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingUnknown, nil)
				args.Cosignature = "!"
				return args
			},
			"unstorable reason": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{})
				args.Reason = math.MaxInt16 + 1
				return args
			},
			"unreadable nonce": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingUnknown, nil)
				args.ProbeNonce = "!"
				return args
			},
			"short nonce": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingUnknown, nil)
				args.ProbeNonce = base64.StdEncoding.EncodeToString(make([]byte, 16))
				return args
			},
			"a kind the identity contradicts": func() *ExtenderPingArgs {
				args := testPingArgs(fresh(100), connect.ExtenderPingUnknown, nil)
				args.PingerKind = string(connect.ExtenderPingerKindExtender)
				return args
			},
		}
		for name, build := range nameArgsBuilders {
			result, err := ExtenderPingReport(&ExtenderPingReportArgs{
				Pings: []*ExtenderPingArgs{build()},
			}, clientSession)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if result.Error != "" || result.Accepted != 0 || result.Rejected != 1 {
				t.Fatalf("%s: accepted=%d rejected=%d error=%q", name, result.Accepted, result.Rejected, result.Error)
			}
		}
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{})), 0)

		// a replay of a stored claim is rejected, and is not a second row
		attestation := fresh(60)
		args := testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))
		for i, wantAccepted := range []int{1, 0} {
			result, err := ExtenderPingReport(&ExtenderPingReportArgs{
				Pings: []*ExtenderPingArgs{args},
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			if result.Accepted != wantAccepted || result.Rejected != 1-wantAccepted {
				t.Fatalf("post %d: accepted=%d rejected=%d", i, result.Accepted, result.Rejected)
			}
		}
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{})), 1)

		// a mixed report stores what verifies and counts the rest
		good := fresh(61)
		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{
				nameArgsBuilders["altered rtt"](),
				testPingArgs(good, connect.ExtenderPingCosigned, target.cosign(t, good)),
				nil,
			},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 2)
	})
}

// Uncosigned claims are capped per post (D1): past the cap they count as
// rejected, and a co-signed ping is never capped.
func TestExtenderPingReportCapsTheUncosignedPerPost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())

		maxUncosignedPerPost := DefaultExtenderPingReportSettings().MaxUncosignedPerPost
		pings := []*ExtenderPingArgs{}
		extra := 5
		for i := range maxUncosignedPerPost + extra {
			attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, uint32(100+i), testPingNowMs())
			outcome := connect.ExtenderPingUnknown
			var verdict *protocol.ExtenderProbeVerdict
			if i%2 == 0 {
				outcome = connect.ExtenderPingRejected
				verdict = &protocol.ExtenderProbeVerdict{Reason: connect.ExtenderProbeVerdictReasonNonce}
			}
			pings = append(pings, testPingArgs(attestation, outcome, verdict))
		}
		// co-signed pings after the cap is reached still go in
		cosignedCount := 3
		for i := range cosignedCount {
			attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, uint32(10+i), testPingNowMs())
			pings = append(pings, testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation)))
		}

		result, err := ExtenderPingReport(&ExtenderPingReportArgs{Pings: pings}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, maxUncosignedPerPost+cosignedCount)
		connect.AssertEqual(t, result.Rejected, extra)

		uncosigned := 0
		cosigned := 0
		for _, ping := range model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{}) {
			if ping.Cosign == model.NetworkPingCosignCosigned {
				cosigned += 1
			} else {
				uncosigned += 1
			}
		}
		connect.AssertEqual(t, uncosigned, maxUncosignedPerPost)
		connect.AssertEqual(t, cosigned, cosignedCount)
	})
}

// A report is refused whole beyond its size, and the caller is held after its
// hourly budget; an empty report costs nothing.
func TestExtenderPingReportBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultExtenderPingReportSettings()
		clientSession := newTestExtenderSession(t, ctx, net.JoinHostPort("192.0.2.1", "443"))

		result, err := ExtenderPingReport(&ExtenderPingReportArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, result.Rejected, 0)

		pings := make([]*ExtenderPingArgs, settings.MaxCount+1)
		for i := range pings {
			pings[i] = &ExtenderPingArgs{}
		}
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{Pings: pings}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("an oversized report was not refused")
		}
		connect.AssertEqual(t, result.Accepted, 0)

		// exactly the cap is a report
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{Pings: pings[:settings.MaxCount]}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Rejected, settings.MaxCount)

		// the rate limit holds the caller after its budget, the post above
		// included
		for i := 1; i < settings.RateLimit; i += 1 {
			result, err = ExtenderPingReport(&ExtenderPingReportArgs{
				Pings: []*ExtenderPingArgs{{}},
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, "")
		}
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{{}},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("the rate limit did not hold")
		}

		// a session without a client is refused before anything else
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{{}},
		}, session.NewLocalClientSession(ctx, "192.0.2.1:443", nil))
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("a report without a client was not refused")
		}
	})
}

// The dashboard counts see every kind, verdict and relay, and the hourly
// feed counts pings and refusals per target and pinger kind.
func TestExtenderPingReportFeedsTheDashboardCounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		providerSession, providerAttestor := newTestPingProvider(t, ctx)
		extenderSession := newTestExtenderSession(t, ctx, net.JoinHostPort("192.0.2.1", "443"))
		pinger := newTestPingExtender(t, ctx, *extenderSession.ByJwt.ClientId)
		target := newTestPingExtender(t, ctx, server.NewId())

		provided := newTestPingAttestation(t, providerAttestor, target.extender.PublicKey, 20, testPingNowMs())
		refused := newTestPingAttestation(t, providerAttestor, target.extender.PublicKey, 21, testPingNowMs())
		result, err := ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{
				testPingArgs(provided, connect.ExtenderPingCosigned, target.cosign(t, provided)),
				testPingArgs(refused, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{
					Reason: connect.ExtenderProbeVerdictReasonRttBelowObserved,
				}),
			},
		}, providerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 2)
		peered := newTestPingAttestation(t, pinger.attestor(), target.extender.PublicKey, 7, testPingNowMs())
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(peered, connect.ExtenderPingCosigned, target.cosign(t, peered))},
		}, extenderSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)

		counts := model.CountExtenderPings(ctx, server.NowUtc())
		connect.AssertEqual(t, counts.Pings24h, int64(3))
		connect.AssertEqual(t, counts.Targets24h, int64(1))
		connect.AssertEqual(t, len(counts.Outcomes24h), len(model.NetworkPingPingerKinds)*len(model.NetworkPingCosigns)*2)
		kindSources := map[int]int64{}
		for _, source := range counts.Sources24h {
			kindSources[source.PingerKind] = source.Sources
		}
		connect.AssertEqual(t, kindSources[model.NetworkPingPingerKindProvider], int64(1))
		connect.AssertEqual(t, kindSources[model.NetworkPingPingerKindExtender], int64(1))

		// the hour the rows were stored in, read back rather than assumed, so
		// a report that straddles the turn of an hour is not a false failure
		storedHours := map[time.Time]bool{}
		for _, ping := range model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{}) {
			storedHours[ping.CreateTime.UTC().Truncate(time.Hour)] = true
		}
		kindHourCounts := map[int]model.ExtenderHourPingCount{}
		for hour := range storedHours {
			for _, count := range model.CountExtenderPingsByHour(ctx, hour) {
				connect.AssertEqual(t, count.ExtenderId, target.extender.ExtenderId)
				summed := kindHourCounts[count.PingerKind]
				summed.ExtenderId = count.ExtenderId
				summed.PingerKind = count.PingerKind
				summed.Pings += count.Pings
				summed.Rejections += count.Rejections
				kindHourCounts[count.PingerKind] = summed
			}
		}
		connect.AssertEqual(t, kindHourCounts[model.NetworkPingPingerKindProvider].Pings, int64(2))
		connect.AssertEqual(t, kindHourCounts[model.NetworkPingPingerKindProvider].Rejections, int64(1))
		connect.AssertEqual(t, kindHourCounts[model.NetworkPingPingerKindExtender].Pings, int64(1))
		connect.AssertEqual(t, kindHourCounts[model.NetworkPingPingerKindExtender].Rejections, int64(0))
		if counts := model.CountExtenderPingsByHour(ctx, server.NowUtc().Add(-3*time.Hour)); len(counts) != 0 {
			t.Fatalf("an empty hour counted %v", counts)
		}
	})
}

// The verdict recomputation reads no database: only a co-signature that
// verifies under the target's key, over exactly this claim, makes a ping
// co-signed, whatever the pinger says.
func TestRecomputePingVerdict(t *testing.T) {
	_, providerPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attestor := connect.NewExtenderProbeProviderAttestor(connect.NewId(), func(data []byte) []byte {
		return ed25519.Sign(providerPrivateKey, data)
	})
	targetPublicKey, targetPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cosign := func(privateKey ed25519.PrivateKey, attestation *protocol.ExtenderProbeAttestation) []byte {
		cosignature, err := connect.SignExtenderProbeVerdict(func(data []byte) []byte {
			return ed25519.Sign(privateKey, data)
		}, attestation)
		if err != nil {
			t.Fatal(err)
		}
		return cosignature
	}
	attestation := newTestPingAttestation(t, attestor, targetPublicKey, 30, testPingNowMs())
	other := newTestPingAttestation(t, attestor, targetPublicKey, 31, testPingNowMs())
	valid := cosign(targetPrivateKey, attestation)

	for _, test := range []struct {
		name        string
		outcome     connect.ExtenderPingOutcome
		reason      uint32
		cosignature []byte
		cosign      int
		reasonOut   int
		kept        bool
	}{
		{name: "cosigned", outcome: connect.ExtenderPingCosigned, cosignature: valid, cosign: model.NetworkPingCosignCosigned, reasonOut: 0, kept: true},
		// the co-signature decides, not the pinger's word
		{name: "a valid co-signature on a claimed refusal", outcome: connect.ExtenderPingRejected, reason: 3, cosignature: valid, cosign: model.NetworkPingCosignCosigned, reasonOut: 0, kept: true},
		{name: "a valid co-signature on a claimed silence", outcome: connect.ExtenderPingUnknown, cosignature: valid, cosign: model.NetworkPingCosignCosigned, reasonOut: 0, kept: true},
		{name: "co-signed by another key", outcome: connect.ExtenderPingCosigned, cosignature: cosign(otherPrivateKey, attestation), cosign: model.NetworkPingCosignRejected, reasonOut: int(connect.ExtenderProbeVerdictReasonBadSignature)},
		{name: "co-signed over another claim", outcome: connect.ExtenderPingCosigned, cosignature: cosign(targetPrivateKey, other), cosign: model.NetworkPingCosignRejected, reasonOut: int(connect.ExtenderProbeVerdictReasonBadSignature)},
		{name: "claimed co-signed with none", outcome: connect.ExtenderPingCosigned, cosign: model.NetworkPingCosignRejected, reasonOut: int(connect.ExtenderProbeVerdictReasonBadSignature)},
		{name: "refused", outcome: connect.ExtenderPingRejected, reason: 2, cosign: model.NetworkPingCosignRejected, reasonOut: 2},
		{name: "refused with a bad co-signature keeps the reason", outcome: connect.ExtenderPingRejected, reason: 4, cosignature: cosign(otherPrivateKey, attestation), cosign: model.NetworkPingCosignRejected, reasonOut: 4},
		{name: "unknown", outcome: connect.ExtenderPingUnknown, reason: 6, cosign: model.NetworkPingCosignUnknown, reasonOut: 0},
	} {
		cosignOut, reasonOut, kept := recomputePingVerdict(targetPublicKey, attestation, test.outcome, test.reason, test.cosignature)
		if cosignOut != test.cosign || reasonOut != test.reasonOut || (0 < len(kept)) != test.kept {
			t.Errorf("%s: cosign=%d reason=%d kept=%t, want cosign=%d reason=%d kept=%t",
				test.name, cosignOut, reasonOut, 0 < len(kept), test.cosign, test.reasonOut, test.kept)
		}
	}
}

// The timestamp window is a day either side of now, and nothing outside what
// the column can hold passes.
func TestPingTimestampInWindow(t *testing.T) {
	settings := DefaultExtenderPingReportSettings()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) uint64 {
		return uint64(now.Add(offset).UnixMilli())
	}
	// one window case: the timestamp and whether it is in
	type windowCase struct {
		name        string
		timestampMs uint64
		want        bool
	}
	cases := []windowCase{
		{name: "now", timestampMs: ms(0), want: true},
		{name: "the backward skew ago", timestampMs: ms(-settings.MaxBackwardClockSkew), want: true},
		{name: "just past the backward skew ago", timestampMs: ms(-settings.MaxBackwardClockSkew - time.Millisecond), want: false},
		{name: "the forward skew ahead", timestampMs: ms(settings.MaxForwardClockSkew), want: true},
		{name: "just past the forward skew ahead", timestampMs: ms(settings.MaxForwardClockSkew + time.Millisecond), want: false},
		{name: "the backward skew ahead", timestampMs: ms(settings.MaxBackwardClockSkew), want: false},
		{name: "zero", timestampMs: 0, want: false},
		{name: "past int64", timestampMs: math.MaxInt64 + 1, want: false},
		{name: "the top", timestampMs: math.MaxUint64, want: false},
	}
	for _, c := range cases {
		if got := pingTimestampInWindow(c.timestampMs, now, settings.MaxBackwardClockSkew, settings.MaxForwardClockSkew); got != c.want {
			t.Errorf("%s: in window = %t, want %t", c.name, got, c.want)
		}
	}
}

// The report entry decodes exactly as the pinger's transport encodes it, and
// refuses what the signing bytes could not cover before any lookup.
func TestExtenderPingArgsDecode(t *testing.T) {
	_, providerPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attestor := connect.NewExtenderProbeProviderAttestor(connect.NewId(), func(data []byte) []byte {
		return ed25519.Sign(providerPrivateKey, data)
	})
	targetPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attestation := newTestPingAttestation(t, attestor, targetPublicKey, 25, testPingNowMs())

	args := testPingArgs(attestation, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{Reason: 1})
	decoded, err := args.proto()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, hex.EncodeToString(decoded.ProbeClientId), hex.EncodeToString(attestation.ProbeClientId))
	connect.AssertEqual(t, hex.EncodeToString(decoded.Signature), hex.EncodeToString(attestation.Signature))
	connect.AssertEqual(t, decoded.RttMs, attestation.RttMs)
	outcome, reason, cosignature, ok := args.verdict()
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, outcome, connect.ExtenderPingRejected)
	connect.AssertEqual(t, reason, uint32(1))
	connect.AssertEqual(t, len(cosignature), 0)

	for name, mutate := range map[string]func(args *ExtenderPingArgs){
		"unknown kind": func(args *ExtenderPingArgs) {
			args.PingerKind = "consumer"
		},
		"both identities": func(args *ExtenderPingArgs) {
			args.PingerExtenderPublicKeyHex = hex.EncodeToString(targetPublicKey)
		},
		"short target key": func(args *ExtenderPingArgs) {
			args.TargetExtenderPublicKeyHex = hex.EncodeToString(targetPublicKey[:16])
		},
		"short nonce": func(args *ExtenderPingArgs) {
			args.ProbeNonce = base64.StdEncoding.EncodeToString(make([]byte, 31))
		},
		"unreadable client id": func(args *ExtenderPingArgs) {
			args.PingerClientId = "not-an-id"
		},
		"unreadable signature": func(args *ExtenderPingArgs) {
			args.Signature = "!"
		},
		"unreadable target key": func(args *ExtenderPingArgs) {
			args.TargetExtenderPublicKeyHex = "zz"
		},
		"no pinger kind": func(args *ExtenderPingArgs) {
			args.PingerKind = ""
		},
	} {
		mutated := *args
		mutate(&mutated)
		if _, err := mutated.proto(); err == nil {
			t.Errorf("%s decoded", name)
		}
	}

	for name, mutate := range map[string]func(args *ExtenderPingArgs){
		"unknown outcome": func(args *ExtenderPingArgs) {
			args.Outcome = "accepted"
		},
		"no outcome": func(args *ExtenderPingArgs) {
			args.Outcome = ""
		},
		"unstorable reason": func(args *ExtenderPingArgs) {
			args.Reason = math.MaxInt16 + 1
		},
		"unreadable cosignature": func(args *ExtenderPingArgs) {
			args.Cosignature = "!"
		},
	} {
		mutated := *args
		mutate(&mutated)
		if _, _, _, ok := mutated.verdict(); ok {
			t.Errorf("%s decoded", name)
		}
	}
}

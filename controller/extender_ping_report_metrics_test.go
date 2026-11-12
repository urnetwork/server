package controller

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The ping report's refusals (extender_ping_report_metrics.go): every claim
// the route refuses before storing is counted by why. Before them a refused
// claim was only a number in the reporter's own result, so an extender slot
// whose reports are refused as another client's -- released miners share one
// key across slots -- left no trace on the operator's side.

// Every series the route can count exists at zero from the start, and no
// other: a reason that is not one of the route's own adds nothing.
func TestExtenderPingReportMetricsAreFixedCardinality(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newExtenderPingReportMetrics(registry)
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.refused), 11)
	for _, reason := range pingReportRefusalReasons {
		connect.AssertEqual(t, testutil.ToFloat64(metrics.refused.WithLabelValues(reason)), float64(0))
	}

	metrics.observe(pingReportRefusalCounts{
		"a reporter's word":                3,
		pingReportRefusalReplay:            2,
		pingReportRefusalPingerNotReporter: 1,
	})
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.refused), 11)
	connect.AssertEqual(t, testutil.ToFloat64(metrics.refused.WithLabelValues(pingReportRefusalReplay)), float64(2))
	connect.AssertEqual(t, testutil.ToFloat64(metrics.refused.WithLabelValues(pingReportRefusalPingerNotReporter)), float64(1))

	// no metrics count nothing
	var none *extenderPingReportMetrics
	none.observe(pingReportRefusalCounts{pingReportRefusalReplay: 1})
}

// The fixed clock of the verification tests.
var testPingReportNow = time.Date(2026, 11, 11, 21, 0, 0, 0, time.UTC)

// An identity from a fixed seed, activated by `clientId`, as the extender
// table would return it.
func newFixedTestPingExtender(seedByte byte, clientId server.Id) *testPingExtender {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	return &testPingExtender{
		extender: &model.NetworkExtender{
			ExtenderId: server.Id{0xe0, seedByte},
			NetworkId:  server.Id{0xa0, seedByte},
			ClientId:   clientId,
			PublicKey:  privateKey.Public().(ed25519.PublicKey),
			Active:     true,
		},
		privateKey: privateKey,
	}
}

// A signed claim of the attestor against the target, with a fixed nonce.
func newFixedTestPingAttestation(
	t testing.TB,
	attestor *connect.ExtenderProbeAttestor,
	targetPublicKey []byte,
	nonceByte byte,
	timestamp time.Time,
) *protocol.ExtenderProbeAttestation {
	t.Helper()
	attestation := &protocol.ExtenderProbeAttestation{
		ExtenderPublicKey: targetPublicKey,
		ProbeNonce:        bytes.Repeat([]byte{nonceByte}, connect.ExtenderProbeNonceByteCount),
		RttMs:             40,
		TimestampMs:       uint64(timestamp.UnixMilli()),
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

// Each claim the verification refuses is counted once, under why, and the
// report's rejected is their total: a claim that does not decode, one out of
// time, one whose pinger is another client or one nobody holds a key for, an
// unknown or own target, a signature that does not verify, and the
// uncosigned past the cap. Every reason is one the counter carries, so none
// is dropped. The keys are in memory and every key, nonce and id is fixed.
// Pure.
func TestVerifyPingReportCountsEachRefusalByReason(t *testing.T) {
	reporterClientId := server.Id{0xc0, 1}
	otherClientId := server.Id{0xc0, 2}
	pinger := newFixedTestPingExtender(1, reporterClientId)
	sibling := newFixedTestPingExtender(2, reporterClientId)
	target := newFixedTestPingExtender(3, otherClientId)
	// another client's extender, whose claims the reporter posts: a slot
	// sharing a key the other client activated last
	foreign := newFixedTestPingExtender(4, otherClientId)
	// keys no extender activated under
	unknownPinger := newFixedTestPingExtender(5, reporterClientId)
	unknownTarget := newFixedTestPingExtender(6, otherClientId)
	keys := &memoryPingReportKeys{
		keyHexExtenders: map[string]*model.NetworkExtender{},
	}
	for _, extender := range []*testPingExtender{pinger, sibling, target, foreign} {
		keys.keyHexExtenders[hex.EncodeToString(extender.extender.PublicKey)] = extender.extender
	}
	providerPrivateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	providerSign := func(data []byte) []byte {
		return ed25519.Sign(providerPrivateKey, data)
	}
	// the reporter as a provider, which has no client key registered, and a
	// provider that is not the reporter
	reporterProvider := connect.NewExtenderProbeProviderAttestor(connect.Id(reporterClientId), providerSign)
	otherProvider := connect.NewExtenderProbeProviderAttestor(connect.Id(otherClientId), providerSign)

	settings := DefaultExtenderPingReportSettings()
	settings.MaxUncosignedPerPost = 1
	now := testPingReportNow
	nonceByte := byte(0)
	claim := func(attestor *connect.ExtenderProbeAttestor, targetPublicKey []byte, timestamp time.Time) *protocol.ExtenderProbeAttestation {
		nonceByte += 1
		return newFixedTestPingAttestation(t, attestor, targetPublicKey, nonceByte, timestamp)
	}
	refused := func() *ExtenderPingArgs {
		attestation := claim(pinger.attestor(), target.extender.PublicKey, now)
		return testPingArgs(attestation, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{
			Reason: connect.ExtenderProbeVerdictReasonUnknownPinger,
		})
	}
	cosigned := claim(pinger.attestor(), target.extender.PublicKey, now)

	args := &ExtenderPingReportArgs{}
	add := func(pingArgs *ExtenderPingArgs) {
		args.Pings = append(args.Pings, pingArgs)
	}
	// stored: a co-signed claim, and the one uncosigned the cap allows -- a
	// target's refusal is stored, whatever its reason
	add(testPingArgs(cosigned, connect.ExtenderPingCosigned, target.cosign(t, cosigned)))
	add(refused())
	// uncosigned_cap: the second uncosigned
	add(refused())
	// malformed: nothing, a nonce that does not decode, an outcome that is not
	// one of the three, a round trip and a relay depth no column holds
	add(nil)
	badNonce := refused()
	badNonce.ProbeNonce = "!"
	add(badNonce)
	badOutcome := refused()
	badOutcome.Outcome = "accepted"
	add(badOutcome)
	longRtt := claim(pinger.attestor(), target.extender.PublicKey, now)
	longRtt.RttMs = math.MaxUint32
	if err := connect.SignExtenderProbeAttestation(pinger.attestor(), longRtt); err != nil {
		t.Fatal(err)
	}
	add(testPingArgs(longRtt, connect.ExtenderPingUnknown, nil))
	deepRelay := refused()
	deepRelay.HopCount = math.MaxInt16 + 1
	add(deepRelay)
	// time_window: two days old
	add(testPingArgs(claim(pinger.attestor(), target.extender.PublicKey, now.Add(-48*time.Hour)), connect.ExtenderPingUnknown, nil))
	// pinger_not_reporter: another client's extender key, another client id
	add(testPingArgs(claim(foreign.attestor(), target.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	add(testPingArgs(claim(otherProvider, target.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	// pinger_unknown: a key no extender activated under, a provider without a
	// registered client key
	add(testPingArgs(claim(unknownPinger.attestor(), target.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	add(testPingArgs(claim(reporterProvider, target.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	// target_unknown
	add(testPingArgs(claim(pinger.attestor(), unknownTarget.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	// target_own: the pinger itself, and a sibling identity of the reporter
	add(testPingArgs(claim(pinger.attestor(), pinger.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	add(testPingArgs(claim(pinger.attestor(), sibling.extender.PublicKey, now), connect.ExtenderPingUnknown, nil))
	// bad_signature: a round trip altered after signing
	altered := testPingArgs(claim(pinger.attestor(), target.extender.PublicKey, now), connect.ExtenderPingUnknown, nil)
	altered.RttMs += 1
	add(altered)

	pings, refusals, err := verifyPingReport(args, reporterClientId, now, keys, settings)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(pings), 2)
	connect.AssertEqual(t, pings[0].Cosign, model.NetworkPingCosignCosigned)
	connect.AssertEqual(t, pings[1].Cosign, model.NetworkPingCosignRejected)
	connect.AssertEqual(t, pings[1].CosignReason, int(connect.ExtenderProbeVerdictReasonUnknownPinger))
	connect.AssertEqual(t, map[string]int(refusals), map[string]int{
		pingReportRefusalUncosignedCap:     1,
		pingReportRefusalMalformed:         5,
		pingReportRefusalTimeWindow:        1,
		pingReportRefusalPingerNotReporter: 2,
		pingReportRefusalPingerUnknown:     2,
		pingReportRefusalTargetUnknown:     1,
		pingReportRefusalTargetOwn:         2,
		pingReportRefusalBadSignature:      1,
	})
	connect.AssertEqual(t, refusals.total(), len(args.Pings)-len(pings))

	// the counts reach the counter as they are
	metrics := newExtenderPingReportMetrics(prometheus.NewRegistry())
	metrics.observe(refusals)
	for reason, count := range refusals {
		connect.AssertEqual(t, testutil.ToFloat64(metrics.refused.WithLabelValues(reason)), float64(count))
	}
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.refused), len(pingReportRefusalReasons))
}

// A claim that verifies is never counted as refused: a report of co-signed
// claims and of claims the targets refused, under the cap, refuses nothing.
// Pure.
func TestVerifyPingReportRefusesNothingValid(t *testing.T) {
	reporterClientId := server.Id{0xc0, 1}
	pinger := newFixedTestPingExtender(1, reporterClientId)
	keys := &memoryPingReportKeys{
		keyHexExtenders: map[string]*model.NetworkExtender{
			hex.EncodeToString(pinger.extender.PublicKey): pinger.extender,
		},
	}
	args := &ExtenderPingReportArgs{}
	for i := byte(0); i < 8; i += 1 {
		target := newFixedTestPingExtender(10+i, server.Id{0xc0, 10 + i})
		keys.keyHexExtenders[hex.EncodeToString(target.extender.PublicKey)] = target.extender
		attestation := newFixedTestPingAttestation(t, pinger.attestor(), target.extender.PublicKey, i+1, testPingReportNow)
		if i%2 == 0 {
			args.Pings = append(args.Pings, testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation)))
		} else {
			args.Pings = append(args.Pings, testPingArgs(attestation, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{
				Reason: connect.ExtenderProbeVerdictReasonRttBelowObserved,
			}))
		}
	}
	pings, refusals, err := verifyPingReport(args, reporterClientId, testPingReportNow, keys, DefaultExtenderPingReportSettings())
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(pings), 8)
	connect.AssertEqual(t, refusals.total(), 0)
	connect.AssertEqual(t, len(refusals), 0)
}

// The refusal counts decode as the wire sends them: a base64 nonce of the
// wrong size is malformed, never a bad signature. Pure.
func TestVerifyPingReportShortNonceIsMalformed(t *testing.T) {
	reporterClientId := server.Id{0xc0, 1}
	pinger := newFixedTestPingExtender(1, reporterClientId)
	target := newFixedTestPingExtender(3, server.Id{0xc0, 2})
	keys := &memoryPingReportKeys{
		keyHexExtenders: map[string]*model.NetworkExtender{
			hex.EncodeToString(pinger.extender.PublicKey): pinger.extender,
			hex.EncodeToString(target.extender.PublicKey): target.extender,
		},
	}
	args := testPingArgs(newFixedTestPingAttestation(t, pinger.attestor(), target.extender.PublicKey, 1, testPingReportNow), connect.ExtenderPingUnknown, nil)
	args.ProbeNonce = base64.StdEncoding.EncodeToString(make([]byte, 16))
	_, refusals, err := verifyPingReport(&ExtenderPingReportArgs{Pings: []*ExtenderPingArgs{args}}, reporterClientId, testPingReportNow, keys, DefaultExtenderPingReportSettings())
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, map[string]int(refusals), map[string]int{pingReportRefusalMalformed: 1})
}

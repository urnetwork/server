package controller

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Extender proximity (connect/DESIGNNOTES4.md): the continent hint a client
// reads before it picks extenders, and the latency report an extender posts
// for the providers that attested their distance to it.
//
// The report is where the operator does the one check neither party to a
// probe could: the provider's signature is verified against the key store,
// so an extender can forward a claim but not invent one, and the extender's
// ownership is verified against the activation, so a claim is attributed to
// an extender the reporting client actually runs.
//
// The latency report is the target-reported ingest, and it is superseded by
// the pinger-reported, co-signed `POST /network/ping-report`
// (extender_ping_controller.go, connect/GEOMAP.md §2.5). It stays accepting
// for one release so extenders on the previous binary keep forwarding their
// providers' claims (D14), and is then removed with its table.

// The action the latency report's rate limit is recorded under.
const ExtenderLatencyReportRateLimitAction = "extender_latency_report"

// The limits of the latency report, and how long what it stores is kept.
type ExtenderLatencyReportSettings struct {
	// reports per user per window. An extender flushes at most every 30 s, so
	// 120 an hour is the honest ceiling; the budget leaves room for a retry
	// storm
	RateLimit       int
	RateLimitWindow time.Duration
	// attestations in one report. The reporter batches 64; anything past this
	// is not a batch
	MaxCount int
	// how long a latency row is kept before the sweep removes it
	Retention time.Duration
}

// The settings the report and its retention sweep run with.
func DefaultExtenderLatencyReportSettings() *ExtenderLatencyReportSettings {
	return &ExtenderLatencyReportSettings{
		RateLimit:       240,
		RateLimitWindow: time.Hour,
		MaxCount:        256,
		Retention:       30 * 24 * time.Hour,
	}
}

// The hint: the continent the operator places the caller's address on, upper
// case, the same mapping the geo dns and the record tag use. Empty when the
// operator cannot place the caller, which a client treats as no hint rather
// than as a continent.
type ExtenderHintResult struct {
	ContinentCode string `json:"continent_code"`
}

// Answers `GET /network/extender-hint` (connect/DESIGNNOTES4.md §4). Nothing
// here can fail the request: an address the operator cannot read or place is
// an empty hint.
func ExtenderHint(clientSession *session.ClientSession) (*ExtenderHintResult, error) {
	result := &ExtenderHintResult{}
	clientIp, _, err := server.SplitClientAddress(clientSession.ClientAddress)
	if err != nil {
		return result, nil
	}
	location, _, err := GetLocationForIp(clientSession.Ctx, clientIp)
	if err != nil {
		if glog.V(2) {
			glog.Infof("[extender]no location for the hint: %s\n", err)
		}
		return result, nil
	}
	result.ContinentCode = model.ContinentCodeForCountry(location.CountryCode)
	return result, nil
}

// One attestation as the report carries it: the fields of the signed message
// (protocol.ExtenderProbeAttestation of a provider, in the json the previous
// extender binary forwards), from which the signing bytes are rebuilt and the
// provider's signature verified.
type ExtenderLatencyAttestationArgs struct {
	// the provider client id
	ClientId             string `json:"client_id"`
	ExtenderPublicKeyHex string `json:"extender_public_key_hex"`
	// base64
	ProbeNonce  string `json:"probe_nonce"`
	RttMs       uint32 `json:"rtt_ms"`
	TimestampMs uint64 `json:"timestamp_ms"`
	// base64 of the provider's ed25519 signature
	Signature string `json:"signature"`
}

// The signed claim the transport form carries. An attestation forwarded here
// is exactly a provider ping claim (connect/GEOMAP.md §2.2 keeps the provider
// format unchanged), so it is decoded by the same transport the ping report
// uses: a field that does not decode is an error here rather than a signature
// that does not verify.
func (self *ExtenderLatencyAttestationArgs) proto() (*protocol.ExtenderProbeAttestation, error) {
	return (&connect.ExtenderPingReport{
		PingerKind:                 connect.ExtenderPingerKindProvider,
		PingerClientId:             self.ClientId,
		TargetExtenderPublicKeyHex: self.ExtenderPublicKeyHex,
		ProbeNonce:                 self.ProbeNonce,
		RttMs:                      self.RttMs,
		TimestampMs:                self.TimestampMs,
		Signature:                  self.Signature,
	}).Proto()
}

// The body of `POST /network/extender-latency`: the attestations an extender
// forwards.
type ExtenderLatencyReportArgs struct {
	Attestations []*ExtenderLatencyAttestationArgs `json:"attestations"`
}

// A refusal of the whole report is a normal answer with `Error`, as an
// activation refusal is. Inside an accepted report, an attestation that does
// not verify, names an extender the caller does not own, or was already
// stored is counted in `Rejected` and not named: the extender can do nothing
// about a provider's bad signature, and the count is what tells a broken
// reporter from a quiet one.
type ExtenderLatencyReportResult struct {
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Error    string `json:"error,omitempty"`
}

// Answers `POST /network/extender-latency` (connect/DESIGNNOTES4.md §3).
//
// Every attestation is checked on its own: it must decode, name an extender
// the calling client activated, name a provider other than that extender's
// own client, and verify under that provider's registered client key. What
// passes is stored; a duplicate of what is already stored is not a second
// sample.
func ExtenderLatencyReport(
	args *ExtenderLatencyReportArgs,
	clientSession *session.ClientSession,
) (*ExtenderLatencyReportResult, error) {
	refuse := func(message string) (*ExtenderLatencyReportResult, error) {
		return &ExtenderLatencyReportResult{
			Error: message,
		}, nil
	}

	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this is the structural guard for a
		// caller that reaches the controller another way
		return refuse("a latency report requires a client")
	}
	if args == nil || len(args.Attestations) == 0 {
		return &ExtenderLatencyReportResult{}, nil
	}
	settings := DefaultExtenderLatencyReportSettings()
	if settings.MaxCount < len(args.Attestations) {
		return refuse(fmt.Sprintf(
			"a latency report carries at most %d attestations",
			settings.MaxCount,
		))
	}
	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
		ExtenderLatencyReportRateLimitAction,
		settings.RateLimit,
		settings.RateLimitWindow,
	); err != nil {
		return refuse(err.Error())
	}
	reporterClientId := *clientSession.ByJwt.ClientId

	// the extenders named, looked up once per key; nil is a key no extender
	// activated under, or one the caller does not own
	keyHexOwnedExtenders := map[string]*model.NetworkExtender{}
	ownedExtender := func(publicKey []byte) *model.NetworkExtender {
		keyHex := hex.EncodeToString(publicKey)
		if extender, ok := keyHexOwnedExtenders[keyHex]; ok {
			return extender
		}
		extender := model.GetNetworkExtenderByPublicKey(clientSession.Ctx, publicKey)
		if extender != nil && extender.ClientId != reporterClientId {
			extender = nil
		}
		keyHexOwnedExtenders[keyHex] = extender
		return extender
	}
	// the provider keys, looked up once per provider
	clientIdProviderKeys := map[server.Id]ed25519.PublicKey{}
	providerKey := func(clientId server.Id) ed25519.PublicKey {
		if publicKey, ok := clientIdProviderKeys[clientId]; ok {
			return publicKey
		}
		var publicKey ed25519.PublicKey
		if keyBytes, err := model.GetClientPublicKey(clientSession.Ctx, clientId); err == nil && len(keyBytes) == ed25519.PublicKeySize {
			publicKey = ed25519.PublicKey(keyBytes)
		}
		clientIdProviderKeys[clientId] = publicKey
		return publicKey
	}

	rejectedCount := 0
	latencies := []*model.NetworkExtenderLatency{}
	for _, attestationArgs := range args.Attestations {
		if attestationArgs == nil {
			rejectedCount += 1
			continue
		}
		attestation, err := attestationArgs.proto()
		if err != nil {
			rejectedCount += 1
			continue
		}
		extender := ownedExtender(attestation.ExtenderPublicKey)
		if extender == nil {
			rejectedCount += 1
			continue
		}
		providerClientId, err := server.IdFromBytes(attestation.ProbeClientId)
		if err != nil {
			rejectedCount += 1
			continue
		}
		if providerClientId == extender.ClientId {
			// an extender attesting its own distance to itself says nothing
			rejectedCount += 1
			continue
		}
		publicKey := providerKey(providerClientId)
		if publicKey == nil || !connect.VerifyExtenderProbeAttestation(publicKey, attestation) {
			rejectedCount += 1
			continue
		}
		if attestation.TimestampMs == 0 {
			rejectedCount += 1
			continue
		}
		// A provider signs any number it likes and the extender's gate only
		// bounds it from below, so an absurd claim reaches here verified. One
		// the columns cannot hold would fail the whole report's insert, and the
		// extender would retry that batch forever; it is rejected instead.
		// the ping report's day on either side, as before its window became
		// asymmetric: these claims predate the forward bound
		maxClockSkew := DefaultExtenderPingReportSettings().MaxBackwardClockSkew
		if math.MaxInt32 < attestation.RttMs || !pingTimestampInWindow(attestation.TimestampMs, server.NowUtc(), maxClockSkew, maxClockSkew) {
			rejectedCount += 1
			continue
		}
		latencies = append(latencies, &model.NetworkExtenderLatency{
			ExtenderId: extender.ExtenderId,
			ClientId:   providerClientId,
			ProbeNonce: attestation.ProbeNonce,
			RttMs:      int(attestation.RttMs),
			ProbeTime:  time.UnixMilli(int64(attestation.TimestampMs)).UTC(),
		})
	}

	acceptedCount := model.AddNetworkExtenderLatencies(clientSession.Ctx, latencies)
	// what verified but was already stored is a replay, not a sample
	rejectedCount += len(latencies) - acceptedCount
	return &ExtenderLatencyReportResult{
		Accepted: acceptedCount,
		Rejected: rejectedCount,
	}, nil
}

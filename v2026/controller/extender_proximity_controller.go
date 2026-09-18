package controller

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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

const (
	// Reports per user per hour. An extender flushes at most every 30 s, so
	// 120 is the honest ceiling; the budget leaves room for a retry storm.
	ExtenderLatencyReportRateLimitAction = "extender_latency_report"
	ExtenderLatencyReportRateLimit       = 240
	ExtenderLatencyReportRateLimitWindow = time.Hour

	// Attestations in one report. The reporter batches 64; anything past this
	// is not a batch.
	ExtenderLatencyReportMaxCount = 256

	// How long a latency row is kept before the sweep removes it.
	ExtenderLatencyRetention = 30 * 24 * time.Hour
)

// The hint: the continent the operator places the caller's address on, upper
// case, the same mapping the geo dns and the record tag use. Empty when the
// operator cannot place the caller, which a client treats as no hint rather
// than as a continent.
type ExtenderHintResult struct {
	ContinentCode string `json:"continent_code"`
}

// ExtenderHint answers `GET /network/extender-hint` (connect/DESIGNNOTES4.md
// §4). Nothing here can fail the request: an address the operator cannot
// read or place is an empty hint.
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
// (connect.ExtenderLatencyAttestation), from which the signing bytes are
// rebuilt and the provider's signature verified.
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

// ExtenderLatencyReport answers `POST /network/extender-latency`
// (connect/DESIGNNOTES4.md §3).
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
	if ExtenderLatencyReportMaxCount < len(args.Attestations) {
		return refuse(fmt.Sprintf(
			"a latency report carries at most %d attestations",
			ExtenderLatencyReportMaxCount,
		))
	}
	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
		ExtenderLatencyReportRateLimitAction,
		ExtenderLatencyReportRateLimit,
		ExtenderLatencyReportRateLimitWindow,
	); err != nil {
		return refuse(err.Error())
	}
	reporterClientId := *clientSession.ByJwt.ClientId

	// the extenders named, looked up once per key; nil is a key no extender
	// activated under, or one the caller does not own
	ownedExtenders := map[string]*model.NetworkExtender{}
	ownedExtender := func(publicKey []byte) *model.NetworkExtender {
		keyHex := hex.EncodeToString(publicKey)
		if extender, ok := ownedExtenders[keyHex]; ok {
			return extender
		}
		extender := model.GetNetworkExtenderByPublicKey(clientSession.Ctx, publicKey)
		if extender != nil && extender.ClientId != reporterClientId {
			extender = nil
		}
		ownedExtenders[keyHex] = extender
		return extender
	}
	// the provider keys, looked up once per provider
	providerKeys := map[server.Id]ed25519.PublicKey{}
	providerKey := func(clientId server.Id) ed25519.PublicKey {
		if publicKey, ok := providerKeys[clientId]; ok {
			return publicKey
		}
		var publicKey ed25519.PublicKey
		if keyBytes, err := model.GetClientPublicKey(clientSession.Ctx, clientId); err == nil && len(keyBytes) == ed25519.PublicKeySize {
			publicKey = ed25519.PublicKey(keyBytes)
		}
		providerKeys[clientId] = publicKey
		return publicKey
	}

	rejected := 0
	latencies := []*model.NetworkExtenderLatency{}
	for _, attestationArgs := range args.Attestations {
		if attestationArgs == nil {
			rejected += 1
			continue
		}
		attestation, err := (&connect.ExtenderLatencyAttestation{
			ClientId:             attestationArgs.ClientId,
			ExtenderPublicKeyHex: attestationArgs.ExtenderPublicKeyHex,
			ProbeNonce:           attestationArgs.ProbeNonce,
			RttMs:                attestationArgs.RttMs,
			TimestampMs:          attestationArgs.TimestampMs,
			Signature:            attestationArgs.Signature,
		}).Proto()
		if err != nil {
			rejected += 1
			continue
		}
		extender := ownedExtender(attestation.ExtenderPublicKey)
		if extender == nil {
			rejected += 1
			continue
		}
		providerClientId, err := server.IdFromBytes(attestation.ProbeClientId)
		if err != nil {
			rejected += 1
			continue
		}
		if providerClientId == extender.ClientId {
			// an extender attesting its own distance to itself says nothing
			rejected += 1
			continue
		}
		publicKey := providerKey(providerClientId)
		if publicKey == nil || !connect.VerifyExtenderProbeAttestation(publicKey, attestation) {
			rejected += 1
			continue
		}
		if attestation.TimestampMs == 0 {
			rejected += 1
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

	accepted := model.AddNetworkExtenderLatencies(clientSession.Ctx, latencies)
	// what verified but was already stored is a replay, not a sample
	rejected += len(latencies) - accepted
	return &ExtenderLatencyReportResult{
		Accepted: accepted,
		Rejected: rejected,
	}, nil
}

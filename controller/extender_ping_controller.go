// The ping report endpoint (connect/GEOMAP.md §2.4, §2.5, §5.7): the claims
// pingers post, verified under the operator's own keys and checked for
// replays across the day partitions, and the settings that fix the report's
// limits and how long a stored ping is kept.
package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The ping report (connect/GEOMAP.md §2.4, §2.5): a pinger -- a provider over
// its client credential, an extender over its activation credential -- posts
// what it measured, and what became of each claim at its target.
//
// The pinger reports because the co-signature makes that safe. A claim that
// carries one has had a second check by the party that saw the round trip,
// and the operator verifies both signatures here, under keys it holds rather
// than keys the report brings: the pinger's under the key store (a provider)
// or the extender table (an extender), and the co-signature under the
// target's stored identity key. A claim without a co-signature has had no
// second check, so it is stored as the pinger's word that the target refused
// or never answered -- never as a measurement, and not on its own as
// evidence against the target (D1, D8). The outcome is therefore recomputed,
// never taken from the report: only a co-signature that verifies makes a
// ping co-signed.
//
// Every claim must name the reporting client as its pinger, so no client
// reports another's pings, and a claim about the pinger's own extender says
// nothing and is refused.

// The action the ping report's rate limit is recorded under.
const PingReportRateLimitAction = "ping_report"

// The limits of the ping report, the clock skews it accepts, and how long
// what it stores is kept and measured (GEOMAP §5.7, D18). The spans
// interlock: the derive phase reads a retention of pings and runs three
// times inside it, and a stored ping is kept until no copy of its claim that
// the report could still accept can arrive (KeepTimeout), which is what both
// the sweep's partition drop and the report's replay lookback read.
type ExtenderPingReportSettings struct {
	// reports per reporting client per window. A pinger's reporter posts
	// when a batch of 64 fills or 30 s after its first ping, so even an
	// extender pinging a thousand peers posts about 120 times an hour; the
	// budget leaves room for a retry storm. It is the client's own, so each
	// extender an account runs has the whole of it
	RateLimit       int
	RateLimitWindow time.Duration
	// pings in one report. The reporter batches 64; anything past this is
	// not a batch, and the report is refused whole
	MaxCount int
	// pings without a verifying co-signature accepted from one report. They
	// are claims only (D1), so a reporter must not be able to fill the table
	// with them; past the cap they count as rejected. A co-signed ping is
	// never capped: it is a measurement the target vouched for
	MaxUncosignedPerPost int
	// how far behind the operator's clock a claim's own timestamp may be. A
	// pinger posts within a flush of measuring, but a reporter that could
	// not reach the operator holds its pings and retries, so the window is
	// the day a ping lives rather than a few minutes
	MaxBackwardClockSkew time.Duration
	// how far ahead of the operator's clock a claim's own timestamp may be.
	// A claim is timestamped when it is measured, so only clock error puts
	// it ahead, and every minute allowed here is a minute a stored ping must
	// outlive
	MaxForwardClockSkew time.Duration
	// how long a ping is a measurement (GEOMAP §5.7, D18): a path measured
	// yesterday says little about today, so the derive phase reads this
	// window, and a derived location lives as long from its derivation
	Retention time.Duration
	// how often the retention sweep runs
	SweepTimeout time.Duration
	// how often the derive phase runs (GEOMAP §5.3, §5.7, D18): three
	// derivations inside every retention, so a fresh ping is solved on
	// before it expires, and a derived location -- swept a retention after
	// the derivation that wrote it -- is renewed twice before it could
	// expire
	DeriveInterval time.Duration
}

// The settings the report, its retention sweep and the derive phase run
// with.
func DefaultExtenderPingReportSettings() *ExtenderPingReportSettings {
	return &ExtenderPingReportSettings{
		RateLimit:            240,
		RateLimitWindow:      time.Hour,
		MaxCount:             256,
		MaxUncosignedPerPost: 64,
		MaxBackwardClockSkew: 24 * time.Hour,
		MaxForwardClockSkew:  5 * time.Minute,
		Retention:            24 * time.Hour,
		SweepTimeout:         time.Hour,
		DeriveInterval:       8 * time.Hour,
	}
}

// How long past its create time a stored ping is kept: the sweep drops a day
// partition only once its upper bound is older than this, and a report is
// checked this far back for a replay. One claim is accepted from
// MaxForwardClockSkew before its own time until MaxBackwardClockSkew after
// it, so two copies of it can arrive that far apart; the span covers that,
// or the retention if that is longer, plus one sweep interval, so that no
// copy the report still accepts can find the row it would replay dropped.
func (self *ExtenderPingReportSettings) KeepTimeout() time.Duration {
	return max(self.Retention, self.MaxBackwardClockSkew+self.MaxForwardClockSkew) + self.SweepTimeout
}

// One attested ping as the report carries it: the fields of the pinger's
// signed claim, from which the signing bytes are rebuilt and the pinger's
// signature verified, and the pinger's account of the target's verdict. The
// json is the wire, field for field connect's ExtenderPingReport.
type ExtenderPingArgs struct {
	// provider or extender
	PingerKind string `json:"pinger_kind"`
	// the provider's client id, empty for an extender pinger
	PingerClientId string `json:"pinger_client_id"`
	// the pinging extender's identity key, hex, empty for a provider pinger
	PingerExtenderPublicKeyHex string `json:"pinger_extender_public_key_hex"`
	TargetExtenderPublicKeyHex string `json:"target_extender_public_key_hex"`
	// base64
	ProbeNonce  string `json:"probe_nonce"`
	RttMs       uint32 `json:"rtt_ms"`
	TimestampMs uint64 `json:"timestamp_ms"`
	// base64 of the pinger's ed25519 signature
	Signature string `json:"signature"`
	// cosigned, rejected or unknown, as the pinger recorded it. The operator
	// recomputes it.
	Outcome string `json:"outcome"`
	// the target's refusal reason, 0 without a verdict
	Reason uint32 `json:"reason"`
	// base64 of the target's co-signature, empty unless cosigned
	Cosignature string `json:"cosignature"`
	// the NLayer relays the probe crossed to reach the chain end the claim
	// names, 0 for a direct ping (GEOMAP §2.9). It is outside the signature,
	// so it is stored as reported. It decides only that a relayed ping is
	// never a solver term, and a pinger that hides a relay can only inflate
	// its round trip, which it could already do by delaying its claim.
	HopCount uint32 `json:"hop_count"`
}

// The pinger's signed claim. A field that does not decode, a pinger kind the
// identity fields contradict, or a field of the wrong size is an error here
// rather than a signature that does not verify.
func (self *ExtenderPingArgs) proto() (*protocol.ExtenderProbeAttestation, error) {
	attestation, err := (&connect.ExtenderPingReport{
		PingerKind:                 connect.ExtenderPingerKind(self.PingerKind),
		PingerClientId:             self.PingerClientId,
		PingerExtenderPublicKeyHex: self.PingerExtenderPublicKeyHex,
		TargetExtenderPublicKeyHex: self.TargetExtenderPublicKeyHex,
		ProbeNonce:                 self.ProbeNonce,
		RttMs:                      self.RttMs,
		TimestampMs:                self.TimestampMs,
		Signature:                  self.Signature,
	}).Proto()
	if err != nil {
		return nil, err
	}
	// the fixed widths the signature covers, checked before anything is
	// looked up for the claim
	if _, err := connect.ExtenderProbeAttestationSigningBytes(attestation); err != nil {
		return nil, err
	}
	return attestation, nil
}

// A report as the pinger's reporter posts it: a batch of attested pings.
type ExtenderPingReportArgs struct {
	Pings []*ExtenderPingArgs `json:"pings"`
}

// A refusal of the whole report is a normal answer with `Error`, as an
// activation refusal is. Inside an accepted report, a ping that does not
// decode, does not name the caller as its pinger, names an unknown target or
// the pinger's own, does not verify, is out of the time window, is past the
// uncosigned cap, or was already stored is counted in `Rejected` and not
// named: the count is what tells a broken reporter from a quiet one.
type ExtenderPingReportResult struct {
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Error    string `json:"error,omitempty"`
}

// Answers `POST /network/ping-report` (GEOMAP §2.5) at the operator's clock.
//
// Every ping is checked on its own: it must decode; name the calling client
// as its pinger -- a provider by its client id, an extender by an identity
// key the calling client activated; name a target extender that exists,
// active or not, and that is not the caller's own; verify under the pinger's
// stored key; and carry a timestamp within a day of now. Its verdict is then
// recomputed under the target's stored key. What passes is stored; a copy of
// what is already stored, on any day, is a replay and not a second sample.
func ExtenderPingReport(
	args *ExtenderPingReportArgs,
	clientSession *session.ClientSession,
) (*ExtenderPingReportResult, error) {
	return extenderPingReport(args, clientSession, server.NowUtc(), DefaultExtenderPingReportSettings())
}

// The report as the operator's clock reads `now`, under `settings`; a test
// moves the clock to post a claim again later.
func extenderPingReport(
	args *ExtenderPingReportArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *ExtenderPingReportSettings,
) (*ExtenderPingReportResult, error) {
	refuse := func(message string) (*ExtenderPingReportResult, error) {
		return &ExtenderPingReportResult{
			Error: message,
		}, nil
	}

	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this is the structural guard for a
		// caller that reaches the controller another way
		return refuse("a ping report requires a client")
	}
	if args == nil || len(args.Pings) == 0 {
		return &ExtenderPingReportResult{}, nil
	}
	if settings.MaxCount < len(args.Pings) {
		return refuse(fmt.Sprintf(
			"a ping report carries at most %d pings",
			settings.MaxCount,
		))
	}
	reporterClientId := *clientSession.ByJwt.ClientId
	// the budget is the reporting client's own, not its account's: an
	// operator runs its whole fleet of extenders under one account
	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		reporterClientId,
		PingReportRateLimitAction,
		settings.RateLimit,
		settings.RateLimitWindow,
	); err != nil {
		return refuse(err.Error())
	}

	pings, rejected, err := verifyPingReport(args, reporterClientId, now, &storedPingReportKeys{
		ctx:              clientSession.Ctx,
		reporterClientId: reporterClientId,
		keyHexExtenders:  map[string]*model.NetworkExtender{},
	}, settings)
	if err != nil {
		return nil, err
	}

	// The replay lookup reaches back KeepTimeout, the span over which two
	// copies of one claim can arrive, which the sweep keeps every row for.
	// The unique key carries the create time, the partition column, so it
	// cannot see a copy stored at another time; the lookup does (GEOMAP §5.7).
	accepted := model.AddReportedNetworkPings(
		clientSession.Ctx,
		pings,
		now.Add(-settings.KeepTimeout()),
	)
	// what verified but was already stored, or repeated in the report, is a
	// replay, not a sample
	rejected += len(pings) - accepted
	return &ExtenderPingReportResult{
		Accepted: accepted,
		Rejected: rejected,
	}, nil
}

// The keys a report's claims are verified under. The endpoint's read the key
// store and the extender table; the verification benchmark's are held in
// memory (GEOMAP §5.8), so both run the same checks over the same claims.
type pingReportKeys interface {
	// the reporting provider's registered client key, nil when it has none,
	// and an error when the key store cannot be read
	providerKey() (ed25519.PublicKey, error)
	// the extender that activated under `publicKey`, active or not, nil for
	// a key no extender activated under
	extenderByKey(publicKey []byte) *model.NetworkExtender
}

// The endpoint's keys, each read from the database at most once per report. A
// provider reports only its own pings, so its client key is the only one a
// report can need.
type storedPingReportKeys struct {
	ctx              context.Context
	reporterClientId server.Id
	// the extenders named, by hex identity key; nil is a key no extender
	// activated under
	keyHexExtenders   map[string]*model.NetworkExtender
	providerKeyRead   bool
	providerPublicKey ed25519.PublicKey
}

// Implements pingReportKeys. A key store that cannot be read fails the
// report, so the reporter retries rather than losing valid pings as rejected.
func (self *storedPingReportKeys) providerKey() (ed25519.PublicKey, error) {
	if !self.providerKeyRead {
		keyBytes, err := model.GetClientPublicKey(self.ctx, self.reporterClientId)
		if err != nil {
			return nil, err
		}
		if len(keyBytes) == ed25519.PublicKeySize {
			self.providerPublicKey = ed25519.PublicKey(keyBytes)
		}
		self.providerKeyRead = true
	}
	return self.providerPublicKey, nil
}

// Implements pingReportKeys.
func (self *storedPingReportKeys) extenderByKey(publicKey []byte) *model.NetworkExtender {
	keyHex := hex.EncodeToString(publicKey)
	if extender, ok := self.keyHexExtenders[keyHex]; ok {
		return extender
	}
	extender := model.GetNetworkExtenderByPublicKey(self.ctx, publicKey)
	self.keyHexExtenders[keyHex] = extender
	return extender
}

// Checks every claim of a report under `keys` as the operator's clock reads
// `now` and `settings` bound it (GEOMAP §2.4), and returns the pings to store
// and how many claims were rejected. No database is behind it but what `keys` reads, so the endpoint
// and the verification benchmark (§5.8) run exactly this. A claim that passes
// the structural checks costs two ed25519 verifications, the pinger's
// signature and the target's co-signature.
func verifyPingReport(
	args *ExtenderPingReportArgs,
	reporterClientId server.Id,
	now time.Time,
	keys pingReportKeys,
	settings *ExtenderPingReportSettings,
) (pings []*model.NetworkPing, rejected int, err error) {
	uncosigned := 0
	pings = []*model.NetworkPing{}
	for _, pingArgs := range args.Pings {
		if pingArgs == nil {
			rejected += 1
			continue
		}
		attestation, err := pingArgs.proto()
		if err != nil {
			rejected += 1
			continue
		}
		outcome, reason, cosignature, ok := pingArgs.verdict()
		if !ok {
			rejected += 1
			continue
		}
		if !pingTimestampInWindow(
			attestation.TimestampMs,
			now,
			settings.MaxBackwardClockSkew,
			settings.MaxForwardClockSkew,
		) {
			rejected += 1
			continue
		}
		if math.MaxInt32 < attestation.RttMs {
			// no round trip is this long, and the column is an int
			rejected += 1
			continue
		}
		if math.MaxInt16 < pingArgs.HopCount {
			// no chain is this deep, and the column is a smallint
			rejected += 1
			continue
		}

		// the pinger must be the caller, and its key is the one the operator
		// holds for it
		var pingerKind int
		var pingerId server.Id
		var pingerPublicKey ed25519.PublicKey
		var pingerExtender *model.NetworkExtender
		switch connect.ExtenderProbeAttestationPingerKind(attestation) {
		case connect.ExtenderPingerKindProvider:
			pingerClientId, err := server.IdFromBytes(attestation.ProbeClientId)
			if err != nil || pingerClientId != reporterClientId {
				rejected += 1
				continue
			}
			publicKey, err := keys.providerKey()
			if err != nil {
				return nil, 0, err
			}
			if publicKey == nil {
				rejected += 1
				continue
			}
			pingerKind = model.NetworkPingPingerKindProvider
			pingerId = pingerClientId
			pingerPublicKey = publicKey
		case connect.ExtenderPingerKindExtender:
			pingerExtender = keys.extenderByKey(attestation.PingerExtenderPublicKey)
			if pingerExtender == nil || pingerExtender.ClientId != reporterClientId {
				// an extender reports only its own pings
				rejected += 1
				continue
			}
			pingerKind = model.NetworkPingPingerKindExtender
			pingerId = pingerExtender.ExtenderId
			pingerPublicKey = ed25519.PublicKey(pingerExtender.PublicKey)
		default:
			rejected += 1
			continue
		}

		target := keys.extenderByKey(attestation.ExtenderPublicKey)
		if target == nil {
			rejected += 1
			continue
		}
		if pingerExtender != nil && target.ExtenderId == pingerExtender.ExtenderId {
			// an extender measuring itself says nothing
			rejected += 1
			continue
		}
		if target.ClientId == reporterClientId {
			// nor does a client measuring an extender it activated itself:
			// for a provider that is its own extender, and for an extender a
			// sibling identity of the same client
			rejected += 1
			continue
		}

		if !connect.VerifyExtenderProbeAttestation(pingerPublicKey, attestation) {
			rejected += 1
			continue
		}

		ping := &model.NetworkPing{
			PingerKind:       pingerKind,
			PingerId:         pingerId,
			TargetExtenderId: target.ExtenderId,
			ProbeNonce:       attestation.ProbeNonce,
			RttMs:            int(attestation.RttMs),
			ProbeTime:        time.UnixMilli(int64(attestation.TimestampMs)).UTC(),
			PingerSignature:  attestation.Signature,
			HopCount:         int(pingArgs.HopCount),
			CreateTime:       now,
		}
		ping.Cosign, ping.CosignReason, ping.Cosignature = recomputePingVerdict(
			ed25519.PublicKey(target.PublicKey),
			attestation,
			outcome,
			reason,
			cosignature,
		)
		if ping.Cosign != model.NetworkPingCosignCosigned {
			if settings.MaxUncosignedPerPost <= uncosigned {
				rejected += 1
				continue
			}
			uncosigned += 1
		}
		pings = append(pings, ping)
	}
	return pings, rejected, nil
}

// The pinger's account of the verdict, decoded: the outcome it recorded, the
// target's reason, and the co-signature it carries. Not ok for an outcome
// that is not one of the three, a co-signature that is not base64, or a
// reason no stored verdict can hold.
func (self *ExtenderPingArgs) verdict() (
	outcome connect.ExtenderPingOutcome,
	reason uint32,
	cosignature []byte,
	ok bool,
) {
	outcome = connect.ExtenderPingOutcome(self.Outcome)
	switch outcome {
	case connect.ExtenderPingCosigned, connect.ExtenderPingRejected, connect.ExtenderPingUnknown:
	default:
		return "", 0, nil, false
	}
	if math.MaxInt16 < self.Reason {
		return "", 0, nil, false
	}
	if self.Cosignature != "" {
		var err error
		cosignature, err = base64.StdEncoding.DecodeString(self.Cosignature)
		if err != nil {
			return "", 0, nil, false
		}
	}
	return outcome, self.Reason, cosignature, true
}

// The operator's verdict on one verified claim, from the target's stored key
// and what the pinger says it received: the stored cosign, its reason, and
// the co-signature to keep.
//
// Only a co-signature that verifies under the target's key, over exactly this
// claim, makes a ping co-signed -- whatever the pinger says, since a pinger
// that holds one cannot be refused, and one that does not cannot be accepted.
// Without one, a pinger that says the target refused is stored as refused
// with the reason it says the target gave. A pinger that says co-signed with
// a co-signature that does not verify is also stored as refused, with the bad
// signature reason: an acceptance nobody can check is not one, and neither
// side escapes the refusal rate by it. A pinger that got no verdict is stored
// as unknown, with no reason, and is not read as a refusal at all. The
// co-signature is kept only when it verifies; anything else is no evidence of
// anything.
func recomputePingVerdict(
	targetPublicKey ed25519.PublicKey,
	attestation *protocol.ExtenderProbeAttestation,
	outcome connect.ExtenderPingOutcome,
	reason uint32,
	cosignature []byte,
) (cosign int, cosignReason int, storedCosignature []byte) {
	cosigned := 0 < len(cosignature) && connect.VerifyExtenderProbeVerdict(
		targetPublicKey,
		attestation,
		&protocol.ExtenderProbeVerdict{
			Accepted:    true,
			Cosignature: cosignature,
		},
	)
	switch {
	case cosigned:
		return model.NetworkPingCosignCosigned, int(connect.ExtenderProbeVerdictReasonOk), cosignature
	case outcome == connect.ExtenderPingRejected:
		return model.NetworkPingCosignRejected, int(reason), nil
	case outcome == connect.ExtenderPingCosigned:
		return model.NetworkPingCosignRejected, int(connect.ExtenderProbeVerdictReasonBadSignature), nil
	default:
		return model.NetworkPingCosignUnknown, int(connect.ExtenderProbeVerdictReasonOk), nil
	}
}

// Whether a claim's own timestamp is set and inside a window from
// `maxBackwardClockSkew` behind the operator's clock to `maxForwardClockSkew`
// ahead of it. The bound also keeps the stored probe time inside what the
// column can hold, whatever a pinger signed.
func pingTimestampInWindow(
	timestampMs uint64,
	now time.Time,
	maxBackwardClockSkew time.Duration,
	maxForwardClockSkew time.Duration,
) bool {
	if timestampMs == 0 || math.MaxInt64 < timestampMs {
		return false
	}
	probeTime := time.UnixMilli(int64(timestampMs))
	return !probeTime.Before(now.Add(-maxBackwardClockSkew)) &&
		!now.Add(maxForwardClockSkew).Before(probeTime)
}

package model

// The SN Top board counts a provider's prefix toward its network's units only
// while the provider is admitted (owner decisions 2026-11-20, "build Top 200
// rule (a)" and "add all three"): the score export decided it a native Speed
// or Quality member (decideProviderEgress, ClientScore.PassesMinimums). Two
// excuses: in a country whose URL probes make no UDP leg (qualityprobe.yml
// country_settings udp_probes: false), Online membership counts instead; in
// an autonomous system where UDP fails at the network level (the fold decides
// that, controller/sn_top_admission.go), admission with the UDP failure
// forgiven counts instead. Any other provider shows no prefix, as a provider
// that declared a proxy egress shows none, and its network is still
// providing.
//
// The export runs in the taskworker every few minutes and the board folds once
// a tempo, so the export publishes its decisions here, one key, and the fold
// reads the newest. Like capture the flag's Online set (ctf_presence_model.go),
// the set is the URL bar's decision: a membership only the country floor gave
// is none.
//
// One bad probe must not take a provider's unit (owner decision 2026-11-20):
// each decision is smoothed over a rolling window with hysteresis. Each
// export folds the current decision, 1 or 0, into an exponentially weighted
// mean with a half-life of SnTopAdmissionHalfLife (about a six-hour window),
// and a decision stops counting only when its mean falls below
// SnTopAdmissionWithholdBelow and counts again only once it reaches
// SnTopAdmissionRestoreAt: about three hours of a sustained change either way.
// A provider new to the set starts at its current decision.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The Redis key the score export writes its admission set to. One key, so its
// hash tag spreads nothing.
const snTopAdmissionKey = "{sn_top_admission}providers"

// How long a published set is kept. Exports run a few minutes apart.
const snTopAdmissionTtl = 6 * time.Hour

// The oldest export the board takes its admission from. Older, the export has
// stalled, and its decisions are not the providers' state at the fold.
const SnTopAdmissionMaxAge = 20 * time.Minute

// The rolling window: the half-life of each decision's mean, and the
// hysteresis band.
const (
	SnTopAdmissionHalfLife      = 2 * time.Hour
	SnTopAdmissionWithholdBelow = 0.35
	SnTopAdmissionRestoreAt     = 0.65
)

// Past this gap since the previous export a provider's history is stale, and
// it starts again at its current decision.
const snTopAdmissionHistoryMaxGap = 24 * time.Hour

// Why a provider is not admitted, from its strict decision
// (decideProviderEgress) and its capability classification. The fold adds
// SnTopUnitReasonExitDiffers for a marked provider.
const (
	SnTopUnitReasonNone = ""
	// the URL checks fail over HTTPS: below the 4/5 ratio, no success, or a
	// failing ratio
	SnTopUnitReasonUrl = "url"
	// only the UDP (HTTP/3) legs fail: with them forgiven it is admitted
	SnTopUnitReasonUdp = "udp"
	// the exit or a connection is a risky address (ARIN risk: a known VPN,
	// proxy or Tor address), which no bucket admits
	SnTopUnitReasonRisk = "risk"
	// too few URL runs yet, or none
	SnTopUnitReasonUnprobed = "unprobed"
	// another common gate: TLS findings, zero carry or reliability
	SnTopUnitReasonOther = "other"
	// the probed exit differs from every network it connects from, or a
	// transport declared a proxy egress (verify_proxied_egress_model.go)
	SnTopUnitReasonExitDiffers = "exit_differs"
)

var snTopUnitReasonCodes = []string{
	SnTopUnitReasonNone,
	SnTopUnitReasonUrl,
	SnTopUnitReasonUdp,
	SnTopUnitReasonRisk,
	SnTopUnitReasonUnprobed,
	SnTopUnitReasonOther,
	SnTopUnitReasonExitDiffers,
}

// One smoothed decision: its mean over the rolling window and whether it
// counts, with hysteresis.
type SnTopAdmissionTrack struct {
	Score  float64
	Counts bool
}

// A track that starts at the decision.
func newSnTopAdmissionTrack(decided bool) SnTopAdmissionTrack {
	if decided {
		return SnTopAdmissionTrack{Score: 1, Counts: true}
	}
	return SnTopAdmissionTrack{Score: 0, Counts: false}
}

// Folds a decision made elapsed after the previous one into the track.
func (self SnTopAdmissionTrack) fold(decided bool, elapsed time.Duration) SnTopAdmissionTrack {
	if elapsed < 0 {
		elapsed = 0
	}
	weight := 1 - math.Exp2(-elapsed.Seconds()/SnTopAdmissionHalfLife.Seconds())
	value := 0.0
	if decided {
		value = 1
	}
	next := SnTopAdmissionTrack{Score: self.Score + weight*(value-self.Score), Counts: self.Counts}
	switch {
	case self.Counts && next.Score < SnTopAdmissionWithholdBelow:
		next.Counts = false
	case !self.Counts && SnTopAdmissionRestoreAt <= next.Score:
		next.Counts = true
	}
	return next
}

// One provider's decisions at the export, and their smoothed tracks.
type SnTopAdmissionProvider struct {
	ClientId server.Id
	// the country the provider is published under, normalized
	// (CtfNormalizeCountryCode)
	CountryCode string
	// the export's current decisions: a native Speed or Quality member; an
	// Online member by the URL bar; admitted once its UDP failures are
	// forgiven (its capability classification, FP2-PROTOCOL.md §3.4); its
	// UDP legs fail (the UDP downgrade); and it has URL evidence at all
	Admitted      bool
	Online        bool
	TcpAdmitted   bool
	UdpDowngraded bool
	Probed        bool
	// why it is not admitted (SnTopUnitReason*), none when it is
	Reason string
	// the smoothed decisions the board counts by
	AdmittedTrack    SnTopAdmissionTrack
	OnlineTrack      SnTopAdmissionTrack
	TcpAdmittedTrack SnTopAdmissionTrack
}

// What the fold decided about the provider's place: its country's URL probes
// make no UDP leg, or its autonomous system fails UDP at the network level.
type SnTopAdmissionExcuses struct {
	UdpProbesDisabled bool
	UdpNetworkLevel   bool
}

// Whether the provider's prefix counts toward its network's units, by its
// smoothed decisions: admitted; or Online where the country's probes make no
// UDP leg; or admitted with UDP forgiven where UDP fails at the network
// level. A provider the export did not decide (nil) counts nothing.
func (self *SnTopAdmissionProvider) CountsUnits(excuses SnTopAdmissionExcuses) bool {
	if self == nil {
		return false
	}
	if self.AdmittedTrack.Counts {
		return true
	}
	if excuses.UdpProbesDisabled && self.OnlineTrack.Counts {
		return true
	}
	return excuses.UdpNetworkLevel && self.TcpAdmittedTrack.Counts
}

// The export's decisions for every provider it scored, by client id.
type SnTopAdmission struct {
	MeasuredAt time.Time
	Providers  map[server.Id]*SnTopAdmissionProvider
}

// Whether the set is recent enough to fold from at now.
func (self *SnTopAdmission) Fresh(now time.Time) bool {
	return self != nil && !self.MeasuredAt.Before(now.Add(-SnTopAdmissionMaxAge))
}

// The encoded set: a version byte, the export's unix milliseconds (8), the
// count (4), then per provider, in client id order, its client id (16),
// country (2), decision flags (1), track flags (1), reason (1) and the three
// track scores (1 each, the score times 255, rounded).
const (
	snTopAdmissionVersion       = 2
	snTopAdmissionHeaderBytes   = 13
	snTopAdmissionProviderBytes = 24

	snTopAdmissionFlagAdmitted      = 1
	snTopAdmissionFlagOnline        = 2
	snTopAdmissionFlagTcpAdmitted   = 4
	snTopAdmissionFlagUdpDowngraded = 8
	snTopAdmissionFlagProbed        = 16
	snTopAdmissionDecisionFlags     = 31

	snTopAdmissionTrackAdmitted    = 1
	snTopAdmissionTrackOnline      = 2
	snTopAdmissionTrackTcpAdmitted = 4
	snTopAdmissionTrackFlags       = 7
)

func encodeSnTopAdmissionScore(score float64) byte {
	return byte(math.Round(min(1, max(0, score)) * 255))
}

func decodeSnTopAdmissionScore(b byte) float64 {
	return float64(b) / 255
}

func encodeSnTopAdmission(admission *SnTopAdmission) []byte {
	providers := make([]*SnTopAdmissionProvider, 0, len(admission.Providers))
	for _, provider := range admission.Providers {
		providers = append(providers, provider)
	}
	slices.SortFunc(providers, func(a, b *SnTopAdmissionProvider) int { return a.ClientId.Cmp(b.ClientId) })
	b := make([]byte, 0, snTopAdmissionHeaderBytes+snTopAdmissionProviderBytes*len(providers))
	b = append(b, snTopAdmissionVersion)
	b = binary.BigEndian.AppendUint64(b, uint64(admission.MeasuredAt.UnixMilli()))
	b = binary.BigEndian.AppendUint32(b, uint32(len(providers)))
	for _, provider := range providers {
		b = append(b, provider.ClientId.Bytes()...)
		country := CtfNormalizeCountryCode(provider.CountryCode)
		b = append(b, country[0], country[1])
		var decisions byte
		for _, flag := range []struct {
			set bool
			bit byte
		}{
			{provider.Admitted, snTopAdmissionFlagAdmitted},
			{provider.Online, snTopAdmissionFlagOnline},
			{provider.TcpAdmitted, snTopAdmissionFlagTcpAdmitted},
			{provider.UdpDowngraded, snTopAdmissionFlagUdpDowngraded},
			{provider.Probed, snTopAdmissionFlagProbed},
		} {
			if flag.set {
				decisions |= flag.bit
			}
		}
		var tracks byte
		if provider.AdmittedTrack.Counts {
			tracks |= snTopAdmissionTrackAdmitted
		}
		if provider.OnlineTrack.Counts {
			tracks |= snTopAdmissionTrackOnline
		}
		if provider.TcpAdmittedTrack.Counts {
			tracks |= snTopAdmissionTrackTcpAdmitted
		}
		reason := slices.Index(snTopUnitReasonCodes, provider.Reason)
		if reason < 0 {
			reason = slices.Index(snTopUnitReasonCodes, SnTopUnitReasonOther)
		}
		b = append(
			b,
			decisions,
			tracks,
			byte(reason),
			encodeSnTopAdmissionScore(provider.AdmittedTrack.Score),
			encodeSnTopAdmissionScore(provider.OnlineTrack.Score),
			encodeSnTopAdmissionScore(provider.TcpAdmittedTrack.Score),
		)
	}
	return b
}

func decodeSnTopAdmission(b []byte) (*SnTopAdmission, error) {
	if len(b) < snTopAdmissionHeaderBytes {
		return nil, fmt.Errorf("sn top admission set holds %d bytes, less than its header", len(b))
	}
	if b[0] != snTopAdmissionVersion {
		return nil, fmt.Errorf("sn top admission set has version %d, not %d", b[0], snTopAdmissionVersion)
	}
	measuredAt := time.UnixMilli(int64(binary.BigEndian.Uint64(b[1:9]))).UTC()
	count := int(binary.BigEndian.Uint32(b[9:13]))
	if len(b) != snTopAdmissionHeaderBytes+count*snTopAdmissionProviderBytes {
		return nil, fmt.Errorf("sn top admission set holds %d bytes for %d providers", len(b), count)
	}
	admission := &SnTopAdmission{
		MeasuredAt: measuredAt,
		Providers:  make(map[server.Id]*SnTopAdmissionProvider, count),
	}
	for i := range count {
		row := b[snTopAdmissionHeaderBytes+i*snTopAdmissionProviderBytes:]
		clientId, err := server.IdFromBytes(row[0:16])
		if err != nil {
			return nil, err
		}
		countryCode := string(row[16:18])
		if CtfNormalizeCountryCode(countryCode) != countryCode {
			return nil, fmt.Errorf("sn top admission set: provider %d has country %q", i, countryCode)
		}
		decisions, tracks, reason := row[18], row[19], int(row[20])
		if decisions&^snTopAdmissionDecisionFlags != 0 || tracks&^snTopAdmissionTrackFlags != 0 || len(snTopUnitReasonCodes) <= reason {
			return nil, fmt.Errorf("sn top admission set: provider %d has flags %x %x and reason %d", i, decisions, tracks, reason)
		}
		admission.Providers[clientId] = &SnTopAdmissionProvider{
			ClientId:         clientId,
			CountryCode:      countryCode,
			Admitted:         decisions&snTopAdmissionFlagAdmitted != 0,
			Online:           decisions&snTopAdmissionFlagOnline != 0,
			TcpAdmitted:      decisions&snTopAdmissionFlagTcpAdmitted != 0,
			UdpDowngraded:    decisions&snTopAdmissionFlagUdpDowngraded != 0,
			Probed:           decisions&snTopAdmissionFlagProbed != 0,
			Reason:           snTopUnitReasonCodes[reason],
			AdmittedTrack:    SnTopAdmissionTrack{Score: decodeSnTopAdmissionScore(row[21]), Counts: tracks&snTopAdmissionTrackAdmitted != 0},
			OnlineTrack:      SnTopAdmissionTrack{Score: decodeSnTopAdmissionScore(row[22]), Counts: tracks&snTopAdmissionTrackOnline != 0},
			TcpAdmittedTrack: SnTopAdmissionTrack{Score: decodeSnTopAdmissionScore(row[23]), Counts: tracks&snTopAdmissionTrackTcpAdmitted != 0},
		}
	}
	return admission, nil
}

// Why a provider the strict decision did not admit is out: its decision's
// reason, with a failure the UDP legs alone explain told apart.
func snTopAdmissionReason(decision *providerEgressDecision, admitted bool, tcpAdmitted bool, udpDowngraded bool, probed bool) string {
	if admitted {
		return SnTopUnitReasonNone
	}
	if decision == nil || !probed {
		return SnTopUnitReasonUnprobed
	}
	switch decision.reason {
	case ProviderExcludedArinRisk:
		return SnTopUnitReasonRisk
	case ProviderExcludedTls, ProviderExcludedZeroCarry, ProviderExcludedReliability, ProviderExcludedBlackhole:
		return SnTopUnitReasonOther
	case ProviderExcludedUnprobed:
		return SnTopUnitReasonUnprobed
	}
	if udpDowngraded && tcpAdmitted {
		return SnTopUnitReasonUdp
	}
	return SnTopUnitReasonUrl
}

// The export's admission set from its decided scores: each provider once, from
// any of the location maps it sits in, since every map decides it the same way
// (rankClientScore). Admitted is native Speed or Quality membership on the
// strict decision (the top-level score); TcpAdmitted is the same of the
// classified copy when only UDP was downgraded; Online is the URL bar's, so a
// membership only the country floor gave is none. The country is the one the
// provider is published under. Each decision folds into the provider's track
// from the previous set, nil without one.
func newSnTopAdmission(
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
	clientIdEgressDecisions map[server.Id]providerEgressDecision,
	previous *SnTopAdmission,
) *SnTopAdmission {
	admission := &SnTopAdmission{
		MeasuredAt: measuredAt.UTC(),
		Providers:  map[server.Id]*SnTopAdmissionProvider{},
	}
	var elapsed time.Duration
	history := previous != nil
	if history {
		elapsed = admission.MeasuredAt.Sub(previous.MeasuredAt)
		history = 0 <= elapsed && elapsed <= snTopAdmissionHistoryMaxGap
	}
	for _, clientScores := range locationClientScores {
		for clientId, clientScore := range clientScores {
			if _, ok := admission.Providers[clientId]; ok {
				continue
			}
			countryCode := CtfCountryUnknown
			probed := false
			if egress := clientIdEgresses[clientId]; egress != nil {
				countryCode = CtfNormalizeCountryCode(normalizeCountryCode(egress.publishedCountryCode))
				probed = egress.egressQuality != nil
			}
			admitted := clientScore.PassesMinimums[RankModeQuality] || clientScore.PassesMinimums[RankModeSpeed]
			udpDowngraded := clientScore.CapabilityDowngrades&ClientScoreCapabilityDowngradeUdp != 0
			tcpAdmitted := admitted
			if classified := clientScore.capabilityCopy; !admitted && udpDowngraded && classified != nil {
				tcpAdmitted = classified.PassesMinimums[RankModeQuality] || classified.PassesMinimums[RankModeSpeed]
			}
			online := clientScore.Online && !clientScore.countryFloor
			var decision *providerEgressDecision
			if d, ok := clientIdEgressDecisions[clientId]; ok {
				decision = &d
			}
			provider := &SnTopAdmissionProvider{
				ClientId:      clientId,
				CountryCode:   countryCode,
				Admitted:      admitted,
				Online:        online,
				TcpAdmitted:   tcpAdmitted,
				UdpDowngraded: udpDowngraded,
				Probed:        probed,
				Reason:        snTopAdmissionReason(decision, admitted, tcpAdmitted, udpDowngraded, probed),
			}
			if prior := previous.provider(clientId); history && prior != nil {
				provider.AdmittedTrack = prior.AdmittedTrack.fold(admitted, elapsed)
				provider.OnlineTrack = prior.OnlineTrack.fold(online, elapsed)
				provider.TcpAdmittedTrack = prior.TcpAdmittedTrack.fold(tcpAdmitted, elapsed)
			} else {
				provider.AdmittedTrack = newSnTopAdmissionTrack(admitted)
				provider.OnlineTrack = newSnTopAdmissionTrack(online)
				provider.TcpAdmittedTrack = newSnTopAdmissionTrack(tcpAdmitted)
			}
			admission.Providers[clientId] = provider
		}
	}
	return admission
}

// The provider's entry, nil when the set or the entry is missing.
func (self *SnTopAdmission) provider(clientId server.Id) *SnTopAdmissionProvider {
	if self == nil {
		return nil
	}
	return self.Providers[clientId]
}

// publishSnTopAdmission writes the export's admission set for the board, its
// tracks folded from the previous set. It runs inside the export and never
// fails it: an error is logged, and the board fold finds a stale set and skips
// the tempo rather than count without it.
func publishSnTopAdmission(
	ctx context.Context,
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
	clientIdEgressDecisions map[server.Id]providerEgressDecision,
) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[sntop]publish the export's admission set failed: %v\n", r)
		}
	}()
	previous, err := ReadSnTopAdmission(ctx)
	if err != nil {
		glog.Errorf("[sntop]the previous admission set is unreadable, the tracks start again: %s\n", err)
		previous = nil
	}
	admission := newSnTopAdmission(measuredAt, locationClientScores, clientIdEgresses, clientIdEgressDecisions, previous)
	value := encodeSnTopAdmission(admission)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snTopAdmissionKey, value, snTopAdmissionTtl).Err())
	})
	admitted, counting := 0, 0
	for _, provider := range admission.Providers {
		if provider.Admitted {
			admitted += 1
		}
		if provider.AdmittedTrack.Counts {
			counting += 1
		}
	}
	glog.Infof("[sntop]published the admission set of the export at %s: %d providers, %d admitted, %d counting\n", admission.MeasuredAt.Format(time.RFC3339), len(admission.Providers), admitted, counting)
}

// ReadSnTopAdmission returns the newest export's admission set, or nil when
// none is kept.
func ReadSnTopAdmission(ctx context.Context) (admission *SnTopAdmission, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snTopAdmissionKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		admission, returnErr = decodeSnTopAdmission(value)
	})
	return
}

// Testing_SetSnTopAdmission writes an admission set as the export would.
func Testing_SetSnTopAdmission(ctx context.Context, admission *SnTopAdmission) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snTopAdmissionKey, encodeSnTopAdmission(admission), snTopAdmissionTtl).Err())
	})
}

// Testing_SnTopAdmissionProvider is an entry whose current decisions and
// tracks agree, as a provider steady in its state is.
func Testing_SnTopAdmissionProvider(clientId server.Id, countryCode string, admitted bool, online bool, tcpAdmitted bool) *SnTopAdmissionProvider {
	reason := SnTopUnitReasonNone
	switch {
	case !admitted && tcpAdmitted:
		reason = SnTopUnitReasonUdp
	case !admitted:
		reason = SnTopUnitReasonUrl
	}
	return &SnTopAdmissionProvider{
		ClientId:         clientId,
		CountryCode:      countryCode,
		Admitted:         admitted,
		Online:           online,
		TcpAdmitted:      tcpAdmitted,
		UdpDowngraded:    !admitted && tcpAdmitted,
		Probed:           true,
		Reason:           reason,
		AdmittedTrack:    newSnTopAdmissionTrack(admitted),
		OnlineTrack:      newSnTopAdmissionTrack(online),
		TcpAdmittedTrack: newSnTopAdmissionTrack(tcpAdmitted),
	}
}

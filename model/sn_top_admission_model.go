package model

// The SN Top board counts a provider's prefix toward its network's units only
// while the provider is admitted (owner decision 2026-11-20, "build Top 200
// rule (a)"): the score export decided it a native Speed or Quality member
// (decideProviderEgress, ClientScore.PassesMinimums). In a country whose URL
// probes make no UDP leg (qualityprobe.yml country_settings udp_probes: false),
// Online membership counts instead. Any other provider shows no prefix, as a
// provider that declared a proxy egress shows none, and its network is still
// providing.
//
// The export runs in the taskworker every few minutes and the board folds once
// a tempo, so the export publishes its decisions here, one key, and the fold
// reads the newest. Like capture the flag's Online set (ctf_presence_model.go),
// the set is the URL bar's decision: a membership only the country floor gave
// is no admission. The UDP setting is the fold's to read, from the same
// catalog the prober is served (controller.LoadProviderEgressSites).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
const snTopAdmissionTtl = time.Hour

// The oldest export the board takes its admission from. Older, the export has
// stalled, and its decisions are not the providers' state at the fold.
const SnTopAdmissionMaxAge = 20 * time.Minute

// One provider's decisions at the export.
type SnTopAdmissionProvider struct {
	ClientId server.Id
	// the country the provider is published under, normalized
	// (CtfNormalizeCountryCode)
	CountryCode string
	// a native Speed or Quality member
	Admitted bool
	// an Online member by the URL bar, not by the country floor
	Online bool
}

// Whether the provider's prefix counts toward its network's units: admitted,
// or Online in a country whose URL probes make no UDP leg.
// udpProbesDisabled says which countries those are; nil is none. A provider
// the export did not decide (nil) counts nothing.
func (self *SnTopAdmissionProvider) CountsUnits(udpProbesDisabled func(countryCode string) bool) bool {
	if self == nil {
		return false
	}
	if self.Admitted {
		return true
	}
	return self.Online && udpProbesDisabled != nil && udpProbesDisabled(self.CountryCode)
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

// The encoded set: the export's unix milliseconds (8), the count (4), then per
// provider its client id (16), country (2) and flags (1: bit 0 admitted, bit 1
// online), in client id order.
const (
	snTopAdmissionHeaderBytes   = 12
	snTopAdmissionProviderBytes = 19
	snTopAdmissionFlagAdmitted  = 1
	snTopAdmissionFlagOnline    = 2
)

func encodeSnTopAdmission(admission *SnTopAdmission) []byte {
	providers := make([]*SnTopAdmissionProvider, 0, len(admission.Providers))
	for _, provider := range admission.Providers {
		providers = append(providers, provider)
	}
	slices.SortFunc(providers, func(a, b *SnTopAdmissionProvider) int { return a.ClientId.Cmp(b.ClientId) })
	b := make([]byte, 0, snTopAdmissionHeaderBytes+snTopAdmissionProviderBytes*len(providers))
	b = binary.BigEndian.AppendUint64(b, uint64(admission.MeasuredAt.UnixMilli()))
	b = binary.BigEndian.AppendUint32(b, uint32(len(providers)))
	for _, provider := range providers {
		b = append(b, provider.ClientId.Bytes()...)
		country := CtfNormalizeCountryCode(provider.CountryCode)
		b = append(b, country[0], country[1])
		var flags byte
		if provider.Admitted {
			flags |= snTopAdmissionFlagAdmitted
		}
		if provider.Online {
			flags |= snTopAdmissionFlagOnline
		}
		b = append(b, flags)
	}
	return b
}

func decodeSnTopAdmission(b []byte) (*SnTopAdmission, error) {
	if len(b) < snTopAdmissionHeaderBytes {
		return nil, fmt.Errorf("sn top admission set holds %d bytes, less than its header", len(b))
	}
	measuredAt := time.UnixMilli(int64(binary.BigEndian.Uint64(b[0:8]))).UTC()
	count := int(binary.BigEndian.Uint32(b[8:12]))
	if len(b) != snTopAdmissionHeaderBytes+count*snTopAdmissionProviderBytes {
		return nil, fmt.Errorf("sn top admission set holds %d bytes for %d providers", len(b), count)
	}
	admission := &SnTopAdmission{
		MeasuredAt: measuredAt,
		Providers:  make(map[server.Id]*SnTopAdmissionProvider, count),
	}
	for i := range count {
		o := snTopAdmissionHeaderBytes + i*snTopAdmissionProviderBytes
		clientId, err := server.IdFromBytes(b[o : o+16])
		if err != nil {
			return nil, err
		}
		countryCode := string(b[o+16 : o+18])
		if CtfNormalizeCountryCode(countryCode) != countryCode {
			return nil, fmt.Errorf("sn top admission set: provider %d has country %q", i, countryCode)
		}
		flags := b[o+18]
		if flags&^(snTopAdmissionFlagAdmitted|snTopAdmissionFlagOnline) != 0 {
			return nil, fmt.Errorf("sn top admission set: provider %d has flags %x", i, flags)
		}
		admission.Providers[clientId] = &SnTopAdmissionProvider{
			ClientId:    clientId,
			CountryCode: countryCode,
			Admitted:    flags&snTopAdmissionFlagAdmitted != 0,
			Online:      flags&snTopAdmissionFlagOnline != 0,
		}
	}
	return admission, nil
}

// The export's admission set from its decided scores: each provider once, from
// any of the location maps it sits in, since every map decides it the same way
// (rankClientScore). Admitted is native Speed or Quality membership, on the
// strict decision (the top-level score, not a capability facet's classified
// copy); Online is the URL bar's, so a membership only the country floor gave
// is none. The country is the one the provider is published under.
func newSnTopAdmission(
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
) *SnTopAdmission {
	admission := &SnTopAdmission{
		MeasuredAt: measuredAt.UTC(),
		Providers:  map[server.Id]*SnTopAdmissionProvider{},
	}
	for _, clientScores := range locationClientScores {
		for clientId, clientScore := range clientScores {
			if _, ok := admission.Providers[clientId]; ok {
				continue
			}
			countryCode := CtfCountryUnknown
			if egress := clientIdEgresses[clientId]; egress != nil {
				countryCode = CtfNormalizeCountryCode(normalizeCountryCode(egress.publishedCountryCode))
			}
			admission.Providers[clientId] = &SnTopAdmissionProvider{
				ClientId:    clientId,
				CountryCode: countryCode,
				Admitted:    clientScore.PassesMinimums[RankModeQuality] || clientScore.PassesMinimums[RankModeSpeed],
				Online:      clientScore.Online && !clientScore.countryFloor,
			}
		}
	}
	return admission
}

// publishSnTopAdmission writes the export's admission set for the board. It
// runs inside the export and never fails it: an error is logged, and the board
// fold finds a stale set and skips the tempo rather than count without it.
func publishSnTopAdmission(
	ctx context.Context,
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[sntop]publish the export's admission set failed: %v\n", r)
		}
	}()
	admission := newSnTopAdmission(measuredAt, locationClientScores, clientIdEgresses)
	value := encodeSnTopAdmission(admission)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snTopAdmissionKey, value, snTopAdmissionTtl).Err())
	})
	admitted := 0
	for _, provider := range admission.Providers {
		if provider.Admitted {
			admitted += 1
		}
	}
	glog.Infof("[sntop]published the admission set of the export at %s: %d providers, %d admitted\n", admission.MeasuredAt.Format(time.RFC3339), len(admission.Providers), admitted)
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

package controller

import (
	"context"
	"encoding/hex"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The extenders lag.dev can measure (lag.dev/PLAN.md A4, A6): every extender
// that serves the measurement api now. That is the set the measurement dns
// publishes (GetExtenderMeasurementHosts, C8) -- measurable, with a live
// certificate for its name -- narrowed to the extenders whose request is
// answered `issued` (A8). One withdrawn (C9), an owner's opt-out included
// (decision 18), or waiting on a new request, is not offered, whatever
// certificate it held. An extender has one measurement name, its id under
// the measurement zone (N1), which resolves to its active addresses of both
// families (N3), so there is one entry per extender. These are already
// public through the certificate logs and dns (§2), so the sample is not
// partitioned the way the directory's channels are.

// One extender that serves the measurement api now, with where it is.
type lagServingExtender struct {
	*ExtenderMeasurementHost
	Place *model.LagPlace
}

// The serving extenders as of loadTime, with an index by extender for the
// track tickets (A6).
type lagServingSet struct {
	loadTime  time.Time
	extenders []*lagServingExtender
	byId      map[server.Id]*lagServingExtender
}

func (self *lagServingSet) get(extenderId server.Id) *lagServingExtender {
	return self.byId[extenderId]
}

// The set is read from every live certificate and every address of its
// extender, so it is shared by every request for extender_sample_cache.
var lagServingCache = struct {
	sync.Mutex
	set *lagServingSet
}{}

// Testing_ResetLagServing drops the cached set, so the next request reads the
// tables a test just wrote.
func Testing_ResetLagServing() {
	lagServingCache.Lock()
	defer lagServingCache.Unlock()
	lagServingCache.set = nil
}

// The extenders of a cached set whose request is still answered issued, read
// now: a withdrawal, an owner's opt-out included, takes effect at the next
// request rather than when the cache is next read (C9). One read for the
// extenders a request offers or tickets, at most 64.
func lagStillIssued(ctx context.Context, extenders []*lagServingExtender) []*lagServingExtender {
	extenderIds := make([]server.Id, 0, len(extenders))
	for _, extender := range extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	measurements := model.GetLagExtenderMeasurements(ctx, extenderIds)
	issued := []*lagServingExtender{}
	for _, extender := range extenders {
		if measurement := measurements[extender.ExtenderId]; measurement != nil && measurement.State == model.NetworkExtenderMeasurementStateIssued {
			issued = append(issued, extender)
		}
	}
	return issued
}

// The serving set, read again when the cached one is older than maxAge.
func getLagServing(ctx context.Context, now time.Time, maxAge time.Duration) *lagServingSet {
	lagServingCache.Lock()
	defer lagServingCache.Unlock()
	if set := lagServingCache.set; set != nil && !now.Before(set.loadTime) && now.Sub(set.loadTime) < maxAge {
		return set
	}
	set := loadLagServing(ctx, now)
	lagServingCache.set = set
	return set
}

// Reads the extenders that serve the measurement api, each placed by its
// derived location, else by GeoLite2 City for its first address (A4). An
// extender that cannot be placed is left out: lag.dev picks its dots by
// distance (L3), and a dot it cannot place it cannot pick. A deployment with
// no extender network serves none, and neither does one whose measurement
// block is off, whose records then carry no measurement id either (C10).
func loadLagServing(ctx context.Context, now time.Time) *lagServingSet {
	set := &lagServingSet{
		loadTime:  now,
		extenders: []*lagServingExtender{},
		byId:      map[server.Id]*lagServingExtender{},
	}
	config, err := EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return set
	}

	hosts := GetExtenderMeasurementHosts(ctx, config, now)
	extenderIds := make([]server.Id, 0, len(hosts))
	for _, host := range hosts {
		extenderIds = append(extenderIds, host.ExtenderId)
	}
	measurements := model.GetLagExtenderMeasurements(ctx, extenderIds)
	derivedPlaces := model.GetLagExtenderDerivedPlaces(ctx, extenderIds)
	for _, host := range hosts {
		if len(host.Ips) == 0 {
			continue
		}
		measurement := measurements[host.ExtenderId]
		if measurement == nil || measurement.State != model.NetworkExtenderMeasurementStateIssued {
			continue
		}
		place := derivedPlaces[host.ExtenderId]
		if place == nil {
			place = lagAddressPlace(host.Ips[0])
		}
		if place == nil {
			continue
		}
		extender := &lagServingExtender{
			ExtenderMeasurementHost: host,
			Place:                   place,
		}
		set.extenders = append(set.extenders, extender)
		set.byId[host.ExtenderId] = extender
	}
	return set
}

// Up to count extenders of the set in a random order that takes one extender
// from each country in turn, so the sample spreads across countries before it
// repeats one (A4).
func sampleLagServing(set *lagServingSet, count int) []*lagServingExtender {
	countryExtenders := map[string][]*lagServingExtender{}
	countries := []string{}
	for _, extender := range set.extenders {
		countryCode := extender.Place.CountryCode
		if _, ok := countryExtenders[countryCode]; !ok {
			countries = append(countries, countryCode)
		}
		countryExtenders[countryCode] = append(countryExtenders[countryCode], extender)
	}

	mathrand.Shuffle(len(countries), func(i, j int) {
		countries[i], countries[j] = countries[j], countries[i]
	})
	for _, extenders := range countryExtenders {
		mathrand.Shuffle(len(extenders), func(i, j int) {
			extenders[i], extenders[j] = extenders[j], extenders[i]
		})
	}

	sample := []*lagServingExtender{}
	for round := 0; len(sample) < count; round += 1 {
		pickedInRound := false
		for _, countryCode := range countries {
			if count <= len(sample) {
				break
			}
			extenders := countryExtenders[countryCode]
			if len(extenders) <= round {
				continue
			}
			sample = append(sample, extenders[round])
			pickedInRound = true
		}
		if !pickedInRound {
			break
		}
	}
	return sample
}

// A place as the sample answers it.
type LagExtenderLocation struct {
	City        string  `json:"city,omitempty"`
	Region      string  `json:"region,omitempty"`
	CountryCode string  `json:"country_code"`
	CountryName string  `json:"country_name,omitempty"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

// One extender that serves the measurement api.
type LagExtender struct {
	ExtenderId server.Id `json:"extender_id"`
	// the extender's ed25519 identity key, hex
	PublicKey string `json:"public_key"`
	// the measurement name (N1), which the browser connects to
	Host string `json:"host"`
	// the extender's active addresses, canonical, IPv4 first; the name
	// resolves to these, and the site colors the dot's ring by the first (L5)
	Ips      []string             `json:"ips"`
	Location *LagExtenderLocation `json:"location"`
}

type LagExtendersResult struct {
	Extenders []*LagExtender `json:"extenders"`
}

func newLagExtender(extender *lagServingExtender) *LagExtender {
	ips := make([]string, 0, len(extender.Ips))
	for _, ip := range extender.Ips {
		ips = append(ips, ip.String())
	}
	return &LagExtender{
		ExtenderId: extender.ExtenderId,
		PublicKey:  hex.EncodeToString(extender.PublicKey),
		Host:       extender.Name,
		Ips:        ips,
		Location: &LagExtenderLocation{
			City:        extender.Place.City,
			Region:      extender.Place.Region,
			CountryCode: extender.Place.CountryCode,
			CountryName: extender.Place.CountryName,
			Latitude:    extender.Place.Latitude,
			Longitude:   extender.Place.Longitude,
		},
	}
}

// Answers `GET /lag/extenders` (A4): up to extender_sample extenders that
// serve the measurement api now, randomized per request and spread across
// countries.
func LagExtenders(clientSession *session.ClientSession) (*LagExtendersResult, error) {
	settings := GetLagSettings()
	set := getLagServing(clientSession.Ctx, server.NowUtc(), settings.ExtenderSampleCache)
	result := &LagExtendersResult{
		Extenders: []*LagExtender{},
	}
	for _, extender := range lagStillIssued(clientSession.Ctx, sampleLagServing(set, settings.ExtenderSample)) {
		result.Extenders = append(result.Extenders, newLagExtender(extender))
	}
	return result, nil
}

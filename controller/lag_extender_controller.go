package controller

import (
	"context"
	"encoding/hex"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The extenders lag.dev can measure (lag.dev/PLAN.md A4, A6): every extender
// that serves the measurement api now. That is the set the measurement dns
// publishes (GetExtenderMeasurementHosts, C8) -- measurable, with a live
// certificate for its name -- narrowed to the extenders whose request is
// answered `issued` (A8) and whose certificate A8 has handed to them. The dns
// publishes a new name within a minute or two of its issuance, but the
// extender installs the certificate only at its next A8 poll, up to half an
// hour later, so a name offered before that would answer without it. One
// withdrawn (C9), an owner's opt-out included (decision 18), or waiting on a
// new request, is not offered, whatever certificate it held. An extender has
// one measurement name, its id under the measurement zone (N1), which
// resolves to its active addresses of both families (N3), so there is one
// entry per extender. These are already public through the certificate logs
// and dns (§2), so the sample is not partitioned the way the directory's
// channels are.

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

// The extenders of a cached set that still serve, read at `now`: their
// request is answered issued and its certificate is delivered. A withdrawal,
// an owner's opt-out included, takes effect at the next request rather than
// when the cache is next read (C9), and so does a new key's request, whose
// certificate the extender has yet to read. One read for the extenders a
// request offers or tickets, at most 64.
func lagStillServing(ctx context.Context, extenders []*lagServingExtender, now time.Time) []*lagServingExtender {
	extenderIds := make([]server.Id, 0, len(extenders))
	for _, extender := range extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	measurements := model.GetLagExtenderMeasurements(ctx, extenderIds, now)
	serving := []*lagServingExtender{}
	for _, extender := range extenders {
		if measurement := measurements[extender.ExtenderId]; measurement != nil && measurement.Serving() {
			serving = append(serving, extender)
		}
	}
	return serving
}

// What the sample offers as of one read, and what each rule made of the set.
type lagOffer struct {
	// the extenders that serve and can be placed
	serving *lagServingSet
	// those in no restart loop (lagStableServing), and whether that read answered
	stable    *lagServingSet
	loopsRead bool
	// those the probes can also reach (lagReachableServing), and whether that read
	// answered
	reachable  *lagServingSet
	probesRead bool
	// what the sample offers: reachable, unless that leaves nothing
	offered *lagServingSet
	// the rules would have left out every serving extender
	floored bool
}

// Reads the serving set and applies the rules. They steer a visitor from a bad
// dot to a good one, so they have nothing to say when no good one is left. If
// together they would leave out every serving extender, the whole set is offered:
// a page of dots that may fail is no worse than a page with none, and on a small
// deployment one that restarts and one that stalls would otherwise empty lag.dev,
// where more often than not the rules or the prober are what is wrong.
func loadLagOffer(ctx context.Context, now time.Time) *lagOffer {
	serving := loadLagServing(ctx, now)
	stable, loopsRead := lagStableServing(ctx, serving, now)
	reachable, probesRead := lagReachableServing(ctx, stable, now)
	offer := &lagOffer{
		serving:    serving,
		stable:     stable,
		loopsRead:  loopsRead,
		reachable:  reachable,
		probesRead: probesRead,
		offered:    reachable,
	}
	if 0 < len(serving.extenders) && len(reachable.extenders) == 0 {
		offer.offered = serving
		offer.floored = true
	}
	return offer
}

// What the sample offers now (loadLagOffer), read again when the cached one is
// older than maxAge.
func getLagServing(ctx context.Context, now time.Time, maxAge time.Duration) *lagServingSet {
	lagServingCache.Lock()
	defer lagServingCache.Unlock()
	if set := lagServingCache.set; set != nil && !now.Before(set.loadTime) && now.Sub(set.loadTime) < maxAge {
		return set
	}
	offer := loadLagOffer(ctx, now)
	if offer.floored {
		glog.Errorf(
			"[lag]the restart loop and probe rules would leave out all %d serving extenders (%d in a restart loop, %d unreachable), offering them all\n",
			len(offer.serving.extenders),
			len(offer.serving.extenders)-len(offer.stable.extenders),
			len(offer.stable.extenders)-len(offer.reachable.extenders),
		)
	}
	lagServingCache.set = offer.offered
	return offer.offered
}

// GetLagServingHosts reads the extenders that serve the measurement api:
// published, issued, and delivered (lagStillServing), with the addresses their
// names resolve to. A deployment with no extender network serves none, and
// neither does one whose measurement block is off, whose records then carry no
// measurement id either (C10). The sample draws from these (loadLagServing),
// and the probes probe them whether or not the sample can place them or leaves
// them out now, so an extender left out for its probes comes back with its
// first success (lagReachableServing).
func GetLagServingHosts(ctx context.Context, now time.Time) []*ExtenderMeasurementHost {
	hosts := []*ExtenderMeasurementHost{}
	config, err := EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return hosts
	}

	published := GetExtenderMeasurementHosts(ctx, config, now)
	extenderIds := make([]server.Id, 0, len(published))
	for _, host := range published {
		extenderIds = append(extenderIds, host.ExtenderId)
	}
	measurements := model.GetLagExtenderMeasurements(ctx, extenderIds, now)
	for _, host := range published {
		if len(host.Ips) == 0 {
			continue
		}
		measurement := measurements[host.ExtenderId]
		if measurement == nil || !measurement.Serving() {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// Reads the extenders that serve the measurement api (GetLagServingHosts).
// Each is placed where the issuance order counts it (A4, C7;
// model.GetLagExtenderPlaces): by GeoLite2 City for its addresses, choosing
// between its two families when they disagree, and moved to its derived
// location only within that country. An extender that cannot be placed is left
// out: lag.dev picks its dots by distance (L3), and a dot it cannot place it
// cannot pick.
func loadLagServing(ctx context.Context, now time.Time) *lagServingSet {
	set := &lagServingSet{
		loadTime:  now,
		extenders: []*lagServingExtender{},
		byId:      map[server.Id]*lagServingExtender{},
	}

	hosts := GetLagServingHosts(ctx, now)
	extenderIds := make([]server.Id, 0, len(hosts))
	for _, host := range hosts {
		extenderIds = append(extenderIds, host.ExtenderId)
	}
	places := model.GetLagExtenderPlaces(ctx, extenderIds)
	for _, host := range hosts {
		place := places[host.ExtenderId]
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
	now := server.NowUtc()
	set := getLagServing(clientSession.Ctx, now, settings.ExtenderSampleCache)
	result := &LagExtendersResult{
		Extenders: []*LagExtender{},
	}
	for _, extender := range lagStillServing(clientSession.Ctx, sampleLagServing(set, settings.ExtenderSample), now) {
		result.Extenders = append(result.Extenders, newLagExtender(extender))
	}
	return result, nil
}

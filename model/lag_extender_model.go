package model

import (
	"context"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Where lag.dev places an extender (lag.dev/PLAN.md A4), which is also the
// country the issuance order counts it in (C7): one rule for both, so the
// order spreads new certificates over the countries visitors see.
//
// The place is GeoLite2 City's for the extender's addresses, one of each
// family, chosen between the two families by lagExtenderAddressPlace. The
// extender's derived location (connect/GEOMAP.md §6) may move the dot within
// that country, never out of it, and only while the derivation is close
// enough to trust (refineLagExtenderPlace). An extender GeoLite2 places
// nowhere has no place: the sample leaves it out, and the order counts it in
// the country it last activated from.

// A place as the extender sample answers it. Names are English, the country
// code lower case, and a name the place does not reach is empty.
type LagPlace struct {
	City        string
	Region      string
	CountryCode string
	CountryName string
	Latitude    float64
	Longitude   float64
}

// The most an extender's derived location may be off, as the RMS residual of
// its pings in km, for lag.dev to move the extender's dot to it. It is the
// 50 km of the monitor's DeriveMaxResidualKm (monitor/signal_derived_locations.go),
// the fleet residual a derivation that explains its pings stays under, applied
// here to the extender's own; the monitor's is a setting in a package that
// imports this one, so the value is repeated rather than shared. On Main at
// 2026-11-01 every extender's was over 6,800 km, the median 45,076 km, and 49
// of 114 derived points had crossed a border.
const LagExtenderDerivedPlaceMaxResidualKm = 50.0

// What GeoLite2 says of one address of an extender.
type LagAddressPlace struct {
	// GeoLite2 City's place for the address, with a country
	Place *LagPlace
	// the radius in km within which GeoLite2 City places the address, 0 when
	// it gives none
	AccuracyRadiusKm int
	// the autonomous system GeoLite2 ASN says announces the address, 0 when
	// the address has no record or no GeoLite2 ASN database is deployed
	Asn uint32
}

// What GeoLite2 says of each address, by address: an entry for each address
// GeoLite2 City places in a country, and none for the rest. An extender's
// address is public through its measurement name (lag.dev/PLAN.md §2), so its
// place keeps the block's own coordinates, which serve distance (L3) better
// than the city's; a visitor's place is the city's point, never this
// (controller lagClientPlace). Without a GeoLite2 City database there is no
// entry at all, which is logged once per call rather than once per address.
// Tests replace it.
var lagAddressPlaces = func(ips []netip.Addr) map[netip.Addr]*LagAddressPlace {
	addressPlaces := map[netip.Addr]*LagAddressPlace{}
	server.HandleError(func() {
		for _, ip := range ips {
			ipInfo, err := server.GetIpInfo(ip)
			if err != nil || ipInfo.CountryCode == "" {
				continue
			}
			addressPlace := &LagAddressPlace{
				Place: &LagPlace{
					City:        ipInfo.City,
					Region:      ipInfo.Region,
					CountryCode: ipInfo.CountryCode,
					CountryName: ipInfo.Country,
					Latitude:    ipInfo.Latitude,
					Longitude:   ipInfo.Longitude,
				},
				AccuracyRadiusKm: ipInfo.AccuracyRadiusKm,
			}
			if asn := server.GetIpAsn(ip); asn != nil {
				addressPlace.Asn = asn.Number
			}
			addressPlaces[ip] = addressPlace
		}
	})
	return addressPlaces
}

// Testing_ReplaceLagAddressPlaces makes lag.dev read what GeoLite2 says of an
// address from `lookup`, nil for an address it places nowhere, rather than
// from the deployed databases, until restore is called.
func Testing_ReplaceLagAddressPlaces(lookup func(ip netip.Addr) *LagAddressPlace) (restore func()) {
	previous := lagAddressPlaces
	lagAddressPlaces = func(ips []netip.Addr) map[netip.Addr]*LagAddressPlace {
		addressPlaces := map[netip.Addr]*LagAddressPlace{}
		for _, ip := range ips {
			if addressPlace := lookup(ip); addressPlace != nil && addressPlace.Place != nil && addressPlace.Place.CountryCode != "" {
				addressPlaces[ip] = addressPlace
			}
		}
		return addressPlaces
	}
	return func() {
		lagAddressPlaces = previous
	}
}

// The place of an extender from what GeoLite2 says of its IPv4 and its IPv6
// address, each nil when the extender has no such address or GeoLite2 places
// it nowhere (lag.dev/PLAN.md A4):
//
//   - one family placed: its place;
//   - both in one country: the address GeoLite2 places more surely, by the
//     smaller accuracy radius, an unknown radius the least sure, and the IPv4
//     address's on a tie. The country is the same either way, and the city
//     and the point are the surer ones: L3 picks near and far by the point,
//     and a country-only record sits at the country's center, for the US a
//     point in Kansas;
//   - in two countries, announced by two autonomous systems, or by one that
//     GeoLite2 ASN does not know: the IPv6 address's. IPv4 space is leased and
//     moved between networks, and GeoLite2 goes on placing it where it was. On
//     Main at 2026-11-01 an extender in Hong Kong had an IPv4 address GeoLite2
//     placed in Riga, to 5 km, from a block RIPE lists in Hong Kong;
//   - in two countries, announced by one autonomous system: the address
//     GeoLite2 places more surely, by the smaller accuracy radius, an unknown
//     radius the least sure, and the IPv6 address's on a tie. An operator in
//     Hong Kong with a Malaysian network's addresses has its IPv4 address
//     placed in a city and its IPv6 address only in the network's country.
//
// Nil when neither family is placed.
func lagExtenderAddressPlace(v4 *LagAddressPlace, v6 *LagAddressPlace) *LagPlace {
	switch {
	case v4 == nil && v6 == nil:
		return nil
	case v6 == nil:
		return v4.Place
	case v4 == nil:
		return v6.Place
	case v4.Place.CountryCode == v6.Place.CountryCode:
		if lagAccuracyRadiusRank(v6.AccuracyRadiusKm) < lagAccuracyRadiusRank(v4.AccuracyRadiusKm) {
			return v6.Place
		}
		return v4.Place
	case v4.Asn == 0 || v6.Asn == 0 || v4.Asn != v6.Asn:
		return v6.Place
	case lagAccuracyRadiusRank(v4.AccuracyRadiusKm) < lagAccuracyRadiusRank(v6.AccuracyRadiusKm):
		return v4.Place
	default:
		return v6.Place
	}
}

// An accuracy radius to compare by, an unknown one the widest.
func lagAccuracyRadiusRank(accuracyRadiusKm int) int {
	if accuracyRadiusKm <= 0 {
		return math.MaxInt
	}
	return accuracyRadiusKm
}

// An extender's derived location as lag.dev may use it: its point, the names
// of the place the point maps to, and how far off the derivation is.
type lagDerivedPlace struct {
	place      *LagPlace
	residualKm float32
}

// Moves the dot to the extender's derived location when the point maps into
// the country GeoLite2 places the extender in and its residual is at most
// LagExtenderDerivedPlaceMaxResidualKm. The dot then takes the derived point
// and the names of its city and region; the country, and its name, stay
// GeoLite2's. Otherwise the place is GeoLite2's, whole.
func refineLagExtenderPlace(place *LagPlace, derived *lagDerivedPlace) *LagPlace {
	if place == nil || derived == nil {
		return place
	}
	if derived.place.CountryCode != place.CountryCode {
		return place
	}
	// written so that a residual that is not a number does not pass
	if !(derived.residualKm <= LagExtenderDerivedPlaceMaxResidualKm) {
		return place
	}
	return &LagPlace{
		City:        derived.place.City,
		Region:      derived.place.Region,
		CountryCode: place.CountryCode,
		CountryName: place.CountryName,
		Latitude:    derived.place.Latitude,
		Longitude:   derived.place.Longitude,
	}
}

// GetLagExtenderPlaces places each of the given extenders where the lag.dev
// sample shows it and the issuance order counts it (A4, C7), by extender id.
// An extender placed nowhere, or that is gone, is absent.
func GetLagExtenderPlaces(ctx context.Context, extenderIds []server.Id) map[server.Id]*LagPlace {
	places := map[server.Id]*LagPlace{}
	if len(extenderIds) == 0 {
		return places
	}
	server.Db(ctx, func(conn server.PgConn) {
		places = getLagExtenderPlacesInConn(ctx, conn, extenderIds)
	})
	return places
}

// GetLagExtenderPlaces on the caller's connection.
//
// An extender is placed by the addresses it is measured at: its active
// addresses that carry tcp, which are the ones the sample lists and its name
// resolves to (A4, N3). An extender with none active is away, or about to be
// (amendment 23), and is placed by the last address of each family it had,
// so the order counts a holder that is away where it is to serve again.
func getLagExtenderPlacesInConn(ctx context.Context, conn server.PgConn, extenderIds []server.Id) map[server.Id]*LagPlace {
	places := map[server.Id]*LagPlace{}
	if len(extenderIds) == 0 {
		return places
	}

	// by extender, by family
	activeAddresses := map[server.Id]map[int]netip.Addr{}
	lastAddresses := map[server.Id]map[int]netip.Addr{}
	result, err := conn.Query(
		ctx,
		`
		SELECT
			extender_id,
			ip_version,
			ip,
			carriers,
			active
		FROM network_extender_address
		WHERE extender_id = ANY($1)
		`,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var extenderId server.Id
			var ipVersion int
			var ip netip.Addr
			var carriers string
			var active bool
			server.Raise(result.Scan(
				&extenderId,
				&ipVersion,
				&ip,
				&carriers,
				&active,
			))
			if !slices.Contains(splitExtenderCarriers(carriers), connect.ExtenderCarrierTcp) {
				continue
			}
			ip = ip.Unmap().WithZone("")
			if lastAddresses[extenderId] == nil {
				lastAddresses[extenderId] = map[int]netip.Addr{}
			}
			lastAddresses[extenderId][ipVersion] = ip
			if active {
				if activeAddresses[extenderId] == nil {
					activeAddresses[extenderId] = map[int]netip.Addr{}
				}
				activeAddresses[extenderId][ipVersion] = ip
			}
		}
	})

	derivedPlaces := map[server.Id]*lagDerivedPlace{}
	result, err = conn.Query(
		ctx,
		`
		SELECT
			derived_location.node_id,
			derived_location.latitude,
			derived_location.longitude,
			derived_location.residual_km,
			COALESCE(city.location_name, ''),
			COALESCE(region.location_name, ''),
			COALESCE(country.location_name, ''),
			COALESCE(country.country_code, '')
		FROM derived_location
		LEFT JOIN location AS city ON city.location_id = derived_location.city_location_id
		LEFT JOIN location AS region ON region.location_id = derived_location.region_location_id
		LEFT JOIN location AS country ON country.location_id = derived_location.country_location_id
		WHERE
			derived_location.node_kind = $1 AND
			derived_location.node_id = ANY($2)
		`,
		DerivedLocationNodeKindExtender,
		extenderIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var extenderId server.Id
			derived := &lagDerivedPlace{
				place: &LagPlace{},
			}
			server.Raise(result.Scan(
				&extenderId,
				&derived.place.Latitude,
				&derived.place.Longitude,
				&derived.residualKm,
				&derived.place.City,
				&derived.place.Region,
				&derived.place.CountryName,
				&derived.place.CountryCode,
			))
			derived.place.CountryCode = strings.ToLower(strings.TrimSpace(derived.place.CountryCode))
			derivedPlaces[extenderId] = derived
		}
	})

	familyAddresses := func(extenderId server.Id) map[int]netip.Addr {
		if addresses := activeAddresses[extenderId]; 0 < len(addresses) {
			return addresses
		}
		return lastAddresses[extenderId]
	}

	ips := []netip.Addr{}
	for extenderId := range lastAddresses {
		for _, ip := range familyAddresses(extenderId) {
			ips = append(ips, ip)
		}
	}
	addressPlaces := lagAddressPlaces(ips)

	for extenderId := range lastAddresses {
		addresses := familyAddresses(extenderId)
		var v4 *LagAddressPlace
		if ip, ok := addresses[4]; ok {
			v4 = addressPlaces[ip]
		}
		var v6 *LagAddressPlace
		if ip, ok := addresses[6]; ok {
			v6 = addressPlaces[ip]
		}
		if place := refineLagExtenderPlace(lagExtenderAddressPlace(v4, v6), derivedPlaces[extenderId]); place != nil {
			places[extenderId] = place
		}
	}
	return places
}

// An extender's latest measurement request as the lag.dev sample reads it
// (lag.dev/PLAN.md A4, A8, C9), with whether the extender holds a
// certificate for it.
type LagExtenderMeasurement struct {
	*NetworkExtenderMeasurement
	// whether a live certificate for the request's names and key has its name
	// published (C8) and has been handed to the extender by A8 (deliver_time),
	// which installs it only when it reads it there
	Delivered bool
}

// Whether the extender serves its measurement name now, as far as its
// request says: the request is answered `issued`, and its certificate is
// published and delivered. A pending, refused or withdrawn request, an
// owner's opt-out included, is one that does not.
func (self *LagExtenderMeasurement) Serving() bool {
	return self.State == NetworkExtenderMeasurementStateIssued && self.Delivered
}

// The latest measurement request of each of the given extenders, by extender
// id, as the certificate model keeps it, with whether a certificate for it is
// delivered at `now`: its state is A8's answer, and an issued request serves
// only once A8 has handed the extender its certificate. Until then, up to
// half an hour after its name is published, the name answers without it. An
// extender with no request is absent.
func GetLagExtenderMeasurements(ctx context.Context, extenderIds []server.Id, now time.Time) map[server.Id]*LagExtenderMeasurement {
	measurements := map[server.Id]*LagExtenderMeasurement{}
	if len(extenderIds) == 0 {
		return measurements
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				`+networkExtenderMeasurementColumns+`,
				EXISTS (
					SELECT 1
					FROM network_extender_measurement_cert
					WHERE
						network_extender_measurement_cert.extender_id = request.extender_id AND
						network_extender_measurement_cert.names = request.names AND
						network_extender_measurement_cert.public_key = request.public_key AND
						network_extender_measurement_cert.withdraw_time IS NULL AND
						network_extender_measurement_cert.publish_time IS NOT NULL AND
						network_extender_measurement_cert.deliver_time IS NOT NULL AND
						$2 < network_extender_measurement_cert.not_after
				)
			FROM network_extender_measurement AS request
			WHERE request.extender_id = ANY($1)
			`,
			extenderIds,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				measurement := &LagExtenderMeasurement{}
				measurement.NetworkExtenderMeasurement = scanNetworkExtenderMeasurement(result, &measurement.Delivered)
				measurements[measurement.ExtenderId] = measurement
			}
		})
	})
	return measurements
}

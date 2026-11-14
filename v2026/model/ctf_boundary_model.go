package model

// What capture the flag reads of the operator's miners (sn CAPTURETHEFLAG.md
// §4.2): the extenders a presence sample finds, and the facts of every miner
// at an epoch's end boundary, which the census step reads once, right after
// the boundary is final, as the users count is.

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The rows of one bulk read by client ids.
const ctfReadBatch = 5000

// One extender record a presence sample found present (X2): active and
// unrevoked, with an active address whose challenge passed in the sample's
// quarter hour.
type CtfExtenderPresence struct {
	ExtenderId server.Id
	// the provider client the record names, which the census counts it as
	ClientId server.Id
	// the record's country: its derived location's, else its activation's
	CountryCode string
	// its passing addresses, nil for a family without one
	Ipv4 *netip.Addr
	Ipv6 *netip.Addr
}

// GetCtfExtenderPresence reads every extender record that is active and
// unrevoked and has an active address whose challenge passed at or after
// passedAfter.
func GetCtfExtenderPresence(ctx context.Context, passedAfter time.Time) []*CtfExtenderPresence {
	byExtender := map[server.Id]*CtfExtenderPresence{}
	order := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.extender_id,
				network_extender.client_id,
				network_extender.country_code,
				COALESCE(derived_country.country_code, ''),
				network_extender_address.ip_version,
				network_extender_address.ip
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active AND
				$1 <= network_extender_address.last_probe_success_time
			LEFT JOIN derived_location ON
				derived_location.node_kind = $2 AND
				derived_location.node_id = network_extender.extender_id
			LEFT JOIN location AS derived_country ON
				derived_country.location_id = derived_location.country_location_id
			WHERE network_extender.active AND network_extender.revoke_time IS NULL
			ORDER BY network_extender.extender_id, network_extender_address.ip_version
			`,
			passedAfter.UTC(),
			DerivedLocationNodeKindExtender,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extenderId, clientId server.Id
				var countryCode, derivedCountryCode string
				var ipVersion int
				var ip netip.Addr
				server.Raise(result.Scan(&extenderId, &clientId, &countryCode, &derivedCountryCode, &ipVersion, &ip))
				presence := byExtender[extenderId]
				if presence == nil {
					presence = &CtfExtenderPresence{
						ExtenderId:  extenderId,
						ClientId:    clientId,
						CountryCode: ctfRecordCountryCode(derivedCountryCode, countryCode),
					}
					byExtender[extenderId] = presence
					order = append(order, extenderId)
				}
				address := ip
				switch ipVersion {
				case 4:
					presence.Ipv4 = &address
				case 6:
					presence.Ipv6 = &address
				}
			}
		})
	})
	presences := make([]*CtfExtenderPresence, len(order))
	for i, extenderId := range order {
		presences[i] = byExtender[extenderId]
	}
	return presences
}

// The country an extender's signed record carries
// (NetworkExtender.RecordCountryCode), normalized.
func ctfRecordCountryCode(derivedCountryCode string, countryCode string) string {
	if strings.TrimSpace(derivedCountryCode) != "" {
		return CtfNormalizeCountryCode(derivedCountryCode)
	}
	return CtfNormalizeCountryCode(countryCode)
}

// A provider client's facts at the end boundary (P1, P3, P6).
type CtfProviderBoundary struct {
	ClientId  server.Id
	NetworkId server.Id
	// network_client.active, and no source client
	Active   bool
	TopLevel bool
	// network_client_location_reliability.connected and .valid
	Connected     bool
	ValidLocation bool
	// the country it is published under, normalized
	CountryCode string
	// holds a Public provide key
	Public bool
}

// GetCtfProviderBoundaries reads the end-boundary facts of each client that
// still exists, by client id.
func GetCtfProviderBoundaries(ctx context.Context, clientIds []server.Id) map[server.Id]*CtfProviderBoundary {
	boundaries := map[server.Id]*CtfProviderBoundary{}
	for start := 0; start < len(clientIds); start += ctfReadBatch {
		chunk := clientIds[start:min(start+ctfReadBatch, len(clientIds))]
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					network_client.client_id,
					network_client.network_id,
					network_client.active,
					network_client.source_client_id IS NULL,
					COALESCE(network_client_location_reliability.connected, false),
					COALESCE(network_client_location_reliability.valid, false),
					COALESCE(country_location.country_code, ''),
					EXISTS (
						SELECT 1 FROM provide_key
						WHERE
							provide_key.client_id = network_client.client_id AND
							provide_key.provide_mode = $2
					)
				FROM network_client
				LEFT JOIN network_client_location_reliability ON
					network_client_location_reliability.client_id = network_client.client_id
				LEFT JOIN location AS country_location ON
					country_location.location_id = network_client_location_reliability.country_location_id
				WHERE network_client.client_id = ANY($1::uuid[])
				`,
				chunk,
				ProvideModePublic,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					boundary := &CtfProviderBoundary{}
					var countryCode string
					server.Raise(result.Scan(
						&boundary.ClientId,
						&boundary.NetworkId,
						&boundary.Active,
						&boundary.TopLevel,
						&boundary.Connected,
						&boundary.ValidLocation,
						&countryCode,
						&boundary.Public,
					))
					boundary.CountryCode = CtfNormalizeCountryCode(countryCode)
					boundaries[boundary.ClientId] = boundary
				}
			})
		})
	}
	return boundaries
}

// GetCtfClientNetworks reads the network of each client that still exists.
func GetCtfClientNetworks(ctx context.Context, clientIds []server.Id) map[server.Id]server.Id {
	networks := map[server.Id]server.Id{}
	for start := 0; start < len(clientIds); start += ctfReadBatch {
		chunk := clientIds[start:min(start+ctfReadBatch, len(clientIds))]
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT client_id, network_id FROM network_client WHERE client_id = ANY($1::uuid[])`,
				chunk,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId, networkId server.Id
					server.Raise(result.Scan(&clientId, &networkId))
					networks[clientId] = networkId
				}
			})
		})
	}
	return networks
}

// One extender record at the end boundary (X1, X3, X6).
type CtfExtenderBoundary struct {
	ExtenderId server.Id
	ClientId   server.Id
	NetworkId  server.Id
	// active and unrevoked, with at least one active address (E1)
	Active bool
	// published on its own canary channel (connect/EXTENDER.md R4)
	Canary bool
	// the record's country: its derived location's, else its activation's
	CountryCode string
	// the record's country is its derived location's
	Derived bool
	// its active addresses, nil for a family without one
	Ipv4 *netip.Addr
	Ipv6 *netip.Addr
	// the countries of its activations in the epoch, normalized; an
	// activation that resolved to no country is left out. They are GeoLite2's
	// for the activating address, as the record's country is without a
	// derived location.
	ActivationCountryCodes []string
}

// GetCtfExtenderBoundaries reads the records of the provider clients clientIds
// at the end boundary, with the countries of their activations in
// [startTime, endTime], by provider client id, each client's records by
// extender id.
func GetCtfExtenderBoundaries(ctx context.Context, clientIds []server.Id, startTime time.Time, endTime time.Time) map[server.Id][]*CtfExtenderBoundary {
	boundaries := map[server.Id][]*CtfExtenderBoundary{}
	byExtender := map[server.Id]*CtfExtenderBoundary{}
	extenderIds := []server.Id{}
	for start := 0; start < len(clientIds); start += ctfReadBatch {
		chunk := clientIds[start:min(start+ctfReadBatch, len(clientIds))]
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					network_extender.extender_id,
					network_extender.client_id,
					network_extender.network_id,
					network_extender.active AND network_extender.revoke_time IS NULL,
					network_extender.canary_channel IS NOT NULL,
					network_extender.country_code,
					COALESCE(derived_country.country_code, '')
				FROM network_extender
				LEFT JOIN derived_location ON
					derived_location.node_kind = $2 AND
					derived_location.node_id = network_extender.extender_id
				LEFT JOIN location AS derived_country ON
					derived_country.location_id = derived_location.country_location_id
				WHERE network_extender.client_id = ANY($1::uuid[])
				ORDER BY network_extender.client_id, network_extender.extender_id
				`,
				chunk,
				DerivedLocationNodeKindExtender,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					boundary := &CtfExtenderBoundary{}
					var countryCode, derivedCountryCode string
					server.Raise(result.Scan(
						&boundary.ExtenderId,
						&boundary.ClientId,
						&boundary.NetworkId,
						&boundary.Active,
						&boundary.Canary,
						&countryCode,
						&derivedCountryCode,
					))
					boundary.CountryCode = ctfRecordCountryCode(derivedCountryCode, countryCode)
					boundary.Derived = strings.TrimSpace(derivedCountryCode) != ""
					boundaries[boundary.ClientId] = append(boundaries[boundary.ClientId], boundary)
					byExtender[boundary.ExtenderId] = boundary
					extenderIds = append(extenderIds, boundary.ExtenderId)
				}
			})
		})
	}
	for start := 0; start < len(extenderIds); start += ctfReadBatch {
		chunk := extenderIds[start:min(start+ctfReadBatch, len(extenderIds))]
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT extender_id, ip_version, ip
				FROM network_extender_address
				WHERE extender_id = ANY($1::uuid[]) AND active
				`,
				chunk,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var extenderId server.Id
					var ipVersion int
					var ip netip.Addr
					server.Raise(result.Scan(&extenderId, &ipVersion, &ip))
					boundary := byExtender[extenderId]
					address := ip
					switch ipVersion {
					case 4:
						boundary.Ipv4 = &address
					case 6:
						boundary.Ipv6 = &address
					}
				}
			})
			result, err = conn.Query(
				ctx,
				`
				SELECT extender_id, country_code
				FROM network_extender_activation
				WHERE extender_id = ANY($1::uuid[]) AND $2 <= activate_time AND activate_time <= $3
				ORDER BY extender_id, activate_time
				`,
				chunk,
				startTime.UTC(),
				endTime.UTC(),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var extenderId server.Id
					var countryCode string
					server.Raise(result.Scan(&extenderId, &countryCode))
					if strings.TrimSpace(countryCode) == "" {
						continue
					}
					boundary := byExtender[extenderId]
					boundary.ActivationCountryCodes = append(boundary.ActivationCountryCodes, CtfNormalizeCountryCode(countryCode))
				}
			})
		})
	}
	for _, boundary := range byExtender {
		// an active record needs an active address (E1)
		if boundary.Ipv4 == nil && boundary.Ipv6 == nil {
			boundary.Active = false
		}
	}
	return boundaries
}

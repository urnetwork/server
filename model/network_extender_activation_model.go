package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// The activation history of an extender (connect/EXTENDER.md M1): one row per
// activation, carrying where the activating address resolved to and the
// privacy-preserving hash of that address, exactly what a provider's
// connection row and its location row carry together.
//
// network_extender holds only the latest activation's location. This table is
// the history, so a latency attestation (connect/DESIGNNOTES4.md §3) can later
// be placed against where the extender was when it was measured, and so an
// extender that moves is visible as a sequence rather than an overwrite.

type NetworkExtenderActivationRecord struct {
	ActivationId server.Id
	ExtenderId   server.Id
	ActivateTime time.Time
	IpVersion    int
	// nil when the activating address could not be read
	ClientAddressHash []byte
	// empty when the address resolved to no country
	CountryCode       string
	LocationId        *server.Id
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
	// the lookup's accuracy radius in km (connect/GEOMAP.md §5.1); nil when
	// it gave none, and on rows written before the column
	AccuracyKm *float32
}

// Writes the history row of one activation inside the activation transaction.
func insertNetworkExtenderActivationInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
	activation *NetworkExtenderActivation,
	activateTime time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_extender_activation (
			activation_id,
			extender_id,
			activate_time,
			ip_version,
			client_address_hash,
			country_code,
			location_id,
			city_location_id,
			region_location_id,
			country_location_id,
			accuracy_km
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`,
		server.NewId(),
		extenderId,
		activateTime.UTC(),
		activation.IpVersion,
		activation.ClientAddressHash,
		activation.CountryCode,
		activation.LocationId,
		activation.CityLocationId,
		activation.RegionLocationId,
		activation.CountryLocationId,
		activation.AccuracyKm,
	))
}

// One extender of a network that activated one family since a time
// (connect#230): the address it holds on that family now, and the earliest of
// those activations. An activation is stored only once its required test
// passed (C2a), so each of these is a host that proved itself. The operator
// counts proven hosts from them, by its own rules, to scale the network's
// activation budget.
type NetworkExtenderProvenHostCandidate struct {
	ExtenderId        server.Id
	Ip                netip.Addr
	FirstActivateTime time.Time
}

// GetNetworkExtenderProvenHostCandidatesInTx reads, in the caller's
// transaction, every extender of `networkId` that activated `ipVersion` at or
// after `minActivateTime`, with its current address of that family and its
// earliest such activation. An address the uptime probes since deactivated
// still counts: a host that proved itself and went down, as a whole fleet does
// in a power event, is the host whose restart should not queue.
//
// network_extender has no index on network_id, so this scans it. It is read
// only for a network whose budget has reached its floor, and the caller keeps
// the count it makes for a minute.
func GetNetworkExtenderProvenHostCandidatesInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	ipVersion int,
	minActivateTime time.Time,
) []*NetworkExtenderProvenHostCandidate {
	candidates := []*NetworkExtenderProvenHostCandidate{}
	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_extender.extender_id,
			network_extender_address.ip,
			MIN(network_extender_activation.activate_time)
		FROM network_extender
		INNER JOIN network_extender_address ON
			network_extender_address.extender_id = network_extender.extender_id AND
			network_extender_address.ip_version = $2
		INNER JOIN network_extender_activation ON
			network_extender_activation.extender_id = network_extender.extender_id AND
			network_extender_activation.ip_version = $2 AND
			$3 <= network_extender_activation.activate_time
		WHERE network_extender.network_id = $1
		GROUP BY network_extender.extender_id, network_extender_address.ip
		`,
		networkId,
		ipVersion,
		minActivateTime.UTC(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			candidate := &NetworkExtenderProvenHostCandidate{}
			server.Raise(result.Scan(
				&candidate.ExtenderId,
				&candidate.Ip,
				&candidate.FirstActivateTime,
			))
			candidates = append(candidates, candidate)
		}
	})
	return candidates
}

// Where the extender with one identity key last passed activation on one
// family, and the network it belongs to (connect#230): what tells a post of a
// key its network already proved at the caller's address from any other post
// from that address.
type NetworkExtenderKeyAddress struct {
	NetworkId    server.Id
	Ip           netip.Addr
	ActivateTime time.Time
}

// GetNetworkExtenderKeyAddress reads where the extender with `publicKey` last
// passed activation on `ipVersion`, and its network, or nil when the key has
// never passed on that family. An address the uptime probes since deactivated
// is still where it passed.
func GetNetworkExtenderKeyAddress(
	ctx context.Context,
	publicKey []byte,
	ipVersion int,
) *NetworkExtenderKeyAddress {
	var keyAddress *NetworkExtenderKeyAddress
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.network_id,
				network_extender_address.ip,
				network_extender_address.activate_time
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.ip_version = $2
			WHERE network_extender.public_key = $1
			`,
			publicKey,
			ipVersion,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				keyAddress = &NetworkExtenderKeyAddress{}
				server.Raise(result.Scan(
					&keyAddress.NetworkId,
					&keyAddress.Ip,
					&keyAddress.ActivateTime,
				))
			}
		})
	})
	return keyAddress
}

// GetNetworkExtenderActivations reads the activation history of one extender
// at or after `minActivateTime`, oldest first.
func GetNetworkExtenderActivations(
	ctx context.Context,
	extenderId server.Id,
	minActivateTime time.Time,
) []*NetworkExtenderActivationRecord {
	activations := []*NetworkExtenderActivationRecord{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				activation_id,
				activate_time,
				ip_version,
				client_address_hash,
				country_code,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id,
				accuracy_km
			FROM network_extender_activation
			WHERE extender_id = $1 AND $2 <= activate_time
			ORDER BY activate_time ASC
			`,
			extenderId,
			minActivateTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				activation := &NetworkExtenderActivationRecord{ExtenderId: extenderId}
				server.Raise(result.Scan(
					&activation.ActivationId,
					&activation.ActivateTime,
					&activation.IpVersion,
					&activation.ClientAddressHash,
					&activation.CountryCode,
					&activation.LocationId,
					&activation.CityLocationId,
					&activation.RegionLocationId,
					&activation.CountryLocationId,
					&activation.AccuracyKm,
				))
				activations = append(activations, activation)
			}
		})
	})
	return activations
}

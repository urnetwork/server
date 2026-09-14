package model

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The operator side of the extender directory (connect/EXTENDER.md C1 to C4).
//
// Three tables hold everything the operator knows about an extender:
//
//   - network_extender is one activated identity key, its ports and the
//     country it was last activated from. It is active while at least one of
//     its addresses is; revoke_time is set when the last one is lost.
//   - network_extender_address is one family the extender was probed on. The
//     uptime probes write only here until an address runs out of attempts.
//   - network_extender_publish is the outbound queue of signed records and
//     revocations. Nothing here publishes; the gossip service drains it
//     (C6, phase 5b).
//
// Signing lives in the caller, not here: this package holds no keys. A write
// that must publish takes a signer callback which runs inside the writing
// transaction, so what is signed is exactly the row set that was stored and
// the two can never disagree. A signer may run more than once, since the
// transaction may be retried.
//
// Timestamps are naive columns holding utc, as everywhere else in this schema.

// Kinds of a publish row. The message is a serialized
// protocol.ExtenderGossipMessage either way.
const (
	NetworkExtenderPublishKindRecord     = 1
	NetworkExtenderPublishKindRevocation = 2
)

// One activated extender identity.
type NetworkExtender struct {
	ExtenderId      server.Id
	NetworkId       server.Id
	ClientId        server.Id
	PublicKey       []byte
	CreateTime      time.Time
	TcpPort         int
	UdpPort         int
	DnsPort         int
	DnsTld          string
	CountryCode     string
	Active          bool
	RevokeTime      *time.Time
	RecordIssueTime *time.Time
	// The location the extender was last activated from (M1), resolved from
	// the activating address. Null until the extender's first activation after
	// the columns existed, and the city and the region are null for a
	// country-only lookup.
	LocationId        *server.Id
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
}

// One family an extender was activated and is probed on. DnsPorts are the dns
// carrier ports this address answered on (L2), ascending; empty when the dns
// carrier was not offered.
type NetworkExtenderAddress struct {
	ExtenderId               server.Id
	IpVersion                int
	Ip                       netip.Addr
	Carriers                 []string
	DnsPorts                 []int
	ActivateTime             time.Time
	LastProbeTime            *time.Time
	LastProbeSuccessTime     *time.Time
	ConsecutiveProbeFailures int
	Active                   bool
	LastPublishTime          *time.Time
}

// One queued gossip message.
type NetworkExtenderPublish struct {
	PublishId     server.Id
	ExtenderId    server.Id
	Kind          int
	Message       []byte
	CreateTime    time.Time
	PublishedTime *time.Time
}

// An extender together with the addresses a caller asked about, which is
// always the active ones unless a Testing_ reader says otherwise.
type NetworkExtenderWithAddresses struct {
	Extender  *NetworkExtender
	Addresses []*NetworkExtenderAddress
}

// The extender fields one uptime probe needs, flattened per address so the
// task can spread addresses over its workers without regrouping (C3).
type NetworkExtenderProbeTarget struct {
	ExtenderId server.Id
	PublicKey  []byte
	TcpPort    int
	DnsTld     string
	IpVersion  int
	Ip         netip.Addr
	Carriers   []string
}

// One address the geo dns sets are sampled from (C5), flattened the same way
// as a probe target: the family decides which record type an address can
// appear in, and the country decides which continent set it is local to.
type NetworkExtenderDnsAddress struct {
	ExtenderId  server.Id
	IpVersion   int
	Ip          netip.Addr
	CountryCode string
}

// Builds the serialized gossip message of a record. issueTime is the time the
// row set was written, which the body must carry so a record is never newer
// than the state it describes.
type NetworkExtenderRecordSigner func(
	extender *NetworkExtender,
	addresses []*NetworkExtenderAddress,
	issueTime time.Time,
) ([]byte, error)

// Builds the serialized gossip message of a revocation.
type NetworkExtenderRevocationSigner func(
	extender *NetworkExtender,
	issueTime time.Time,
) ([]byte, error)

// What one activation asks to be stored (C2). The caller has already probed
// the address, so nothing here is checked again.
type NetworkExtenderActivation struct {
	NetworkId   server.Id
	ClientId    server.Id
	PublicKey   []byte
	TcpPort     int
	UdpPort     int
	DnsPort     int
	DnsTld      string
	CountryCode string
	IpVersion   int
	Ip          netip.Addr
	Carriers    []string
	// the dns ports that passed their probe on this address (L2)
	DnsPorts []int
	// the location the activating address resolved to (M1), already created in
	// the location table by the caller. Each field is nil when the lookup did
	// not reach that granularity; all four nil is a lookup that failed, which
	// must still store the activation.
	LocationId        *server.Id
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
}

// WithLocation stores the location the activating address resolved to (M1),
// which the caller has already passed through CreateLocation so every id it
// carries names a row. A nil location, and any level the lookup did not reach,
// leaves the matching id null: the four ids are a refinement of the country
// code, never a precondition of storing the activation.
//
// CreateLocation degrades a location it cannot name all the way to the country
// (an unnamed city resolves at region granularity, an unnamed region at
// country granularity) and leaves the finer ids zero when it does, so the zero
// id, not the location type, is what decides which columns are written.
func (self *NetworkExtenderActivation) WithLocation(
	location *Location,
) *NetworkExtenderActivation {
	if location == nil {
		return self
	}
	id := func(locationId server.Id) *server.Id {
		if locationId == (server.Id{}) {
			return nil
		}
		return &locationId
	}
	self.LocationId = id(location.LocationId)
	self.CityLocationId = id(location.CityLocationId)
	self.RegionLocationId = id(location.RegionLocationId)
	self.CountryLocationId = id(location.CountryLocationId)
	return self
}

// The wire form of a carrier list, which the column holds as one
// comma-separated value. Blanks are dropped and the caller's order is kept,
// since it is the order the extender reported its carriers in.
func joinExtenderCarriers(carriers []string) string {
	kept := []string{}
	for _, carrier := range carriers {
		carrier = strings.TrimSpace(carrier)
		if carrier == "" || slices.Contains(kept, carrier) {
			continue
		}
		kept = append(kept, carrier)
	}
	return strings.Join(kept, ",")
}

// The inverse, tolerant of the empty column an address with no carrier would
// have.
func splitExtenderCarriers(carriers string) []string {
	kept := []string{}
	for _, carrier := range strings.Split(carriers, ",") {
		if carrier = strings.TrimSpace(carrier); carrier != "" {
			kept = append(kept, carrier)
		}
	}
	return kept
}

// The wire form of a dns port list, which the column holds as one
// comma-separated value. Unlike the carriers, the order is not the caller's:
// the ports are stored ascending, because that is the order a client dials
// them in (L2) and the record lists them in.
func joinExtenderDnsPorts(dnsPorts []int) string {
	kept := []int{}
	for _, dnsPort := range dnsPorts {
		if dnsPort < 1 || 65535 < dnsPort || slices.Contains(kept, dnsPort) {
			continue
		}
		kept = append(kept, dnsPort)
	}
	slices.Sort(kept)
	ports := []string{}
	for _, dnsPort := range kept {
		ports = append(ports, strconv.Itoa(dnsPort))
	}
	return strings.Join(ports, ",")
}

// The inverse, tolerant of the empty column an address activated without the
// dns carrier -- or before the column existed -- has. A value that is not a
// port is dropped rather than raised: the column is data the operator wrote,
// and a record is better short one port than not signed at all.
func splitExtenderDnsPorts(dnsPorts string) []int {
	kept := []int{}
	for _, dnsPortString := range strings.Split(dnsPorts, ",") {
		dnsPort, err := strconv.Atoi(strings.TrimSpace(dnsPortString))
		if err != nil || dnsPort < 1 || 65535 < dnsPort || slices.Contains(kept, dnsPort) {
			continue
		}
		kept = append(kept, dnsPort)
	}
	slices.Sort(kept)
	return kept
}

// Reads the active addresses of one extender inside an open transaction,
// ordered by family so a signed record has a stable address order.
//
// The rows are locked, and that is load bearing rather than defensive. Two
// probe workers of one tick can deactivate the two families of one extender at
// the same instant; each transaction takes its snapshot before the other
// commits, so an unlocked read would show each of them the OTHER family still
// active and neither would revoke -- leaving an extender with no address
// marked active and no revocation ever published. Locking makes the second
// transaction fail to serialize and run again on a snapshot that includes the
// first, which is the only reading that can decide this correctly.
func getActiveNetworkExtenderAddressesInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
) []*NetworkExtenderAddress {
	addresses := []*NetworkExtenderAddress{}
	result, err := tx.Query(
		ctx,
		`
		SELECT
			ip_version,
			ip,
			carriers,
			dns_ports,
			activate_time,
			last_probe_time,
			last_probe_success_time,
			consecutive_probe_failures,
			last_publish_time
		FROM network_extender_address
		WHERE extender_id = $1 AND active
		ORDER BY ip_version
		FOR UPDATE
		`,
		extenderId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			address := &NetworkExtenderAddress{
				ExtenderId: extenderId,
				Active:     true,
			}
			var carriers string
			var dnsPorts string
			server.Raise(result.Scan(
				&address.IpVersion,
				&address.Ip,
				&carriers,
				&dnsPorts,
				&address.ActivateTime,
				&address.LastProbeTime,
				&address.LastProbeSuccessTime,
				&address.ConsecutiveProbeFailures,
				&address.LastPublishTime,
			))
			address.Carriers = splitExtenderCarriers(carriers)
			address.DnsPorts = splitExtenderDnsPorts(dnsPorts)
			addresses = append(addresses, address)
		}
	})
	return addresses
}

// Reads one extender inside an open transaction, or nil when it is gone.
func getNetworkExtenderInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
) *NetworkExtender {
	var extender *NetworkExtender
	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_id,
			client_id,
			public_key,
			create_time,
			tcp_port,
			udp_port,
			dns_port,
			dns_tld,
			country_code,
			active,
			revoke_time,
			record_issue_time
		FROM network_extender
		WHERE extender_id = $1
		FOR UPDATE
		`,
		extenderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			e := &NetworkExtender{ExtenderId: extenderId}
			server.Raise(result.Scan(
				&e.NetworkId,
				&e.ClientId,
				&e.PublicKey,
				&e.CreateTime,
				&e.TcpPort,
				&e.UdpPort,
				&e.DnsPort,
				&e.DnsTld,
				&e.CountryCode,
				&e.Active,
				&e.RevokeTime,
				&e.RecordIssueTime,
			))
			extender = e
		}
	})
	return extender
}

// Inserts one publish row inside an open transaction.
func insertNetworkExtenderPublishInTx(
	ctx context.Context,
	tx server.PgTx,
	extenderId server.Id,
	kind int,
	message []byte,
	createTime time.Time,
) server.Id {
	publishId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_extender_publish (
			publish_id,
			extender_id,
			kind,
			message,
			create_time
		)
		VALUES ($1, $2, $3, $4, $5)
		`,
		publishId,
		extenderId,
		kind,
		message,
		createTime.UTC(),
	))
	return publishId
}

// ActivateNetworkExtender stores one activation and publishes the record that
// describes it (C2).
//
// The extender row is keyed by its identity key, so re-activating the same key
// from another family adds the second address to the same extender rather than
// creating a second one, and the record signed here therefore lists both. A
// re-activation also clears revoke_time and raises record_issue_time, which is
// what makes the new record win over any revocation the directory already has
// (B5).
//
// Everything is one transaction: the upserts, the signature over what they
// wrote, and the publish row. A caller that signed outside it could publish an
// address set that was never stored.
func ActivateNetworkExtender(
	ctx context.Context,
	activation *NetworkExtenderActivation,
	signRecord NetworkExtenderRecordSigner,
) *NetworkExtenderWithAddresses {
	var activated *NetworkExtenderWithAddresses

	server.Tx(ctx, func(tx server.PgTx) {
		activated = nil
		issueTime := server.NowUtc()

		var extenderId server.Id
		var createTime time.Time
		result, err := tx.Query(
			ctx,
			`
			INSERT INTO network_extender (
				extender_id,
				network_id,
				client_id,
				public_key,
				create_time,
				tcp_port,
				udp_port,
				dns_port,
				dns_tld,
				country_code,
				active,
				revoke_time,
				record_issue_time,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, true, NULL, $11, $12, $13, $14, $15)
			ON CONFLICT (public_key) DO UPDATE
			SET
				network_id = $2,
				client_id = $3,
				tcp_port = $6,
				udp_port = $7,
				dns_port = $8,
				dns_tld = $9,
				country_code = $10,
				active = true,
				revoke_time = NULL,
				record_issue_time = $11,
				location_id = $12,
				city_location_id = $13,
				region_location_id = $14,
				country_location_id = $15
			RETURNING extender_id, create_time
			`,
			server.NewId(),
			activation.NetworkId,
			activation.ClientId,
			activation.PublicKey,
			issueTime,
			activation.TcpPort,
			activation.UdpPort,
			activation.DnsPort,
			activation.DnsTld,
			activation.CountryCode,
			issueTime,
			activation.LocationId,
			activation.CityLocationId,
			activation.RegionLocationId,
			activation.CountryLocationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&extenderId, &createTime))
			}
		})

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_address (
				extender_id,
				ip_version,
				ip,
				carriers,
				dns_ports,
				activate_time,
				consecutive_probe_failures,
				active
			)
			VALUES ($1, $2, $3, $4, $5, $6, 0, true)
			ON CONFLICT (extender_id, ip_version) DO UPDATE
			SET
				ip = $3,
				carriers = $4,
				dns_ports = $5,
				activate_time = $6,
				consecutive_probe_failures = 0,
				active = true
			`,
			extenderId,
			activation.IpVersion,
			activation.Ip,
			joinExtenderCarriers(activation.Carriers),
			joinExtenderDnsPorts(activation.DnsPorts),
			issueTime,
		))

		extender := &NetworkExtender{
			ExtenderId:      extenderId,
			NetworkId:       activation.NetworkId,
			ClientId:        activation.ClientId,
			PublicKey:       activation.PublicKey,
			CreateTime:      createTime,
			TcpPort:         activation.TcpPort,
			UdpPort:         activation.UdpPort,
			DnsPort:         activation.DnsPort,
			DnsTld:          activation.DnsTld,
			CountryCode:     activation.CountryCode,
			Active:          true,
			RecordIssueTime: &issueTime,
			// the four ids this activation just wrote; the last activation of
			// either family wins, so an extender's location is always the one
			// its most recent address resolved to (M1)
			LocationId:        activation.LocationId,
			CityLocationId:    activation.CityLocationId,
			RegionLocationId:  activation.RegionLocationId,
			CountryLocationId: activation.CountryLocationId,
		}
		addresses := getActiveNetworkExtenderAddressesInTx(ctx, tx, extenderId)

		message, err := signRecord(extender, addresses, issueTime)
		server.Raise(err)
		insertNetworkExtenderPublishInTx(
			ctx,
			tx,
			extenderId,
			NetworkExtenderPublishKindRecord,
			message,
			issueTime,
		)

		activated = &NetworkExtenderWithAddresses{
			Extender:  extender,
			Addresses: addresses,
		}
	})

	return activated
}

// Testing_NetworkExtenderProbeBarrier, when set, runs inside the probe result
// transaction after the address has been deactivated and before the remaining
// addresses are read.
//
// It exists so a test can force the one interleaving that decides whether an
// extender is revoked: another worker deactivating the sibling family and
// committing while this transaction is already open. Test only, and never set
// in production.
var Testing_NetworkExtenderProbeBarrier func()

// What one probe result changed, so the task can log it and a test can assert
// it without re-reading the rows.
type NetworkExtenderProbeOutcome struct {
	AddressDeactivated bool
	ExtenderRevoked    bool
}

// RecordNetworkExtenderProbeResult applies one uptime probe result (C3).
//
// A success resets the failure counter, which is what makes the budget
// consecutive rather than cumulative: an extender that flaps is not eventually
// removed for the sum of its bad minutes. A failure spends one attempt, and
// the address is deactivated on the attempt that reaches
// maxConsecutiveProbeFailures. Only a new activation re-enables it, so a dead
// address costs the probe task nothing after that.
//
// Losing the last active address revokes the extender: the revocation is
// signed and queued in the same transaction that deactivates the address, so
// the directory can never learn of an extender that was removed here without
// also learning why.
//
// A result for an address that is already inactive, or for an extender that is
// gone, changes nothing.
func RecordNetworkExtenderProbeResult(
	ctx context.Context,
	extenderId server.Id,
	ipVersion int,
	success bool,
	probeTime time.Time,
	maxConsecutiveProbeFailures int,
	signRevocation NetworkExtenderRevocationSigner,
) *NetworkExtenderProbeOutcome {
	outcome := &NetworkExtenderProbeOutcome{}

	server.Tx(ctx, func(tx server.PgTx) {
		*outcome = NetworkExtenderProbeOutcome{}

		if success {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_extender_address
				SET
					consecutive_probe_failures = 0,
					last_probe_time = $3,
					last_probe_success_time = $3
				WHERE extender_id = $1 AND ip_version = $2 AND active
				`,
				extenderId,
				ipVersion,
				probeTime.UTC(),
			))
			return
		}

		var consecutiveProbeFailures int
		found := false
		result, err := tx.Query(
			ctx,
			`
			UPDATE network_extender_address
			SET
				consecutive_probe_failures = consecutive_probe_failures + 1,
				last_probe_time = $3
			WHERE extender_id = $1 AND ip_version = $2 AND active
			RETURNING consecutive_probe_failures
			`,
			extenderId,
			ipVersion,
			probeTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&consecutiveProbeFailures))
				found = true
			}
		})
		if !found || consecutiveProbeFailures < maxConsecutiveProbeFailures {
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_address
			SET active = false
			WHERE extender_id = $1 AND ip_version = $2
			`,
			extenderId,
			ipVersion,
		))
		outcome.AddressDeactivated = true

		if Testing_NetworkExtenderProbeBarrier != nil {
			Testing_NetworkExtenderProbeBarrier()
		}
		if 0 < len(getActiveNetworkExtenderAddressesInTx(ctx, tx, extenderId)) {
			return
		}
		extender := getNetworkExtenderInTx(ctx, tx, extenderId)
		if extender == nil || !extender.Active {
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET active = false, revoke_time = $2
			WHERE extender_id = $1
			`,
			extenderId,
			probeTime.UTC(),
		))
		message, err := signRevocation(extender, probeTime)
		server.Raise(err)
		insertNetworkExtenderPublishInTx(
			ctx,
			tx,
			extenderId,
			NetworkExtenderPublishKindRevocation,
			message,
			probeTime,
		)
		outcome.ExtenderRevoked = true
	})

	return outcome
}

// PublishNetworkExtenderRecord signs and queues a fresh record for one
// extender and stamps its active addresses (C4).
//
// The stamp is what moves the extender to the back of the drip order, so it
// belongs in the same transaction as the record it accounts for: a stamp
// without a record would skip the extender for a whole rotation, and a record
// without a stamp would republish it every tick.
//
// Returns false when the extender went inactive or lost its last address
// between being selected and being published, which is expected under a
// concurrent probe tick and is not an error.
func PublishNetworkExtenderRecord(
	ctx context.Context,
	extenderId server.Id,
	signRecord NetworkExtenderRecordSigner,
) bool {
	published := false

	server.Tx(ctx, func(tx server.PgTx) {
		published = false

		extender := getNetworkExtenderInTx(ctx, tx, extenderId)
		if extender == nil || !extender.Active {
			return
		}
		addresses := getActiveNetworkExtenderAddressesInTx(ctx, tx, extenderId)
		if len(addresses) == 0 {
			return
		}

		issueTime := server.NowUtc()
		message, err := signRecord(extender, addresses, issueTime)
		server.Raise(err)
		insertNetworkExtenderPublishInTx(
			ctx,
			tx,
			extenderId,
			NetworkExtenderPublishKindRecord,
			message,
			issueTime,
		)

		// record_issue_time tracks the newest record of this extender, which
		// a republish is as much as an activation is
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET record_issue_time = $2
			WHERE extender_id = $1
			`,
			extenderId,
			issueTime,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_address
			SET last_publish_time = $2
			WHERE extender_id = $1 AND active
			`,
			extenderId,
			issueTime,
		))
		published = true
	})

	return published
}

// GetActiveNetworkExtenderProbeTargets reads every address the uptime task
// probes (C3), oldest probe first so a run that is cut short still makes
// progress through the set rather than re-probing the same head.
func GetActiveNetworkExtenderProbeTargets(ctx context.Context) []*NetworkExtenderProbeTarget {
	targets := []*NetworkExtenderProbeTarget{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.extender_id,
				network_extender.public_key,
				network_extender.tcp_port,
				network_extender.dns_tld,
				network_extender_address.ip_version,
				network_extender_address.ip,
				network_extender_address.carriers
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.active
			ORDER BY network_extender_address.last_probe_time ASC NULLS FIRST
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				target := &NetworkExtenderProbeTarget{}
				var carriers string
				server.Raise(result.Scan(
					&target.ExtenderId,
					&target.PublicKey,
					&target.TcpPort,
					&target.DnsTld,
					&target.IpVersion,
					&target.Ip,
					&carriers,
				))
				target.Carriers = splitExtenderCarriers(carriers)
				targets = append(targets, target)
			}
		})
	})

	return targets
}

// GetActiveNetworkExtenderDnsAddresses reads every address the geo dns sets
// may hold (C5): one row per active address of an active extender, with the
// country the extender activated from.
//
// The order is stable rather than random. The sampler does its own shuffling
// from a seeded source, so a random order here would only make one tick's
// sets impossible to reproduce for the same reason the sampler is seeded.
func GetActiveNetworkExtenderDnsAddresses(ctx context.Context) []*NetworkExtenderDnsAddress {
	addresses := []*NetworkExtenderDnsAddress{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.extender_id,
				network_extender.country_code,
				network_extender_address.ip_version,
				network_extender_address.ip
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.active
			ORDER BY network_extender.extender_id, network_extender_address.ip_version
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				address := &NetworkExtenderDnsAddress{}
				server.Raise(result.Scan(
					&address.ExtenderId,
					&address.CountryCode,
					&address.IpVersion,
					&address.Ip,
				))
				addresses = append(addresses, address)
			}
		})
	})

	return addresses
}

// CountActiveNetworkExtenders counts extenders with at least one active
// address, which is the population the drip has to rotate through (C4).
func CountActiveNetworkExtenders(ctx context.Context) int {
	count := 0

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(DISTINCT network_extender.extender_id)
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.active
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})

	return count
}

// GetNetworkExtenderIdsForPublish selects the next drip batch: the active
// extenders whose records are oldest (C4).
//
// The order key is the oldest publish stamp over the extender's active
// addresses, with never-published sorting first. A null cannot be folded in by
// MIN, which ignores nulls and would rank an extender with one fresh address
// and one never-published address by the fresh one, so the null is mapped to
// the earliest representable time instead.
func GetNetworkExtenderIdsForPublish(ctx context.Context, limit int) []server.Id {
	extenderIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_extender.extender_id
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.active
			GROUP BY network_extender.extender_id
			ORDER BY MIN(COALESCE(
				network_extender_address.last_publish_time,
				TIMESTAMP '-infinity'
			)) ASC
			LIMIT $1
			`,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extenderId server.Id
				server.Raise(result.Scan(&extenderId))
				extenderIds = append(extenderIds, extenderId)
			}
		})
	})

	return extenderIds
}

// GetRandomActiveNetworkExtenders samples active extenders with their active
// addresses, which is what an activation answers with as bootstrap (C2).
//
// excludeExtenderId leaves one extender out, normally the one being activated,
// which has no use for its own record in the sample. The zero id excludes
// nothing.
func GetRandomActiveNetworkExtenders(
	ctx context.Context,
	limit int,
	excludeExtenderId server.Id,
) []*NetworkExtenderWithAddresses {
	extenders := []*NetworkExtenderWithAddresses{}

	server.Db(ctx, func(conn server.PgConn) {
		extenderWithAddresses := map[server.Id]*NetworkExtenderWithAddresses{}
		extenderIds := []server.Id{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.extender_id,
				network_extender.network_id,
				network_extender.client_id,
				network_extender.public_key,
				network_extender.create_time,
				network_extender.tcp_port,
				network_extender.udp_port,
				network_extender.dns_port,
				network_extender.dns_tld,
				network_extender.country_code,
				network_extender.record_issue_time,
				network_extender_address.ip_version,
				network_extender_address.ip,
				network_extender_address.carriers,
				network_extender_address.dns_ports,
				network_extender_address.activate_time,
				network_extender_address.last_publish_time
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.extender_id IN (
				SELECT sampled.extender_id
				FROM network_extender sampled
				INNER JOIN network_extender_address sampled_address ON
					sampled_address.extender_id = sampled.extender_id AND
					sampled_address.active
				WHERE sampled.active AND sampled.extender_id != $2
				GROUP BY sampled.extender_id
				ORDER BY random()
				LIMIT $1
			)
			ORDER BY network_extender.extender_id, network_extender_address.ip_version
			`,
			limit,
			excludeExtenderId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				extender := &NetworkExtender{Active: true}
				address := &NetworkExtenderAddress{Active: true}
				var carriers string
				var dnsPorts string
				server.Raise(result.Scan(
					&extender.ExtenderId,
					&extender.NetworkId,
					&extender.ClientId,
					&extender.PublicKey,
					&extender.CreateTime,
					&extender.TcpPort,
					&extender.UdpPort,
					&extender.DnsPort,
					&extender.DnsTld,
					&extender.CountryCode,
					&extender.RecordIssueTime,
					&address.IpVersion,
					&address.Ip,
					&carriers,
					&dnsPorts,
					&address.ActivateTime,
					&address.LastPublishTime,
				))
				address.ExtenderId = extender.ExtenderId
				address.Carriers = splitExtenderCarriers(carriers)
				address.DnsPorts = splitExtenderDnsPorts(dnsPorts)

				entry, ok := extenderWithAddresses[extender.ExtenderId]
				if !ok {
					entry = &NetworkExtenderWithAddresses{
						Extender:  extender,
						Addresses: []*NetworkExtenderAddress{},
					}
					extenderWithAddresses[extender.ExtenderId] = entry
					extenderIds = append(extenderIds, extender.ExtenderId)
				}
				entry.Addresses = append(entry.Addresses, address)
			}
		})

		for _, extenderId := range extenderIds {
			extenders = append(extenders, extenderWithAddresses[extenderId])
		}
	})

	return extenders
}

// ClaimUnpublishedExtenderPublishes takes the head of the outbound queue for
// the gossip service (C6, phase 5b).
//
// SKIP LOCKED is what lets a second replica run at all: it takes the next
// rows rather than blocking on the first replica's, so two services drain the
// queue together instead of one waiting behind the other. The lock lasts only
// as long as this transaction, so a caller that crashes before stamping leaves
// the rows to be claimed again -- publishing twice is harmless, since gossip
// is idempotent per message, while losing a record is not.
func ClaimUnpublishedExtenderPublishes(ctx context.Context, limit int) []*NetworkExtenderPublish {
	publishes := []*NetworkExtenderPublish{}

	server.Tx(ctx, func(tx server.PgTx) {
		publishes = []*NetworkExtenderPublish{}

		result, err := tx.Query(
			ctx,
			`
			SELECT
				publish_id,
				extender_id,
				kind,
				message,
				create_time
			FROM network_extender_publish
			WHERE published_time IS NULL
			ORDER BY create_time ASC, publish_id ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
			`,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				publish := &NetworkExtenderPublish{}
				server.Raise(result.Scan(
					&publish.PublishId,
					&publish.ExtenderId,
					&publish.Kind,
					&publish.Message,
					&publish.CreateTime,
				))
				publishes = append(publishes, publish)
			}
		})
	})

	return publishes
}

// MarkExtenderPublishPublished stamps one queue row as delivered. A row that
// is already stamped is left alone, so a redelivery after a crash does not
// move the time forward.
func MarkExtenderPublishPublished(ctx context.Context, publishId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_publish
			SET published_time = $2
			WHERE publish_id = $1 AND published_time IS NULL
			`,
			publishId,
			server.NowUtc(),
		))
	})
}

// Testing_CreateNetworkExtender writes one extender and its addresses exactly
// as given, so a test can start from a state the normal writes would take
// several ticks to reach.
func Testing_CreateNetworkExtender(
	ctx context.Context,
	extender *NetworkExtender,
	addresses []*NetworkExtenderAddress,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender (
				extender_id,
				network_id,
				client_id,
				public_key,
				create_time,
				tcp_port,
				udp_port,
				dns_port,
				dns_tld,
				country_code,
				active,
				revoke_time,
				record_issue_time,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			`,
			extender.ExtenderId,
			extender.NetworkId,
			extender.ClientId,
			extender.PublicKey,
			extender.CreateTime.UTC(),
			extender.TcpPort,
			extender.UdpPort,
			extender.DnsPort,
			extender.DnsTld,
			extender.CountryCode,
			extender.Active,
			extender.RevokeTime,
			extender.RecordIssueTime,
			extender.LocationId,
			extender.CityLocationId,
			extender.RegionLocationId,
			extender.CountryLocationId,
		))
		for _, address := range addresses {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_address (
					extender_id,
					ip_version,
					ip,
					carriers,
					dns_ports,
					activate_time,
					last_probe_time,
					last_probe_success_time,
					consecutive_probe_failures,
					active,
					last_publish_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				`,
				extender.ExtenderId,
				address.IpVersion,
				address.Ip,
				joinExtenderCarriers(address.Carriers),
				joinExtenderDnsPorts(address.DnsPorts),
				address.ActivateTime.UTC(),
				address.LastProbeTime,
				address.LastProbeSuccessTime,
				address.ConsecutiveProbeFailures,
				address.Active,
				address.LastPublishTime,
			))
		}
	})
}

// Testing_CreateNetworkExtenderPopulation bulk-creates count active extenders,
// each with one never-published active v6 address, in one statement.
//
// The drip's batch only grows past its floor above eight thousand active
// extenders, so a test of that boundary needs a population no row-at-a-time
// helper could build in reasonable time. Keys and addresses are derived from
// the series index, so they are unique, synthetic and reproducible.
func Testing_CreateNetworkExtenderPopulation(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	count int,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			WITH created AS (
				INSERT INTO network_extender (
					extender_id,
					network_id,
					client_id,
					public_key,
					create_time,
					active
				)
				SELECT
					gen_random_uuid(),
					$1,
					$2,
					sha256(('extender-population-' || i::text)::bytea),
					$3,
					true
				FROM generate_series(1, $4) AS i
				RETURNING extender_id
			)
			INSERT INTO network_extender_address (
				extender_id,
				ip_version,
				ip,
				carriers,
				activate_time,
				active
			)
			SELECT
				created.extender_id,
				6,
				('2001:db8::'::inet + row_number() OVER (ORDER BY created.extender_id))::inet,
				'tcp',
				$3,
				true
			FROM created
			`,
			networkId,
			clientId,
			server.NowUtc(),
			count,
		))
	})
}

// Testing_GetNetworkExtender reads one extender with every address it has,
// active or not, which is what an assertion about a deactivated address needs.
func Testing_GetNetworkExtender(
	ctx context.Context,
	extenderId server.Id,
) *NetworkExtenderWithAddresses {
	var entry *NetworkExtenderWithAddresses

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_id,
				client_id,
				public_key,
				create_time,
				tcp_port,
				udp_port,
				dns_port,
				dns_tld,
				country_code,
				active,
				revoke_time,
				record_issue_time,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id
			FROM network_extender
			WHERE extender_id = $1
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				extender := &NetworkExtender{ExtenderId: extenderId}
				server.Raise(result.Scan(
					&extender.NetworkId,
					&extender.ClientId,
					&extender.PublicKey,
					&extender.CreateTime,
					&extender.TcpPort,
					&extender.UdpPort,
					&extender.DnsPort,
					&extender.DnsTld,
					&extender.CountryCode,
					&extender.Active,
					&extender.RevokeTime,
					&extender.RecordIssueTime,
					&extender.LocationId,
					&extender.CityLocationId,
					&extender.RegionLocationId,
					&extender.CountryLocationId,
				))
				entry = &NetworkExtenderWithAddresses{
					Extender:  extender,
					Addresses: []*NetworkExtenderAddress{},
				}
			}
		})
		if entry == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
			SELECT
				ip_version,
				ip,
				carriers,
				dns_ports,
				activate_time,
				last_probe_time,
				last_probe_success_time,
				consecutive_probe_failures,
				active,
				last_publish_time
			FROM network_extender_address
			WHERE extender_id = $1
			ORDER BY ip_version
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				address := &NetworkExtenderAddress{ExtenderId: extenderId}
				var carriers string
				var dnsPorts string
				server.Raise(result.Scan(
					&address.IpVersion,
					&address.Ip,
					&carriers,
					&dnsPorts,
					&address.ActivateTime,
					&address.LastProbeTime,
					&address.LastProbeSuccessTime,
					&address.ConsecutiveProbeFailures,
					&address.Active,
					&address.LastPublishTime,
				))
				address.Carriers = splitExtenderCarriers(carriers)
				address.DnsPorts = splitExtenderDnsPorts(dnsPorts)
				entry.Addresses = append(entry.Addresses, address)
			}
		})
	})

	return entry
}

// Testing_DeactivateNetworkExtenderAddress deactivates one address directly,
// which is what another probe worker that has just spent the last of its
// budget leaves behind.
func Testing_DeactivateNetworkExtenderAddress(
	ctx context.Context,
	extenderId server.Id,
	ipVersion int,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_address
			SET active = false
			WHERE extender_id = $1 AND ip_version = $2
			`,
			extenderId,
			ipVersion,
		))
	})
}

// Testing_GetNetworkExtenderPublishes reads the whole outbound queue, oldest
// first, stamped rows included.
func Testing_GetNetworkExtenderPublishes(ctx context.Context) []*NetworkExtenderPublish {
	publishes := []*NetworkExtenderPublish{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				publish_id,
				extender_id,
				kind,
				message,
				create_time,
				published_time
			FROM network_extender_publish
			ORDER BY create_time ASC, publish_id ASC
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				publish := &NetworkExtenderPublish{}
				server.Raise(result.Scan(
					&publish.PublishId,
					&publish.ExtenderId,
					&publish.Kind,
					&publish.Message,
					&publish.CreateTime,
					&publish.PublishedTime,
				))
				publishes = append(publishes, publish)
			}
		})
	})

	return publishes
}

// Testing_HoldExtenderPublishLock locks the given queue rows and runs hold
// while the lock is held, then releases it by failing the transaction.
//
// This is how a test observes SKIP LOCKED deterministically: a concurrent
// claim made from inside hold must skip exactly these rows, with no timing
// assumption about two racing claims.
func Testing_HoldExtenderPublishLock(
	ctx context.Context,
	publishIds []server.Id,
	hold func(),
) {
	held := fmt.Errorf("testing_hold_extender_publish_lock")
	func() {
		defer func() {
			if err := recover(); err != nil && err != held {
				panic(err)
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, publishId := range publishIds {
				result, err := tx.Query(
					ctx,
					`
					SELECT publish_id
					FROM network_extender_publish
					WHERE publish_id = $1
					FOR UPDATE
					`,
					publishId,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
					}
				})
			}
			hold()
			// roll the lock back rather than commit, so the queue is exactly
			// as the test left it
			panic(held)
		})
	}()
}

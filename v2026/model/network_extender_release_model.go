package model

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The gated tier's tables (connect/EXTENDER.md R3, R4): the durable fleet a
// release draws from, the ledger the release policy reads and writes, the
// block reports the per-country blocked state is derived from, and the
// operator's designations of a tier and a canary.
//
// The policy itself is connect's (connect.ExtenderReleasePolicy); this file
// is its database ledger and the loaders around it, so every replica of the
// api shares one count of requests, releases and reports. Nothing here is on
// a packet path.

// The gated tier with active addresses, which a release draws from (R3):
// every active extender signed into the gated tier, canaries included, since
// a gated canary is placed by its partition like any other gated record.
func GetActiveGatedNetworkExtenders(ctx context.Context) []*NetworkExtenderWithAddresses {
	return getActiveNetworkExtendersWithAddresses(ctx, connect.ExtenderDirectoryTierGated)
}

// Every active extender of one tier with its active addresses, in id order.
func getActiveNetworkExtendersWithAddresses(ctx context.Context, directoryTier int) []*NetworkExtenderWithAddresses {
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
				COALESCE(LOWER(derived_country.country_code::text), ''),
				network_extender.record_issue_time,
				network_extender.directory_tier,
				COALESCE(network_extender.canary_channel, ''),
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
			LEFT JOIN derived_location ON
				derived_location.node_kind = $2 AND
				derived_location.node_id = network_extender.extender_id
			LEFT JOIN location AS derived_country ON
				derived_country.location_id = derived_location.country_location_id
			WHERE network_extender.active AND network_extender.directory_tier = $1
				AND (network_extender.canary_channel IS NULL OR network_extender.canary_channel = 'gated')
			ORDER BY network_extender.extender_id, network_extender_address.ip_version
			`,
			directoryTier,
			DerivedLocationNodeKindExtender,
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
					&extender.DerivedCountryCode,
					&extender.RecordIssueTime,
					&extender.DirectoryTier,
					&extender.CanaryChannel,
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

// Moves one extender between tiers (R1), the operator's decision. The next
// record signed for it carries the tier; a gated extender's records already
// in client directories expire within a day and are never dripped again.
func SetNetworkExtenderDirectoryTier(ctx context.Context, extenderId server.Id, directoryTier int) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET directory_tier = $2
			WHERE extender_id = $1
			`,
			extenderId,
			directoryTier,
		))
	})
}

// Designates one extender a canary of one channel (R4), or none with the
// empty channel. The placement follows from the channel: a dns canary is in
// its continent's sets alone, a gated canary in its gated partition.
func SetNetworkExtenderCanaryChannel(ctx context.Context, extenderId server.Id, canaryChannel string) {
	server.Tx(ctx, func(tx server.PgTx) {
		var channel *string
		if canaryChannel = strings.TrimSpace(canaryChannel); canaryChannel != "" {
			channel = &canaryChannel
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET canary_channel = $2
			WHERE extender_id = $1
			`,
			extenderId,
			channel,
		))
	})
}

// The release ledger over the database (R3), one per request: the policy's
// counts of requests per identity and vantage and of distinct identities per
// record and country. Replicas serialize admission on identity, vantage and
// country locks. Counts and writes share one READ COMMITTED transaction.
type NetworkExtenderReleaseLedger struct {
	ctx         context.Context
	extenderIds map[string]server.Id
}

func NewNetworkExtenderReleaseLedger(ctx context.Context, extenderIds map[string]server.Id) *NetworkExtenderReleaseLedger {
	return &NetworkExtenderReleaseLedger{
		ctx:         ctx,
		extenderIds: extenderIds,
	}
}

// Locks shared budget keys in stable order to avoid deadlocks across replicas.
// Hash collisions only serialize unrelated requests; they never weaken a cap.
func (self *NetworkExtenderReleaseLedger) Transact(identity []byte, vantage string, countryCode string, _ []string, apply func(connect.ExtenderReleaseLedgerTx)) {
	lockIds := []int64{}
	addLock := func(namespace string, value string) {
		sum := sha256.Sum256([]byte("extender-release\x00" + namespace + "\x00" + value))
		lockIds = append(lockIds, int64(binary.BigEndian.Uint64(sum[:8])))
	}
	addLock("identity", hex.EncodeToString(identity))
	addLock("vantage", vantage)
	// Country-wide serialization keeps lock usage constant even for a large
	// partition. Releases are low-frequency control-plane work; unrelated
	// countries can still proceed concurrently.
	addLock("country", countryCode)
	slices.Sort(lockIds)
	lockIds = slices.Compact(lockIds)
	server.Tx(self.ctx, func(tx server.PgTx) {
		for _, lockId := range lockIds {
			server.RaisePgResult(tx.Exec(self.ctx, "SELECT pg_advisory_xact_lock($1)", lockId))
		}
		apply(&networkExtenderReleaseTx{NetworkExtenderReleaseLedger: self, tx: tx})
	}, server.TxReadCommitted)
}

// Transaction-local view; the parent context and key map are immutable.
type networkExtenderReleaseTx struct {
	*NetworkExtenderReleaseLedger
	tx server.PgTx
}

// Loads only the identity's bounded epoch disclosures for eligibility checks.
// Admission re-reads after acquiring its transaction locks.
func (self *NetworkExtenderReleaseLedger) IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error) {
	var keyHexes []string
	server.Tx(self.ctx, func(tx server.PgTx) {
		view := &networkExtenderReleaseTx{NetworkExtenderReleaseLedger: self, tx: tx}
		keyHexes, _ = view.IssuedKeyHexes(identity, epoch)
	}, server.TxReadCommitted)
	return keyHexes, nil
}

// Reads every prior disclosure in the epoch, independent of current eligibility.
func (self *networkExtenderReleaseTx) IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error) {
	keyHexes := []string{}
	result, err := self.tx.Query(self.ctx, `
		SELECT COALESCE(encode(network_extender.public_key, 'hex'), 'removed:' || network_extender_release.extender_id::varchar)
		FROM network_extender_release
		LEFT JOIN network_extender USING (extender_id)
		WHERE identity = $1 AND epoch = $2
		GROUP BY network_extender.public_key, network_extender_release.extender_id
		ORDER BY MIN(release_id::varchar)
	`, identity, int64(epoch))
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var keyHex string
			server.Raise(result.Scan(&keyHex))
			keyHexes = append(keyHexes, keyHex)
		}
	})
	return keyHexes, nil
}

// Counts rates after all shared admission locks have been acquired.
func (self *networkExtenderReleaseTx) RequestCounts(identity []byte, vantage string, since time.Time) (int, int) {
	identityCount := 0
	vantageCount := 0
	result, err := self.tx.Query(
		self.ctx,
		`
			SELECT
				(SELECT COUNT(*) FROM network_extender_release_request WHERE identity = $1 AND $3 <= request_time),
				(SELECT COUNT(*) FROM network_extender_release_request WHERE vantage = $2 AND $3 <= request_time)
			`,
		identity,
		vantage,
		since.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&identityCount, &vantageCount))
		}
	})
	return identityCount, vantageCount
}

// Refused requests commit in the same transaction as admitted requests.
func (self *networkExtenderReleaseTx) RecordRequest(identity []byte, vantage string, now time.Time) {
	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
			INSERT INTO network_extender_release_request (request_id, identity, vantage, request_time)
			VALUES ($1, $2, $3, $4)
			`,
		server.NewId(),
		identity,
		vantage,
		now.UTC(),
	))
}

// Country reservations are read while every competing replica is excluded.
func (self *networkExtenderReleaseTx) ClientCount(keyHex string, countryCode string, identity []byte, since time.Time) int {
	extenderId, ok := self.extenderIds[strings.ToLower(keyHex)]
	if !ok {
		panic("release ledger is missing an extender key")
	}
	count := 0
	result, err := self.tx.Query(
		self.ctx,
		`
			SELECT COUNT(DISTINCT identity)
			FROM network_extender_release
			WHERE
				extender_id = $1 AND
				country_code = $2 AND
				identity != $3 AND
				$4 <= release_time
			`,
		extenderId,
		countryCode,
		identity,
		since.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return count
}

// A disclosed record is reserved before any replica may inspect the budget.
func (self *networkExtenderReleaseTx) RecordRelease(identity []byte, keyHex string, countryCode string, epoch uint64, now time.Time) {
	extenderId, ok := self.extenderIds[strings.ToLower(keyHex)]
	if !ok {
		panic("release ledger is missing an extender key")
	}
	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
			INSERT INTO network_extender_release (release_id, extender_id, identity, country_code, epoch, release_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
		server.NewId(),
		extenderId,
		identity,
		countryCode,
		int64(epoch),
		now.UTC(),
	))
}

// Records that a client in a country could not reach an extender (R4).
func RecordNetworkExtenderBlockReport(
	ctx context.Context,
	extenderId server.Id,
	clientId server.Id,
	countryCode string,
	reportTime time.Time,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_block_report (report_id, extender_id, client_id, country_code, report_time)
			VALUES ($1, $2, $3, $4, $5)
			`,
			server.NewId(),
			extenderId,
			clientId,
			strings.ToLower(strings.TrimSpace(countryCode)),
			reportTime.UTC(),
		))
	})
}

// Whether an extender is blocked in a country at `now` (R4,
// connect.ExtenderBlockedState's rule over the tables): at least
// `reportThreshold` distinct clients there reported it since the report
// window began, and the operator's own probe reached one of its active
// addresses since the probe window began, so an extender that is simply down
// is not blocked.
func NetworkExtenderBlockedInCountry(
	ctx context.Context,
	extenderId server.Id,
	countryCode string,
	reportThreshold int,
	reportWindow time.Duration,
	probeWindow time.Duration,
	now time.Time,
) bool {
	blocked := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				(
					SELECT COUNT(DISTINCT client_id)
					FROM network_extender_block_report
					WHERE extender_id = $1 AND country_code = $2 AND $3 <= report_time
				),
				(
					SELECT COUNT(*)
					FROM network_extender_address
					WHERE extender_id = $1 AND active AND $4 <= last_probe_success_time
				)
			`,
			extenderId,
			strings.ToLower(strings.TrimSpace(countryCode)),
			now.Add(-reportWindow).UTC(),
			now.Add(-probeWindow).UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var reporterCount int
				var probedAddressCount int
				server.Raise(result.Scan(&reporterCount, &probedAddressCount))
				blocked = reportThreshold <= reporterCount && 0 < probedAddressCount
			}
		})
	})
	return blocked
}

// The blocked source a release reads (connect.ExtenderReleaseBlockedSource):
// NetworkExtenderBlockedInCountry by key, under the blocked-state defaults.
type NetworkExtenderBlockedSource struct {
	ctx         context.Context
	extenderIds map[string]server.Id
	settings    *connect.ExtenderBlockedStateSettings
}

func NewNetworkExtenderBlockedSource(
	ctx context.Context,
	extenderIds map[string]server.Id,
	settings *connect.ExtenderBlockedStateSettings,
) *NetworkExtenderBlockedSource {
	if settings == nil {
		settings = connect.DefaultExtenderBlockedStateSettings()
	}
	return &NetworkExtenderBlockedSource{
		ctx:         ctx,
		extenderIds: extenderIds,
		settings:    settings,
	}
}

func (self *NetworkExtenderBlockedSource) Blocked(keyHex string, countryCode string) bool {
	extenderId, ok := self.extenderIds[strings.ToLower(keyHex)]
	if !ok {
		return false
	}
	return NetworkExtenderBlockedInCountry(
		self.ctx,
		extenderId,
		countryCode,
		self.settings.ReportThreshold,
		self.settings.ReportWindow,
		self.settings.ProbeWindow,
		server.NowUtc(),
	)
}

// Loads one partition's blocked flags in one database round trip, outside the
// admission transaction. Unknown or removed records have no live blocked flag.
func (self *NetworkExtenderBlockedSource) BlockedKeys(keyHexes []string, countryCode string) map[string]bool {
	keyHexesById := map[server.Id]string{}
	extenderIds := []server.Id{}
	for _, keyHex := range keyHexes {
		if id, ok := self.extenderIds[strings.ToLower(keyHex)]; ok {
			if _, seen := keyHexesById[id]; !seen {
				extenderIds = append(extenderIds, id)
			}
			keyHexesById[id] = strings.ToLower(keyHex)
		}
	}
	blocked := map[string]bool{}
	if len(extenderIds) == 0 {
		return blocked
	}
	now := server.NowUtc()
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `
			SELECT candidate.extender_id
			FROM unnest($1::uuid[]) AS candidate(extender_id)
			WHERE (
				SELECT COUNT(DISTINCT client_id) FROM network_extender_block_report
				WHERE extender_id = candidate.extender_id AND country_code = $2 AND $4 <= report_time
			) >= $3 AND EXISTS (
				SELECT 1 FROM network_extender_address
				WHERE extender_id = candidate.extender_id AND active AND $5 <= last_probe_success_time
			)
		`, extenderIds, strings.ToLower(strings.TrimSpace(countryCode)), self.settings.ReportThreshold,
			now.Add(-self.settings.ReportWindow).UTC(), now.Add(-self.settings.ProbeWindow).UTC())
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				blocked[keyHexesById[id]] = true
			}
		})
	})
	return blocked
}

// Drops the release, request and report rows older than `minTime`, which a
// maintenance task runs past the longest window the policy reads (the client
// window, R3). Phased: the task that calls it is not wired yet.
func RemoveExpiredNetworkExtenderReleases(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_extender_release_request WHERE request_time < $1`,
			minTime.UTC(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_extender_release WHERE release_time < $1`,
			minTime.UTC(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_extender_block_report WHERE report_time < $1`,
			minTime.UTC(),
		))
	})
}

// The key map the ledger and the blocked source are built over: hex key to
// extender id, lower case.
func NetworkExtenderIdsByKeyHex(extenders []*NetworkExtenderWithAddresses) map[string]server.Id {
	extenderIds := map[string]server.Id{}
	for _, extender := range extenders {
		extenderIds[hex.EncodeToString(extender.Extender.PublicKey)] = extender.Extender.ExtenderId
	}
	return extenderIds
}

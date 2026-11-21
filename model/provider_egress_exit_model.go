package model

// A provider is judged by its probed exit (owner decision 2026-11-20, "judge
// by exit IP"). The exit is the address the operator's /ip echo saw through
// the provider's tunnel (controller.SubmitProviderEgressLocation): where the
// provider's users' traffic leaves. The address the provider connects to the
// platform from can be somewhere else entirely. A miner proxy slot on a build
// that sends its users' traffic out of its host, rather than its proxy,
// connects from a residential proxy and exits from a datacenter. So wherever a
// provider has a fresh probe:
//
//   - ARIN Quality is the exit's: each of its connections carries the exit's
//     non-Quality and verified-subscriber facts, with their database build
//     epochs, in both slots of the pair (ApplyArinFacts), instead of its own
//     address's. Risk is either address's: a risky exit, a known VPN, Tor or
//     proxy address, takes the provider out of every bucket, Speed and Online
//     included, as its own risky address always has.
//   - Country is the exit's: the probe corrects a lookup's country, as
//     before, and when the exit differs from every network the provider
//     connects from, it places the provider ahead of a derived location and
//     however coarse, since both then describe the network the provider
//     connects from, not the exit.
//   - Units and settlement: a provider whose exit differs from every network
//     it connected from at the probe (ExitDiffers) gets the mark of a declared
//     proxy egress (MarkVerifyProxiedEgress), so it counts at no SN Top,
//     capture the flag or extender unit, and its day is recorded durably for
//     the settlement epoch (provider_egress_exit_day). From SN25 epoch 7, a
//     provider with a differing judgement in an epoch's window earns no usage
//     in it (GetProviderEgressExitDiffersClientIds, read by the epoch's
//     provider rows).
//
// The exit differs only on positive evidence (compareProviderEgressExit): no
// network the provider connected from at the probe is the exit's network in
// its address family (the /29 of an IPv4 address, the /56 of an IPv6 one, as
// each connection's address hash keeps it, server.ClientIpHashForAddr), none
// is in the exit's autonomous system in either family, and every system is
// known. So a carrier NAT or a second line of one ISP, and a provider that
// connects over IPv6 while the probe exits over IPv4, are one network, while
// a farm that connects through a proxy list in other systems than its exit is
// not. The connect announce records the system of each network a probed
// provider connects from (SetProviderConnectionNetworkAsn); a network without
// that record is no evidence, so a provider is first judged to differ at a
// probe after it has connected since an earlier one.
//
// The exit address is never stored. The ingest judges it once, and keeps the
// judgement per client in Redis while the probe is fresh
// (ProviderEgressLocationMaxAge): the connect announce reads it to judge each
// new connection, and the ingest re-judges the provider's live connections. A
// lost judgement judges the connection's own address, as before this change,
// until the provider's next probe.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// What a provider is judged by while its probe is fresh.
type ProviderEgressExitJudgement struct {
	// when the probe observed the exit, by the prober's clock
	ObservedAt time.Time `json:"observed_at"`
	// The exit's ARIN facts in each slot of the ingest's database pair, as
	// setArinConnectionFacts stamps a connection. A latest epoch of 0 is an
	// exit no database classified: connections keep their own facts.
	ArinRisk                  bool      `json:"arin_risk"`
	ArinNonQuality            bool      `json:"arin_non_quality"`
	ArinQualityVerified       bool      `json:"arin_quality_verified"`
	ArinDatabaseBuildEpoch    int64     `json:"arin_database_build_epoch"`
	ArinLookupAt              time.Time `json:"arin_lookup_at"`
	ArinOldRisk               bool      `json:"arin_old_risk"`
	ArinOldNonQuality         bool      `json:"arin_old_non_quality"`
	ArinOldQualityVerified    bool      `json:"arin_old_quality_verified"`
	ArinOldDatabaseBuildEpoch int64     `json:"arin_old_database_build_epoch"`
	// the exit differs from every network the provider connected from at the
	// probe (compareProviderEgressExit)
	ExitDiffers bool `json:"exit_differs"`
}

// Judges a connection's ARIN facts by the exit as well as its own address.
// Each slot's non-Quality and verified-subscriber facts and its epoch become
// the exit's. Risk is either address's (owner decision 2026-11-20, "apply exit
// risk too"): a risky exit, a known VPN, Tor or proxy address among them,
// makes the connection risky as if it connected from there, which excludes it
// from every bucket, while the connection's own risk still holds; a risky
// slot is never verified. An exit without an old slot clears the
// connection's: under a fallback to the old database, the connection's own
// address would otherwise decide its Quality. A nil judgement, or an exit no
// database classified, changes nothing.
func (self *ProviderEgressExitJudgement) ApplyArinFacts(scores *ConnectionLocationScores) {
	if self == nil || scores == nil || self.ArinDatabaseBuildEpoch <= 0 {
		return
	}
	scores.ArinRisk = scores.ArinRisk || self.ArinRisk
	scores.ArinNonQuality = self.ArinNonQuality
	scores.ArinQualityVerified = self.ArinQualityVerified && !scores.ArinRisk
	scores.ArinDatabaseBuildEpoch = self.ArinDatabaseBuildEpoch
	lookupAt := self.ArinLookupAt
	scores.ArinLookupAt = &lookupAt
	if 0 < self.ArinOldDatabaseBuildEpoch {
		scores.ArinOldRisk = scores.ArinOldRisk || self.ArinOldRisk
		scores.ArinOldNonQuality = self.ArinOldNonQuality
		scores.ArinOldQualityVerified = self.ArinOldQualityVerified && !scores.ArinOldRisk
		scores.ArinOldDatabaseBuildEpoch = self.ArinOldDatabaseBuildEpoch
	} else {
		scores.ArinOldRisk = false
		scores.ArinOldNonQuality = false
		scores.ArinOldQualityVerified = false
		scores.ArinOldDatabaseBuildEpoch = 0
	}
}

// Whether the provider's exit differs from every network it connects from. A
// nil judgement is no evidence that it does.
func (self *ProviderEgressExitJudgement) Differs() bool {
	return self != nil && self.ExitDiffers
}

func providerEgressExitKey(clientId server.Id) string {
	return fmt.Sprintf("{pexit_%s}", clientId)
}

// KEYS[1] is the client's judgement; ARGV[1] the probe's observed_at in unix
// milliseconds, ARGV[2] the judgement and ARGV[3] its lifetime in
// milliseconds. Stores the judgement unless a later probe's is stored, and
// returns 1 when it did. A replay of the stored probe stores it again, so a
// retried submission completes the work a failed one left.
var providerEgressExitSetScript = redis.NewScript(`
local stored = redis.call('HGET', KEYS[1], 'observed_at')
if stored and tonumber(ARGV[1]) < tonumber(stored) then
  return 0
end
redis.call('HSET', KEYS[1], 'observed_at', ARGV[1], 'judgement', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// Stores the judgement for as long as its probe is fresh, unless a later
// probe's is stored. Returns whether it stored it.
func setProviderEgressExitJudgement(ctx context.Context, clientId server.Id, judgement *ProviderEgressExitJudgement) (stored bool) {
	lifetime := judgement.ObservedAt.Add(ProviderEgressLocationMaxAge).Sub(server.NowUtc())
	if lifetime <= 0 {
		return false
	}
	judgementJson, err := json.Marshal(judgement)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		result, err := providerEgressExitSetScript.Run(
			ctx,
			r,
			[]string{providerEgressExitKey(clientId)},
			strconv.FormatInt(judgement.ObservedAt.UnixMilli(), 10),
			string(judgementJson),
			strconv.FormatInt(lifetime.Milliseconds(), 10),
		).Int()
		server.Raise(err)
		stored = result == 1
	})
	return
}

// The client's judgement while its probe is fresh, nil without one. A
// malformed judgement raises, as every malformed state read does.
func GetProviderEgressExitJudgement(ctx context.Context, clientId server.Id) *ProviderEgressExitJudgement {
	var judgement *ProviderEgressExitJudgement
	server.Redis(ctx, func(r server.RedisClient) {
		judgementJson, err := r.HGet(ctx, providerEgressExitKey(clientId), "judgement").Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		judgement = &ProviderEgressExitJudgement{}
		server.Raise(json.Unmarshal([]byte(judgementJson), judgement))
	})
	if judgement != nil && judgement.ObservedAt.Before(server.NowUtc().Add(-ProviderEgressLocationMaxAge)) {
		return nil
	}
	return judgement
}

// One network a provider connected from, or its exit: the address family (4
// or 6, 0 when unknown), the address hash (server.ClientIpHashForAddr: the /29
// of an IPv4 address, the /56 of an IPv6 one) and the autonomous system (0
// when unknown).
type providerEgressNetwork struct {
	family      int
	addressHash [32]byte
	asn         uint32
}

// How an exit compares with the networks its provider connected from at the
// probe.
type providerEgressExitComparison struct {
	// on positive evidence only: there is a network to compare, none is the
	// exit's network in its family or in the exit's system in either family,
	// and every system is known
	differs bool
	// no network is in the exit's address family
	crossFamily bool
}

// Compares an exit with the networks its provider connected from. A network
// with an unknown system, or an exit with one, could be the exit's own, so it
// is no evidence of a difference.
func compareProviderEgressExit(exit providerEgressNetwork, networks []providerEgressNetwork) providerEgressExitComparison {
	if len(networks) == 0 {
		return providerEgressExitComparison{}
	}
	sameFamily := false
	sameNetwork := false
	sameAsn := false
	unknownAsn := exit.asn == 0
	for _, network := range networks {
		if network.family == exit.family {
			sameFamily = true
			if network.addressHash == exit.addressHash {
				sameNetwork = true
			}
		}
		switch {
		case network.asn == 0:
			unknownAsn = true
		case network.asn == exit.asn:
			sameAsn = true
		}
	}
	return providerEgressExitComparison{
		differs:     !sameNetwork && !sameAsn && !unknownAsn,
		crossFamily: !sameFamily,
	}
}

// Replaces the GeoLite2 ASN lookup (server.GetIpAsn) while set. Tests only.
var providerNetworkAsnOverride atomic.Pointer[func(netip.Addr) uint32]

// The autonomous system of an address, 0 when unknown.
func providerNetworkAsn(addr netip.Addr) uint32 {
	if override := providerNetworkAsnOverride.Load(); override != nil {
		return (*override)(addr)
	}
	if asn := server.GetIpAsn(addr); asn != nil {
		return asn.Number
	}
	return 0
}

// Numbers addresses with asnOf in place of GeoLite2 ASN until restore is
// called. Tests only; they number their synthetic addresses.
func Testing_SetProviderNetworkAsn(asnOf func(netip.Addr) uint32) (restore func()) {
	previous := providerNetworkAsnOverride.Swap(&asnOf)
	return func() {
		providerNetworkAsnOverride.Store(previous)
	}
}

// The exit's network.
func providerEgressNetworkOf(addr netip.Addr) providerEgressNetwork {
	addr = addr.Unmap()
	return providerEgressNetwork{
		family:      server.IpVersionForAddr(addr),
		addressHash: server.ClientIpHashForAddr(addr),
		asn:         providerNetworkAsn(addr),
	}
}

func providerConnectionNetworkAsnKey(clientId server.Id) string {
	return fmt.Sprintf("{pnet_%s}", clientId)
}

// Records the autonomous system of the network a probed provider's connection
// came from, by the connection row's own address hash (server.ClientIpHash),
// for the comparison of the provider's next probe. One field per network,
// kept while the provider's probes are fresh. An address whose system is
// unknown records nothing, which is no evidence.
func SetProviderConnectionNetworkAsn(ctx context.Context, clientId server.Id, clientIp string) {
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return
	}
	asn := providerNetworkAsn(addr)
	if asn == 0 {
		return
	}
	addressHash, err := server.ClientIpHash(clientIp)
	if err != nil {
		return
	}
	key := providerConnectionNetworkAsnKey(clientId)
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, key, hex.EncodeToString(addressHash[:]), strconv.FormatUint(uint64(asn), 10))
			pipe.PExpire(ctx, key, ProviderEgressLocationMaxAge)
			return nil
		})
		server.Raise(err)
	})
}

// The recorded systems of the client's networks, by address hash. A
// malformed field is skipped: it is only less evidence.
func getProviderConnectionNetworkAsns(ctx context.Context, clientId server.Id) map[[32]byte]uint32 {
	asns := map[[32]byte]uint32{}
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.HGetAll(ctx, providerConnectionNetworkAsnKey(clientId)).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		for field, value := range values {
			addressHash, err := hex.DecodeString(field)
			if err != nil || len(addressHash) != 32 {
				continue
			}
			asn, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				continue
			}
			asns[[32]byte(addressHash)] = uint32(asn)
		}
	})
	return asns
}

// The networks of the client's connections that were live at the probe:
// every connected one, and every one that closed at or after the probe less
// the clock skew the ingest allows the prober (skew), each with its recorded
// system. It reads network_client_connection_client_id_connect_time, whose
// rows for one client the closed-connection sweep bounds.
func getProviderConnectionNetworksAt(ctx context.Context, clientId server.Id, observedAt time.Time, skew time.Duration) []providerEgressNetwork {
	networks := []providerEgressNetwork{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT
				client_address_hash,
				COALESCE(ip_version, 0)
			FROM network_client_connection
			WHERE
				client_id = $1 AND
				client_address_hash IS NOT NULL AND
				(connected OR $2 <= disconnect_time)
			`,
			clientId,
			observedAt.UTC().Add(-skew),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var addressHash []byte
				var family int
				server.Raise(result.Scan(&addressHash, &family))
				if len(addressHash) == 32 {
					networks = append(networks, providerEgressNetwork{
						family:      family,
						addressHash: [32]byte(addressHash),
					})
				}
			}
		})
	})
	if 0 < len(networks) {
		asns := getProviderConnectionNetworkAsns(ctx, clientId)
		for i := range networks {
			networks[i].asn = asns[networks[i].addressHash]
		}
	}
	return networks
}

// Counts a differing judgement in its client's row for the UTC day its probe
// observed the exit. A replay of a probe already counted counts nothing.
func recordProviderEgressExitDay(ctx context.Context, clientId server.Id, observedAt time.Time, crossFamily bool) {
	observedAt = observedAt.UTC()
	day := time.Date(observedAt.Year(), observedAt.Month(), observedAt.Day(), 0, 0, 0, 0, time.UTC)
	crossFamilyCount := 0
	if crossFamily {
		crossFamilyCount = 1
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			INSERT INTO provider_egress_exit_day (
				day,
				client_id,
				network_id,
				differs_count,
				cross_family_count,
				first_observed_at,
				last_observed_at
			)
			SELECT $1, network_client.client_id, network_client.network_id, 1, $3, $4, $4
			FROM network_client
			WHERE network_client.client_id = $2
			ON CONFLICT (day, client_id) DO UPDATE
			SET
				differs_count = provider_egress_exit_day.differs_count + 1,
				cross_family_count = provider_egress_exit_day.cross_family_count + EXCLUDED.cross_family_count,
				last_observed_at = EXCLUDED.last_observed_at
			WHERE provider_egress_exit_day.last_observed_at < EXCLUDED.last_observed_at
			`,
			day,
			clientId,
			crossFamilyCount,
			observedAt,
		))
	})
}

// The clients the durable record holds a differing judgement for whose probe
// observed the exit in [startTime, endTime): the first or the last judgement
// of one of their days. Both are judgements a probe made, so a day counts only
// on its own evidence, never on one inferred between them. In a window of a
// day or more that is exact: a day's judgements span less than a day, so when
// any lies in the window, its first or its last does. A client the record has
// nothing for in the window is not in it, whether its probes found its exit
// in its own networks, found no evidence, or never ran. The settlement epoch
// reads this after the epoch (controller st_ledger_v2_rows.go).
func GetProviderEgressExitDiffersClientIds(ctx context.Context, startTime time.Time, endTime time.Time) map[server.Id]bool {
	clientIds := map[server.Id]bool{}
	startTime, endTime = startTime.UTC(), endTime.UTC()
	if !startTime.Before(endTime) {
		return clientIds
	}
	// a row's judgements all fall on its own day
	firstDay := time.Date(startTime.Year(), startTime.Month(), startTime.Day(), 0, 0, 0, 0, time.UTC)
	lastDay := time.Date(endTime.Year(), endTime.Month(), endTime.Day(), 0, 0, 0, 0, time.UTC)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT client_id
			FROM provider_egress_exit_day
			WHERE
				$1 <= day AND
				day <= $2 AND
				0 < differs_count AND
				(
					($3 <= first_observed_at AND first_observed_at < $4) OR
					($3 <= last_observed_at AND last_observed_at < $4)
				)
			`,
			firstDay,
			lastDay,
			startTime,
			endTime,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds[clientId] = true
			}
		})
	})
	return clientIds
}

// Judges a provider by the exit a probe observed at judgement.ObservedAt,
// whose ARIN facts the caller classified into judgement, and returns the
// judgement with ExitDiffers set and whether it was the latest:
//
//  1. whether the exit differs from every network the provider connected
//     from at the probe, allowing skew for the prober's clock
//     (compareProviderEgressExit);
//  2. the judgement, kept for the connect announce, unless a later probe's is
//     kept;
//  3. for the latest judgement whose exit differs, the 11-19 mark of a
//     declared proxy egress, and the day in the durable record
//     (recordProviderEgressExitDay);
//  4. and for the latest judgement, the provider's live connections, judged
//     by it and placed by locationId, the probe's place
//     (judgeProviderConnectionsByExit).
//
// Each step takes and releases its own connection.
func JudgeProviderEgressExit(
	ctx context.Context,
	clientId server.Id,
	exitIp netip.Addr,
	judgement ProviderEgressExitJudgement,
	locationId server.Id,
	skew time.Duration,
) (judged ProviderEgressExitJudgement, latest bool) {
	comparison := compareProviderEgressExit(
		providerEgressNetworkOf(exitIp),
		getProviderConnectionNetworksAt(ctx, clientId, judgement.ObservedAt, skew),
	)
	judgement.ExitDiffers = comparison.differs
	if !setProviderEgressExitJudgement(ctx, clientId, &judgement) {
		return judgement, false
	}
	if comparison.differs {
		MarkVerifyProxiedEgress(ctx, clientId)
		recordProviderEgressExitDay(ctx, clientId, judgement.ObservedAt, comparison.crossFamily)
	}
	judgeProviderConnectionsByExit(ctx, clientId, &judgement, locationId)
	return judgement, true
}

// One live connection's location row, as judgeProviderConnectionsByExit reads
// and rewrites it.
type providerExitConnectionRow struct {
	connectionId      server.Id
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
	scores            ConnectionLocationScores
	writeToken        *server.Id
}

// Judges the client's live connections by its exit, as SetConnectionLocation
// in the connect announce judges a new one: their ARIN facts are judged by the
// exit (ApplyArinFacts), and when the exit differs, a row is placed at the
// probe's place. A placed row, like one the probe placed at connect, has
// no genesis or radius of its own. A row already judged so is not written. A
// row the connect announce rewrote since it was read, as its write token
// shows, is left as written: that write judged it by the stored judgement.
func judgeProviderConnectionsByExit(ctx context.Context, clientId server.Id, judgement *ProviderEgressExitJudgement, locationId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		var cityLocationId, regionLocationId, countryLocationId *server.Id
		result, err := tx.Query(
			ctx,
			`
			SELECT
				city_location_id,
				region_location_id,
				country_location_id
			FROM location
			WHERE location_id = $1
			`,
			locationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&cityLocationId, &regionLocationId, &countryLocationId))
			}
		})
		placeable := countryLocationId != nil
		if placeable {
			// a coarser place files under its country, as model.SetConnectionLocation stores it
			if cityLocationId == nil {
				cityLocationId = countryLocationId
			}
			if regionLocationId == nil {
				regionLocationId = countryLocationId
			}
		}

		rows := []*providerExitConnectionRow{}
		result, err = tx.Query(
			ctx,
			`
			SELECT
				network_client_location.connection_id,
				network_client_location.city_location_id,
				network_client_location.region_location_id,
				network_client_location.country_location_id,
				network_client_location.arin_risk,
				network_client_location.arin_non_quality,
				network_client_location.arin_quality_verified,
				network_client_location.arin_lookup_at,
				network_client_location.arin_database_build_epoch,
				network_client_location.arin_old_risk,
				network_client_location.arin_old_non_quality,
				network_client_location.arin_old_quality_verified,
				network_client_location.arin_old_database_build_epoch,
				network_client_location.arin_quality_write_token
			FROM network_client_connection
			INNER JOIN network_client_location ON
				network_client_location.connection_id = network_client_connection.connection_id
			WHERE
				network_client_connection.client_id = $1 AND
				network_client_connection.connected
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &providerExitConnectionRow{}
				server.Raise(result.Scan(
					&row.connectionId,
					&row.cityLocationId,
					&row.regionLocationId,
					&row.countryLocationId,
					&row.scores.ArinRisk,
					&row.scores.ArinNonQuality,
					&row.scores.ArinQualityVerified,
					&row.scores.ArinLookupAt,
					&row.scores.ArinDatabaseBuildEpoch,
					&row.scores.ArinOldRisk,
					&row.scores.ArinOldNonQuality,
					&row.scores.ArinOldQualityVerified,
					&row.scores.ArinOldDatabaseBuildEpoch,
					&row.writeToken,
				))
				rows = append(rows, row)
			}
		})

		for _, row := range rows {
			judgedScores := row.scores
			judgement.ApplyArinFacts(&judgedScores)
			factsChanged := judgedScores.ArinRisk != row.scores.ArinRisk ||
				judgedScores.ArinNonQuality != row.scores.ArinNonQuality ||
				judgedScores.ArinQualityVerified != row.scores.ArinQualityVerified ||
				judgedScores.ArinDatabaseBuildEpoch != row.scores.ArinDatabaseBuildEpoch ||
				judgedScores.ArinOldRisk != row.scores.ArinOldRisk ||
				judgedScores.ArinOldNonQuality != row.scores.ArinOldNonQuality ||
				judgedScores.ArinOldQualityVerified != row.scores.ArinOldQualityVerified ||
				judgedScores.ArinOldDatabaseBuildEpoch != row.scores.ArinOldDatabaseBuildEpoch
			place := placeable && judgement.ExitDiffers &&
				(row.cityLocationId != *cityLocationId || row.regionLocationId != *regionLocationId || row.countryLocationId != *countryLocationId)
			if !factsChanged && !place {
				continue
			}
			if place {
				row.cityLocationId = *cityLocationId
				row.regionLocationId = *regionLocationId
				row.countryLocationId = *countryLocationId
			}
			// fresh tokens keep the written verified facts (db_subscriber_quality.go,
			// db_arindb_canary.go)
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_client_location
				SET
					city_location_id = $2,
					region_location_id = $3,
					country_location_id = $4,
					genesis_location_id = CASE WHEN $5 THEN NULL ELSE genesis_location_id END,
					accuracy_km = CASE WHEN $5 THEN NULL ELSE accuracy_km END,
					arin_non_quality = $6,
					arin_quality_verified = $7,
					arin_lookup_at = $8,
					arin_database_build_epoch = $9,
					arin_quality_write_token = $10,
					arin_old_risk = $11,
					arin_old_non_quality = $12,
					arin_old_quality_verified = $13,
					arin_old_database_build_epoch = $14,
					arin_old_quality_write_token = $15,
					arin_risk = $17
				WHERE
					connection_id = $1 AND
					arin_quality_write_token IS NOT DISTINCT FROM $16
				`,
				row.connectionId,
				row.cityLocationId,
				row.regionLocationId,
				row.countryLocationId,
				place,
				judgedScores.ArinNonQuality,
				judgedScores.ArinQualityVerified,
				judgedScores.ArinLookupAt,
				judgedScores.ArinDatabaseBuildEpoch,
				server.NewId(),
				judgedScores.ArinOldRisk,
				judgedScores.ArinOldNonQuality,
				judgedScores.ArinOldQualityVerified,
				judgedScores.ArinOldDatabaseBuildEpoch,
				server.NewId(),
				row.writeToken,
				judgedScores.ArinRisk,
			))
		}
	})
}

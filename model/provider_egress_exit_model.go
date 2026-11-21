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
//     address's. Risk stays the connection address's: it is the one ARIN
//     fact that also gates Speed and Online, which keep their providers.
//   - Country is the exit's: the probe corrects a lookup's country, as
//     before, and when the exit is in none of the networks the provider
//     connects from, it places the provider ahead of a derived location and
//     however coarse, since both then describe the network the provider
//     connects from, not the exit.
//   - Units: a provider whose exit is in none of the networks its
//     connections came from at the probe (ExitDiffers) gets the mark of a
//     declared proxy egress (MarkVerifyProxiedEgress), so it counts at no SN
//     Top, capture the flag or extender unit. A network is the /29 of an IPv4
//     address or the /56 of an IPv6 one, as each connection's address hash
//     keeps it (server.ClientIpHashForAddr); the IPv4 /29 is the head unit.
//
// The exit address is never stored. The ingest judges it once, and keeps the
// judgement per client in Redis while the probe is fresh
// (ProviderEgressLocationMaxAge): the connect announce reads it to judge each
// new connection, and the ingest re-judges the provider's live connections. A
// lost judgement judges the connection's own address, as before this change,
// until the provider's next probe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
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
	ArinNonQuality            bool      `json:"arin_non_quality"`
	ArinQualityVerified       bool      `json:"arin_quality_verified"`
	ArinDatabaseBuildEpoch    int64     `json:"arin_database_build_epoch"`
	ArinLookupAt              time.Time `json:"arin_lookup_at"`
	ArinOldNonQuality         bool      `json:"arin_old_non_quality"`
	ArinOldQualityVerified    bool      `json:"arin_old_quality_verified"`
	ArinOldDatabaseBuildEpoch int64     `json:"arin_old_database_build_epoch"`
	// the exit is in none of the networks the provider's connections came
	// from at the probe
	ExitDiffers bool `json:"exit_differs"`
}

// Replaces the Quality facts of a connection's own address with the exit's.
// Each slot's non-Quality and verified-subscriber facts and its epoch become
// the exit's; risk stays the connection's, and a risky slot is never
// verified. An exit without an old slot clears the connection's: under a
// fallback to the old database, the connection's own address would otherwise
// decide its Quality. A nil judgement, or an exit no database classified,
// changes nothing.
func (self *ProviderEgressExitJudgement) ApplyArinFacts(scores *ConnectionLocationScores) {
	if self == nil || scores == nil || self.ArinDatabaseBuildEpoch <= 0 {
		return
	}
	scores.ArinNonQuality = self.ArinNonQuality
	scores.ArinQualityVerified = self.ArinQualityVerified && !scores.ArinRisk
	scores.ArinDatabaseBuildEpoch = self.ArinDatabaseBuildEpoch
	lookupAt := self.ArinLookupAt
	scores.ArinLookupAt = &lookupAt
	if 0 < self.ArinOldDatabaseBuildEpoch {
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

// Whether the provider's exit is in none of the networks it connects from. A
// nil judgement is no evidence that it is.
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

// Whether the exit's network is none of the connections' networks. Each hash
// is server.ClientIpHashForAddr's. Without a connection to compare, it is
// not: there is no evidence the provider connects from anywhere else.
func providerEgressExitDiffers(exitIp netip.Addr, connectionAddressHashes [][32]byte) bool {
	if len(connectionAddressHashes) == 0 {
		return false
	}
	return !slices.Contains(connectionAddressHashes, server.ClientIpHashForAddr(exitIp.Unmap()))
}

// The address hashes of the client's connections that were live at the
// probe: every connected one, and every one that closed at or after the probe
// less the clock skew the ingest allows the prober (skew). It reads
// network_client_connection_client_id_connect_time, whose rows for one client
// the closed-connection sweep bounds.
func getProviderConnectionAddressHashesAt(ctx context.Context, clientId server.Id, observedAt time.Time, skew time.Duration) [][32]byte {
	connectionAddressHashes := [][32]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT client_address_hash
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
				server.Raise(result.Scan(&addressHash))
				if len(addressHash) == 32 {
					connectionAddressHashes = append(connectionAddressHashes, [32]byte(addressHash))
				}
			}
		})
	})
	return connectionAddressHashes
}

// Judges a provider by the exit a probe observed at judgement.ObservedAt,
// whose ARIN facts the caller classified into judgement, and returns the
// judgement with ExitDiffers set and whether it was the latest:
//
//  1. whether the exit differs from every network the provider connected
//     from at the probe, allowing skew for the prober's clock;
//  2. the judgement, kept for the connect announce, unless a later probe's is
//     kept;
//  3. for the latest judgement, the 11-19 mark of a declared proxy egress
//     when the exit differs;
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
	judgement.ExitDiffers = providerEgressExitDiffers(
		exitIp,
		getProviderConnectionAddressHashesAt(ctx, clientId, judgement.ObservedAt, skew),
	)
	if !setProviderEgressExitJudgement(ctx, clientId, &judgement) {
		return judgement, false
	}
	if judgement.ExitDiffers {
		MarkVerifyProxiedEgress(ctx, clientId)
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
// in the connect announce judges a new one: their ARIN Quality facts become
// the exit's (ApplyArinFacts), and when the exit differs, a row is placed at
// the probe's place. A placed row, like one the probe placed at connect, has
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
			factsChanged := judgedScores.ArinNonQuality != row.scores.ArinNonQuality ||
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
					arin_old_quality_write_token = $15
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
			))
		}
	})
}

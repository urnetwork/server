package model

// Capture the flag's records after presence (sn CAPTURETHEFLAG.md §4.3–§5,
// ctf_schema.go): each epoch's census, written once by the census step; its
// on-chain commitment and draw; what each covered epoch fixed at the first
// live ledger that covered it; the ledgers built; and the carry contract's
// returns. The census step writes the census, the driver everything after it,
// and the api reads them all.
//
// Every record is keyed by the deployment and the operator's v1 id n. The CTF
// pool's on-chain mirror (roots, accounts, funding, demands, amounts) is the
// st_ledger_v2 tables under its pool id 2^32 + n.

import (
	"context"
	"fmt"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

// The census's exclusion reasons (sn ctf.Reason): a member that is not a
// candidate is counted under the lowest-numbered one it fails.
const (
	CtfReasonRevoked        = 1
	CtfReasonOffline        = 2
	CtfReasonNotPublic      = 3
	CtfReasonNoLocation     = 4
	CtfReasonOwnNetwork     = 5
	CtfReasonHeadBound      = 6
	CtfReasonNoWallet       = 7
	CtfReasonDuplicate      = 8
	CtfReasonCountryChanged = 9
	CtfReasonPresence       = 10
	CtfNumReasons           = 10
)

// CtfNearMissReasons are the rules a near miss fails (§9.1): a member that
// failed only these was present long enough, or in the wrong country.
const CtfNearMissReasons uint32 = 1<<CtfReasonCountryChanged | 1<<CtfReasonPresence

// The rows of one statement of a bulk insert.
const ctfBatchRows = 5000

// ---------------------------------------------------------------------------
// Census
// ---------------------------------------------------------------------------

// An epoch's census (§4.3) and its counts.
type CtfCensus struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	Epoch         uint64
	// sha256 of Header, what commitCtfCensus commits
	CensusHash [32]byte
	// the canonical census header (urnetwork-ctf-census-v1)
	Header             []byte
	Candidates         int64
	ProviderCandidates int64
	ExtenderCandidates int64
	ProviderUnits      int64
	ExtenderUnits      int64
	// the miners the samples found in each role during the epoch: the
	// population the exclusion counts cover
	ProviderMembers int64
	ExtenderMembers int64
	// ascending sample indexes
	ExcusedSamples       []int
	SamplesTaken         int
	ProviderSamplesTaken int
	// half of the samples not excused, rounded up (CtfRequiredSamples)
	RequiredSamples int
	StartBlock      uint64
	EndBlock        uint64
	StartTime       time.Time
	EndTime         time.Time
	// when the end-boundary facts were read
	CaptureTime time.Time
	// the score export whose Online set decided P1 at the end
	ProviderSourceTime time.Time
	CreateTime         time.Time
	// when the census objects were put in the ledger object store
	PublishedTime *time.Time
}

// One object of a census, by its sha256.
type CtfCensusObject struct {
	Ref  [32]byte
	Data []byte
}

// One miner the samples found in a role during the epoch, with every rule of
// §4.2 evaluated.
type CtfCensusMember struct {
	Epoch    uint64
	Role     int
	ClientId server.Id
	// the client's network at the end, nil when it has none
	NetworkId *server.Id
	// role 2: the extender record evaluated at the end
	ExtenderId *server.Id
	Candidate  bool
	// 0 for a candidate, else the lowest-numbered reason it fails, which the
	// census counts it under
	Reason int
	// bit 1 << reason for every rule it fails
	Failed uint32
	// its own country at the end: a code of the 249, or CtfCountryUnknown
	CountryCode string
	// a candidate's unit's draw country, its lowest candidate client id's;
	// empty for a member that is not a candidate
	DrawCountryCode string
	// the samples not excused that found it present in CountryCode
	PresentSamples int
	// its unit and family (4 or 6), nil and 0 for none
	Unit       *[32]byte
	UnitFamily int
	// its payout coldkey under the provider pool's wallet selection (E4), nil
	// for none
	Coldkey *[32]byte
}

// NearMiss reports whether the member failed only presence, a change of
// country, or both (§9.1 near_misses).
func (self *CtfCensusMember) NearMiss() bool {
	return self.Failed != 0 && self.Failed&^CtfNearMissReasons == 0
}

// One country and role of a census.
type CtfCensusPot struct {
	CountryCode string
	Role        int
	// the units in the country and role's draw at the census
	Units      int
	Candidates int
	NearMisses int
}

// One row of a census's exclusion object (sn ctf.Exclusion).
type CtfCensusExclusion struct {
	CountryCode string
	Role        int
	Reason      int
	Count       int
}

// CreateCtfCensus writes an epoch's census with its objects, members, pots and
// exclusions, all or nothing, once. False when the epoch's census exists, in
// which case nothing is written.
func CreateCtfCensus(
	ctx context.Context,
	census *CtfCensus,
	objects []*CtfCensusObject,
	members []*CtfCensusMember,
	pots []*CtfCensusPot,
	exclusions []*CtfCensusExclusion,
) (created bool) {
	key := requireStDeploymentKey(census.DeploymentKey)
	excused := make([]int16, len(census.ExcusedSamples))
	for i, index := range census.ExcusedSamples {
		excused[i] = int16(index)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		created = false
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_census (
				deployment_key, no_id, epoch, census_hash, header,
				candidates, provider_candidates, extender_candidates, provider_units, extender_units,
				provider_members, extender_members, excused_samples, samples_taken, provider_samples_taken,
				required_samples, start_block, end_block, start_time, end_time,
				capture_time, provider_source_time, create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
			`,
			key, int64(census.NoId), int64(census.Epoch), census.CensusHash[:], census.Header,
			census.Candidates, census.ProviderCandidates, census.ExtenderCandidates, census.ProviderUnits, census.ExtenderUnits,
			census.ProviderMembers, census.ExtenderMembers, excused, int16(census.SamplesTaken), int16(census.ProviderSamplesTaken),
			int16(census.RequiredSamples), int64(census.StartBlock), int64(census.EndBlock), census.StartTime.UTC(), census.EndTime.UTC(),
			census.CaptureTime.UTC(), census.ProviderSourceTime.UTC(), server.NowUtc(),
		)
		server.Raise(err)
		if tag.RowsAffected() == 0 {
			return
		}
		created = true

		refs := make([][]byte, len(objects))
		datas := make([][]byte, len(objects))
		for i, object := range objects {
			refs[i] = object.Ref[:]
			datas[i] = object.Data
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO ctf_census_object (deployment_key, no_id, epoch, ref, data)
			SELECT $1::varchar, $2::bigint, $3::bigint, object.ref, object.data
			FROM unnest($4::bytea[], $5::bytea[]) AS object (ref, data)
			ON CONFLICT (deployment_key, no_id, epoch, ref) DO NOTHING
			`,
			key, int64(census.NoId), int64(census.Epoch), refs, datas,
		))

		for start := 0; start < len(members); start += ctfBatchRows {
			batch := members[start:min(start+ctfBatchRows, len(members))]
			roles := make([]int16, len(batch))
			clientIds := make([]server.Id, len(batch))
			networkIds := make([]server.Id, len(batch))
			extenderIds := make([]server.Id, len(batch))
			reasons := make([]int16, len(batch))
			faileds := make([]int32, len(batch))
			countryCodes := make([]string, len(batch))
			drawCountryCodes := make([]string, len(batch))
			presentSamples := make([]int16, len(batch))
			units := make([][]byte, len(batch))
			unitFamilies := make([]int16, len(batch))
			coldkeys := make([][]byte, len(batch))
			for i, member := range batch {
				if member.Candidate != (member.Reason == 0) || member.Candidate != (member.Failed == 0) {
					panic(fmt.Errorf("ctf census member %s is a candidate %t with reason %d and failed rules %b", member.ClientId, member.Candidate, member.Reason, member.Failed))
				}
				roles[i] = int16(member.Role)
				clientIds[i] = member.ClientId
				if member.NetworkId != nil {
					networkIds[i] = *member.NetworkId
				}
				if member.ExtenderId != nil {
					extenderIds[i] = *member.ExtenderId
				}
				reasons[i] = int16(member.Reason)
				faileds[i] = int32(member.Failed)
				countryCodes[i] = member.CountryCode
				drawCountryCodes[i] = member.DrawCountryCode
				presentSamples[i] = int16(member.PresentSamples)
				units[i] = []byte{}
				if member.Unit != nil {
					units[i] = member.Unit[:]
					unitFamilies[i] = int16(member.UnitFamily)
				}
				coldkeys[i] = []byte{}
				if member.Coldkey != nil {
					coldkeys[i] = member.Coldkey[:]
				}
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO ctf_census_member (
					deployment_key, no_id, epoch, role, client_id, network_id, extender_id,
					candidate, reason, failed, country_code, draw_country_code, present_samples,
					unit, unit_family, coldkey
				)
				SELECT
					$1::varchar, $2::bigint, $3::bigint, member.role, member.client_id,
					NULLIF(member.network_id, '00000000-0000-0000-0000-000000000000'::uuid),
					NULLIF(member.extender_id, '00000000-0000-0000-0000-000000000000'::uuid),
					member.reason = 0, member.reason, member.failed, member.country_code,
					NULLIF(member.draw_country_code, ''), member.present_samples,
					CASE WHEN octet_length(member.unit) = 32 THEN member.unit END,
					CASE WHEN octet_length(member.unit) = 32 THEN member.unit_family END,
					CASE WHEN octet_length(member.coldkey) = 32 THEN member.coldkey END
				FROM unnest(
					$4::smallint[], $5::uuid[], $6::uuid[], $7::uuid[], $8::smallint[], $9::integer[],
					$10::varchar[], $11::varchar[], $12::smallint[], $13::bytea[], $14::smallint[], $15::bytea[]
				) AS member (
					role, client_id, network_id, extender_id, reason, failed,
					country_code, draw_country_code, present_samples, unit, unit_family, coldkey
				)
				`,
				key, int64(census.NoId), int64(census.Epoch),
				roles, clientIds, networkIds, extenderIds, reasons, faileds,
				countryCodes, drawCountryCodes, presentSamples, units, unitFamilies, coldkeys,
			))
		}

		potCountryCodes := make([]string, len(pots))
		potRoles := make([]int16, len(pots))
		potUnits := make([]int32, len(pots))
		potCandidates := make([]int32, len(pots))
		potNearMisses := make([]int32, len(pots))
		for i, pot := range pots {
			potCountryCodes[i] = pot.CountryCode
			potRoles[i] = int16(pot.Role)
			potUnits[i] = int32(pot.Units)
			potCandidates[i] = int32(pot.Candidates)
			potNearMisses[i] = int32(pot.NearMisses)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO ctf_census_pot (deployment_key, no_id, epoch, country_code, role, units, candidates, near_misses)
			SELECT $1::varchar, $2::bigint, $3::bigint, pot.country_code, pot.role, pot.units, pot.candidates, pot.near_misses
			FROM unnest($4::varchar[], $5::smallint[], $6::integer[], $7::integer[], $8::integer[])
				AS pot (country_code, role, units, candidates, near_misses)
			`,
			key, int64(census.NoId), int64(census.Epoch), potCountryCodes, potRoles, potUnits, potCandidates, potNearMisses,
		))

		exclusionCountryCodes := make([]string, len(exclusions))
		exclusionRoles := make([]int16, len(exclusions))
		exclusionReasons := make([]int16, len(exclusions))
		exclusionCounts := make([]int32, len(exclusions))
		for i, exclusion := range exclusions {
			exclusionCountryCodes[i] = exclusion.CountryCode
			exclusionRoles[i] = int16(exclusion.Role)
			exclusionReasons[i] = int16(exclusion.Reason)
			exclusionCounts[i] = int32(exclusion.Count)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO ctf_census_exclusion (deployment_key, no_id, epoch, country_code, role, reason, count)
			SELECT $1::varchar, $2::bigint, $3::bigint, exclusion.country_code, exclusion.role, exclusion.reason, exclusion.count
			FROM unnest($4::varchar[], $5::smallint[], $6::smallint[], $7::integer[])
				AS exclusion (country_code, role, reason, count)
			`,
			key, int64(census.NoId), int64(census.Epoch), exclusionCountryCodes, exclusionRoles, exclusionReasons, exclusionCounts,
		))
	})
	return
}

const ctfCensusColumns = `
	census_hash, header, candidates, provider_candidates, extender_candidates, provider_units, extender_units,
	provider_members, extender_members, excused_samples, samples_taken, provider_samples_taken, required_samples,
	start_block, end_block, start_time, end_time, capture_time, provider_source_time, create_time, published_time
`

func scanCtfCensus(result server.PgResult, census *CtfCensus) {
	var censusHash []byte
	var excused []int16
	var samplesTaken, providerSamplesTaken, requiredSamples int16
	var startBlock, endBlock int64
	server.Raise(result.Scan(
		&censusHash, &census.Header, &census.Candidates, &census.ProviderCandidates, &census.ExtenderCandidates,
		&census.ProviderUnits, &census.ExtenderUnits, &census.ProviderMembers, &census.ExtenderMembers,
		&excused, &samplesTaken, &providerSamplesTaken, &requiredSamples,
		&startBlock, &endBlock, &census.StartTime, &census.EndTime, &census.CaptureTime, &census.ProviderSourceTime,
		&census.CreateTime, &census.PublishedTime,
	))
	census.CensusHash = [32]byte(censusHash)
	census.ExcusedSamples = make([]int, len(excused))
	for i, index := range excused {
		census.ExcusedSamples[i] = int(index)
	}
	census.SamplesTaken = int(samplesTaken)
	census.ProviderSamplesTaken = int(providerSamplesTaken)
	census.RequiredSamples = int(requiredSamples)
	census.StartBlock = uint64(startBlock)
	census.EndBlock = uint64(endBlock)
}

// GetCtfCensus returns an epoch's census, or nil before it is built.
func GetCtfCensus(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *CtfCensus {
	key := requireStDeploymentKey(deploymentKey)
	var census *CtfCensus
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfCensusColumns+` FROM ctf_census WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				census = &CtfCensus{DeploymentKey: deploymentKey, NoId: noId, Epoch: epoch}
				scanCtfCensus(result, census)
			}
		})
	})
	return census
}

// GetCtfCensuses returns the censuses of epochs fromEpoch … toEpoch, by epoch.
func GetCtfCensuses(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, fromEpoch uint64, toEpoch uint64) []*CtfCensus {
	key := requireStDeploymentKey(deploymentKey)
	censuses := []*CtfCensus{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT epoch, `+ctfCensusColumns+` FROM ctf_census
			WHERE deployment_key = $1 AND no_id = $2 AND $3 <= epoch AND epoch <= $4
			ORDER BY epoch`,
			key, int64(noId), int64(fromEpoch), int64(toEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var epoch int64
				census := &CtfCensus{DeploymentKey: deploymentKey, NoId: noId}
				var censusHash []byte
				var excused []int16
				var samplesTaken, providerSamplesTaken, requiredSamples int16
				var startBlock, endBlock int64
				server.Raise(result.Scan(
					&epoch, &censusHash, &census.Header, &census.Candidates, &census.ProviderCandidates, &census.ExtenderCandidates,
					&census.ProviderUnits, &census.ExtenderUnits, &census.ProviderMembers, &census.ExtenderMembers,
					&excused, &samplesTaken, &providerSamplesTaken, &requiredSamples,
					&startBlock, &endBlock, &census.StartTime, &census.EndTime, &census.CaptureTime, &census.ProviderSourceTime,
					&census.CreateTime, &census.PublishedTime,
				))
				census.Epoch = uint64(epoch)
				census.CensusHash = [32]byte(censusHash)
				census.ExcusedSamples = make([]int, len(excused))
				for i, index := range excused {
					census.ExcusedSamples[i] = int(index)
				}
				census.SamplesTaken = int(samplesTaken)
				census.ProviderSamplesTaken = int(providerSamplesTaken)
				census.RequiredSamples = int(requiredSamples)
				census.StartBlock = uint64(startBlock)
				census.EndBlock = uint64(endBlock)
				censuses = append(censuses, census)
			}
		})
	})
	return censuses
}

// GetCtfCensusObjects returns a census's objects by sha256: the header, the
// row chunks, the exclusion object and the countries object (sn
// ctf.CensusInput.Objects). Empty once retention has removed them; they stay
// in the ledger object store.
func GetCtfCensusObjects(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) map[[32]byte][]byte {
	key := requireStDeploymentKey(deploymentKey)
	objects := map[[32]byte][]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT ref, data FROM ctf_census_object WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var ref, data []byte
				server.Raise(result.Scan(&ref, &data))
				objects[[32]byte(ref)] = data
			}
		})
	})
	return objects
}

// SetCtfCensusPublished records that a census's objects are in the ledger
// object store.
func SetCtfCensusPublished(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, publishedTime time.Time) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE ctf_census SET published_time = $4
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND published_time IS NULL
			`,
			key, int64(noId), int64(epoch), publishedTime.UTC(),
		))
	})
}

const ctfCensusMemberColumns = `
	epoch, role, client_id, network_id, extender_id, candidate, reason, failed,
	country_code, draw_country_code, present_samples, unit, unit_family, coldkey
`

func scanCtfCensusMember(result server.PgResult) *CtfCensusMember {
	member := &CtfCensusMember{}
	var epoch int64
	var role, reason, presentSamples int16
	var failed int32
	var drawCountryCode *string
	var unit, coldkey []byte
	var unitFamily *int16
	server.Raise(result.Scan(
		&epoch, &role, &member.ClientId, &member.NetworkId, &member.ExtenderId, &member.Candidate, &reason, &failed,
		&member.CountryCode, &drawCountryCode, &presentSamples, &unit, &unitFamily, &coldkey,
	))
	member.Epoch = uint64(epoch)
	member.Role = int(role)
	member.Reason = int(reason)
	member.Failed = uint32(failed)
	if drawCountryCode != nil {
		member.DrawCountryCode = *drawCountryCode
	}
	member.PresentSamples = int(presentSamples)
	if len(unit) == 32 && unitFamily != nil {
		member.Unit = (*[32]byte)(unit)
		member.UnitFamily = int(*unitFamily)
	}
	if len(coldkey) == 32 {
		member.Coldkey = (*[32]byte)(coldkey)
	}
	return member
}

// GetCtfCensusMembers returns every member of an epoch's census, by role and
// client id.
func GetCtfCensusMembers(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfCensusMember {
	key := requireStDeploymentKey(deploymentKey)
	members := []*CtfCensusMember{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfCensusMemberColumns+` FROM ctf_census_member
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY role, client_id`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				members = append(members, scanCtfCensusMember(result))
			}
		})
	})
	return members
}

// GetCtfCensusClientMembers returns one client's members in the censuses of
// epochs fromEpoch … toEpoch, newest epoch first, providers before extenders.
// An epoch and role without a row is one where no sample found the client in
// the role.
func GetCtfCensusClientMembers(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, clientId server.Id, fromEpoch uint64, toEpoch uint64) []*CtfCensusMember {
	key := requireStDeploymentKey(deploymentKey)
	members := []*CtfCensusMember{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfCensusMemberColumns+` FROM ctf_census_member
			WHERE deployment_key = $1 AND no_id = $2 AND client_id = $3 AND $4 <= epoch AND epoch <= $5
			ORDER BY epoch DESC, role`,
			key, int64(noId), clientId, int64(fromEpoch), int64(toEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				members = append(members, scanCtfCensusMember(result))
			}
		})
	})
	return members
}

// GetCtfCensusColdkeys returns the payout coldkey of every candidate of an
// epoch's census, by client id: the coldkeys the draw's winners are paid to
// (sn ctf.EpochInput.Coldkeys). A client that is a candidate in both roles has
// one coldkey.
func GetCtfCensusColdkeys(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) map[server.Id][32]byte {
	key := requireStDeploymentKey(deploymentKey)
	coldkeys := map[server.Id][32]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id, coldkey FROM ctf_census_member
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND candidate
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var coldkey []byte
				server.Raise(result.Scan(&clientId, &coldkey))
				coldkeys[clientId] = [32]byte(coldkey)
			}
		})
	})
	return coldkeys
}

// GetCtfCensusPots returns a census's 498 country and role counts, by country
// and role.
func GetCtfCensusPots(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfCensusPot {
	key := requireStDeploymentKey(deploymentKey)
	pots := []*CtfCensusPot{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT country_code, role, units, candidates, near_misses FROM ctf_census_pot
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY country_code, role
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				pot := &CtfCensusPot{}
				var role int16
				server.Raise(result.Scan(&pot.CountryCode, &role, &pot.Units, &pot.Candidates, &pot.NearMisses))
				pot.Role = int(role)
				pots = append(pots, pot)
			}
		})
	})
	return pots
}

// GetCtfCensusExclusions returns a census's exclusion rows, as its exclusion
// object holds them: by country, role and reason.
func GetCtfCensusExclusions(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfCensusExclusion {
	key := requireStDeploymentKey(deploymentKey)
	exclusions := []*CtfCensusExclusion{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT country_code, role, reason, count FROM ctf_census_exclusion
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY country_code, role, reason
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				exclusion := &CtfCensusExclusion{}
				var role, reason int16
				server.Raise(result.Scan(&exclusion.CountryCode, &role, &reason, &exclusion.Count))
				exclusion.Role = int(role)
				exclusion.Reason = int(reason)
				exclusions = append(exclusions, exclusion)
			}
		})
	})
	return exclusions
}

// RemoveCtfCensusObjectsBefore deletes the stored objects of every census
// before epoch that was published and whose epoch a finalized CTF ledger
// covers. A committed census is a fixed input of every root that covers its
// epoch until one is final (§5), so its objects are kept here, as well as in
// the ledger object store, until then; a census no finalized ledger covers,
// such as a shadow run's, is kept.
func RemoveCtfCensusObjectsBefore(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) (removed int64) {
	key := requireStDeploymentKey(deploymentKey)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM ctf_census_object
			USING ctf_census
			WHERE
				ctf_census_object.deployment_key = $1 AND ctf_census_object.no_id = $2 AND ctf_census_object.epoch < $3 AND
				ctf_census.deployment_key = ctf_census_object.deployment_key AND
				ctf_census.no_id = ctf_census_object.no_id AND
				ctf_census.epoch = ctf_census_object.epoch AND
				ctf_census.published_time IS NOT NULL AND
				EXISTS (
					SELECT 1 FROM ctf_ledger
					WHERE
						ctf_ledger.deployment_key = ctf_census.deployment_key AND
						ctf_ledger.no_id = ctf_census.no_id AND
						ctf_ledger.status = 'finalized' AND
						ctf_ledger.first_epoch <= ctf_census.epoch AND
						ctf_census.epoch <= ctf_ledger.epoch
				)
			`,
			key, int64(noId), int64(epoch),
		)
		server.Raise(err)
		removed = tag.RowsAffected()
	})
	return
}

// RemoveCtfCensusMembersBefore deletes the census members of every epoch
// before epoch. The census, its pots and exclusions stay.
func RemoveCtfCensusMembersBefore(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) (removed int64) {
	key := requireStDeploymentKey(deploymentKey)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`DELETE FROM ctf_census_member WHERE deployment_key = $1 AND no_id = $2 AND epoch < $3`,
			key, int64(noId), int64(epoch),
		)
		server.Raise(err)
		removed = tag.RowsAffected()
	})
	return
}

// ---------------------------------------------------------------------------
// Commitment and draw
// ---------------------------------------------------------------------------

// The commitment's states: built and not sent, sent, seen committed on chain,
// missed (its window passed without one), refused (the chain will never take
// it, as when another hash is committed).
const (
	CtfCommitmentStatusPending   = "pending"
	CtfCommitmentStatusSent      = "sent"
	CtfCommitmentStatusCommitted = "committed"
	CtfCommitmentStatusMissed    = "missed"
	CtfCommitmentStatusRefused   = "refused"
)

// An epoch's census commitment (commitCtfCensus) and its draw round.
type CtfCensusCommitment struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	Epoch         uint64
	CensusHash    [32]byte
	Status        string
	TxHash        string
	// B_k and its EVM timestamp in unix seconds, once committed
	CommitBlock     *uint64
	CommitBlockTime *uint64
	// ρ_k (sn ctf.DrawRound(CommitBlockTime))
	Round      *uint64
	CreateTime time.Time
	UpdateTime time.Time
}

func ctfUint64OrNil(value *uint64) *int64 {
	if value == nil {
		return nil
	}
	v := int64(*value)
	return &v
}

func ctfUint64FromNil(value *int64) *uint64 {
	if value == nil {
		return nil
	}
	v := uint64(*value)
	return &v
}

// SetCtfCensusCommitment writes an epoch's commitment, or updates its status,
// transaction, block, block time and round. A commitment of another census
// hash for the epoch is refused: false.
func SetCtfCensusCommitment(ctx context.Context, commitment *CtfCensusCommitment) (set bool) {
	key := requireStDeploymentKey(commitment.DeploymentKey)
	var txHash *string
	if commitment.TxHash != "" {
		txHash = &commitment.TxHash
	}
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_census_commitment (
				deployment_key, no_id, epoch, census_hash, status, tx_hash,
				commit_block, commit_block_time, round, create_time, update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
			ON CONFLICT (deployment_key, no_id, epoch) DO UPDATE
			SET
				status = EXCLUDED.status,
				tx_hash = COALESCE(EXCLUDED.tx_hash, ctf_census_commitment.tx_hash),
				commit_block = COALESCE(EXCLUDED.commit_block, ctf_census_commitment.commit_block),
				commit_block_time = COALESCE(EXCLUDED.commit_block_time, ctf_census_commitment.commit_block_time),
				round = COALESCE(EXCLUDED.round, ctf_census_commitment.round),
				update_time = EXCLUDED.update_time
			WHERE ctf_census_commitment.census_hash = EXCLUDED.census_hash
			`,
			key, int64(commitment.NoId), int64(commitment.Epoch), commitment.CensusHash[:], commitment.Status, txHash,
			ctfUint64OrNil(commitment.CommitBlock), ctfUint64OrNil(commitment.CommitBlockTime), ctfUint64OrNil(commitment.Round), now,
		)
		server.Raise(err)
		set = tag.RowsAffected() == 1
	})
	return
}

const ctfCensusCommitmentColumns = `epoch, census_hash, status, tx_hash, commit_block, commit_block_time, round, create_time, update_time`

func scanCtfCensusCommitment(result server.PgResult, commitment *CtfCensusCommitment) {
	var epoch int64
	var censusHash []byte
	var txHash *string
	var commitBlock, commitBlockTime, round *int64
	server.Raise(result.Scan(&epoch, &censusHash, &commitment.Status, &txHash, &commitBlock, &commitBlockTime, &round, &commitment.CreateTime, &commitment.UpdateTime))
	commitment.Epoch = uint64(epoch)
	commitment.CensusHash = [32]byte(censusHash)
	if txHash != nil {
		commitment.TxHash = *txHash
	}
	commitment.CommitBlock = ctfUint64FromNil(commitBlock)
	commitment.CommitBlockTime = ctfUint64FromNil(commitBlockTime)
	commitment.Round = ctfUint64FromNil(round)
}

// GetCtfCensusCommitment returns an epoch's commitment, or nil.
func GetCtfCensusCommitment(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *CtfCensusCommitment {
	key := requireStDeploymentKey(deploymentKey)
	var commitment *CtfCensusCommitment
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfCensusCommitmentColumns+` FROM ctf_census_commitment WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				commitment = &CtfCensusCommitment{DeploymentKey: deploymentKey, NoId: noId}
				scanCtfCensusCommitment(result, commitment)
			}
		})
	})
	return commitment
}

// GetCtfCensusCommitments returns the commitments of every epoch from
// fromEpoch, by epoch.
func GetCtfCensusCommitments(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, fromEpoch uint64) []*CtfCensusCommitment {
	key := requireStDeploymentKey(deploymentKey)
	commitments := []*CtfCensusCommitment{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfCensusCommitmentColumns+` FROM ctf_census_commitment
			WHERE deployment_key = $1 AND no_id = $2 AND $3 <= epoch
			ORDER BY epoch`,
			key, int64(noId), int64(fromEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				commitment := &CtfCensusCommitment{DeploymentKey: deploymentKey, NoId: noId}
				scanCtfCensusCommitment(result, commitment)
				commitments = append(commitments, commitment)
			}
		})
	})
	return commitments
}

// An epoch's draw (§4.4): the verified quicknet pulse of its round and the
// seed it gives.
type CtfDraw struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	Epoch         uint64
	CensusHash    [32]byte
	Round         uint64
	// the pulse's 48-byte BLS signature
	Signature  []byte
	Randomness [32]byte
	Seed       [32]byte
	CreateTime time.Time
}

// CreateCtfDraw writes an epoch's draw once. False when the epoch has one.
func CreateCtfDraw(ctx context.Context, draw *CtfDraw) (created bool) {
	key := requireStDeploymentKey(draw.DeploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_draw (deployment_key, no_id, epoch, census_hash, round, signature, randomness, seed, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
			`,
			key, int64(draw.NoId), int64(draw.Epoch), draw.CensusHash[:], int64(draw.Round), draw.Signature, draw.Randomness[:], draw.Seed[:], server.NowUtc(),
		)
		server.Raise(err)
		created = tag.RowsAffected() == 1
	})
	return
}

// GetCtfDraw returns an epoch's draw, or nil.
func GetCtfDraw(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *CtfDraw {
	key := requireStDeploymentKey(deploymentKey)
	var draw *CtfDraw
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT census_hash, round, signature, randomness, seed, create_time FROM ctf_draw
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				draw = &CtfDraw{DeploymentKey: deploymentKey, NoId: noId, Epoch: epoch}
				var censusHash, randomness, seed []byte
				var round int64
				server.Raise(result.Scan(&censusHash, &round, &draw.Signature, &randomness, &seed, &draw.CreateTime))
				draw.CensusHash = [32]byte(censusHash)
				draw.Round = uint64(round)
				draw.Randomness = [32]byte(randomness)
				draw.Seed = [32]byte(seed)
			}
		})
	})
	return draw
}

// ---------------------------------------------------------------------------
// Each covered epoch's result
// ---------------------------------------------------------------------------

// What a covered epoch fixed at the first live ledger that covered it (§4.5,
// §5; sn ctf.EpochResult): its amounts and the references of its award, pot
// and pending objects. The api reads them; Report keeps the driver's whole
// serialization for anything not in a column.
type CtfEpochResult struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	Epoch         uint64
	// the ledger that fixed it: its root epoch e and ledger hash
	LedgerEpoch uint64
	LedgerHash  [32]byte
	// the committed census and its block, nil when none was committed
	CensusHash  *[32]byte
	CensusBlock *uint64
	// why a committed census gave no draw, empty otherwise
	CensusError string
	Drawn       bool
	// ρ_k, 0 without a draw
	Round      uint64
	Candidates uint64
	// f_k, the carry contract's recycledFor[k], X_k, R_k and the carry
	// credited
	Funding  uint256.Int
	Recycled uint256.Int
	Owed     uint256.Int
	Returned uint256.Int
	Credited uint256.Int
	// M_k
	Matured bool
	// F_k, any shortfall taken from the pots, and the slot amount
	// floor(F_k / 2490) with its remainder
	Fresh         uint256.Int
	Shortfall     uint256.Int
	SlotBase      uint256.Int
	SlotRemainder int
	// the award rows
	Awards       int
	PaidSlots    uint256.Int
	PaidPots     uint256.Int
	PaidPending  uint256.Int
	PendingTotal uint256.Int
	// Σ pot out
	Carried    uint256.Int
	AwardsRef  [32]byte
	PotsRef    [32]byte
	PendingRef [32]byte
	Report     []byte
	CreateTime time.Time
}

// One award row (§5; sn ctf.Award).
type CtfAward struct {
	Epoch       uint64
	Role        int
	CountryCode string
	Rank        int
	Unit        [32]byte
	// 4 or 6, 0 when unknown
	UnitFamily int
	ClientId   server.Id
	Coldkey    [32]byte
	Slot       uint256.Int
	Pot        uint256.Int
	// the pot share is a pending award
	Deferred bool
}

// One pot row (§5; sn ctf.Pot).
type CtfPot struct {
	Epoch       uint64
	CountryCode string
	Role        int
	Units       int
	Winners     int
	In          uint256.Int
	Out         uint256.Int
}

// One pending award outstanding after an epoch (§5; sn ctf.PendingAward).
type CtfPendingAward struct {
	Epoch       uint64
	AwardEpoch  uint64
	Role        int
	CountryCode string
	Rank        int
	Coldkey     [32]byte
	Amount      uint256.Int
}

func ctfDecimals(values []uint256.Int) []string {
	decimals := make([]string, len(values))
	for i := range values {
		decimals[i] = values[i].Dec()
	}
	return decimals
}

// CreateCtfEpochResult writes what a covered epoch fixed, with its awards,
// pots and pending awards, all or nothing, once: an epoch's census, draw and
// awards are fixed inputs (§5). False when the epoch has a result, in which
// case nothing is written.
func CreateCtfEpochResult(ctx context.Context, result *CtfEpochResult, awards []*CtfAward, pots []*CtfPot, pending []*CtfPendingAward) (created bool) {
	key := requireStDeploymentKey(result.DeploymentKey)
	var censusHash []byte
	if result.CensusHash != nil {
		censusHash = result.CensusHash[:]
	}
	var censusError *string
	if result.CensusError != "" {
		censusError = &result.CensusError
	}
	var report *string
	if len(result.Report) != 0 {
		text := string(result.Report)
		report = &text
	}
	server.Tx(ctx, func(tx server.PgTx) {
		created = false
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_epoch (
				deployment_key, no_id, epoch, ledger_epoch, ledger_hash, census_hash, census_block, census_error,
				drawn, round, candidates, funding, recycled, owed, returned, credited, matured,
				fresh, shortfall, slot_base, slot_remainder, awards,
				paid_slots, paid_pots, paid_pending, pending_total, carried,
				awards_ref, pots_ref, pending_ref, report, create_time
			)
			VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12::numeric, $13::numeric, $14::numeric, $15::numeric, $16::numeric, $17,
				$18::numeric, $19::numeric, $20::numeric, $21, $22,
				$23::numeric, $24::numeric, $25::numeric, $26::numeric, $27::numeric,
				$28, $29, $30, $31::jsonb, $32
			)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
			`,
			key, int64(result.NoId), int64(result.Epoch), int64(result.LedgerEpoch), result.LedgerHash[:], censusHash, ctfUint64OrNil(result.CensusBlock), censusError,
			result.Drawn, int64(result.Round), int64(result.Candidates),
			result.Funding.Dec(), result.Recycled.Dec(), result.Owed.Dec(), result.Returned.Dec(), result.Credited.Dec(), result.Matured,
			result.Fresh.Dec(), result.Shortfall.Dec(), result.SlotBase.Dec(), result.SlotRemainder, result.Awards,
			result.PaidSlots.Dec(), result.PaidPots.Dec(), result.PaidPending.Dec(), result.PendingTotal.Dec(), result.Carried.Dec(),
			result.AwardsRef[:], result.PotsRef[:], result.PendingRef[:], report, server.NowUtc(),
		)
		server.Raise(err)
		if tag.RowsAffected() == 0 {
			return
		}
		created = true

		if 0 < len(awards) {
			roles := make([]int16, len(awards))
			countryCodes := make([]string, len(awards))
			ranks := make([]int16, len(awards))
			units := make([][]byte, len(awards))
			unitFamilies := make([]int16, len(awards))
			clientIds := make([]server.Id, len(awards))
			coldkeys := make([][]byte, len(awards))
			slots := make([]uint256.Int, len(awards))
			potShares := make([]uint256.Int, len(awards))
			deferreds := make([]bool, len(awards))
			for i, award := range awards {
				roles[i] = int16(award.Role)
				countryCodes[i] = award.CountryCode
				ranks[i] = int16(award.Rank)
				units[i] = award.Unit[:]
				unitFamilies[i] = int16(award.UnitFamily)
				clientIds[i] = award.ClientId
				coldkeys[i] = award.Coldkey[:]
				slots[i] = award.Slot
				potShares[i] = award.Pot
				deferreds[i] = award.Deferred
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO ctf_award (
					deployment_key, no_id, epoch, role, country_code, rank, unit, unit_family,
					client_id, coldkey, slot_rao, pot_rao, deferred
				)
				SELECT
					$1::varchar, $2::bigint, $3::bigint, award.role, award.country_code, award.rank, award.unit,
					NULLIF(award.unit_family, 0), award.client_id, award.coldkey,
					award.slot_rao::numeric, award.pot_rao::numeric, award.deferred
				FROM unnest(
					$4::smallint[], $5::varchar[], $6::smallint[], $7::bytea[], $8::smallint[],
					$9::uuid[], $10::bytea[], $11::text[], $12::text[], $13::boolean[]
				) AS award (role, country_code, rank, unit, unit_family, client_id, coldkey, slot_rao, pot_rao, deferred)
				`,
				key, int64(result.NoId), int64(result.Epoch),
				roles, countryCodes, ranks, units, unitFamilies, clientIds, coldkeys, ctfDecimals(slots), ctfDecimals(potShares), deferreds,
			))
		}

		if 0 < len(pots) {
			countryCodes := make([]string, len(pots))
			roles := make([]int16, len(pots))
			units := make([]int32, len(pots))
			winners := make([]int16, len(pots))
			ins := make([]uint256.Int, len(pots))
			outs := make([]uint256.Int, len(pots))
			for i, pot := range pots {
				countryCodes[i] = pot.CountryCode
				roles[i] = int16(pot.Role)
				units[i] = int32(pot.Units)
				winners[i] = int16(pot.Winners)
				ins[i] = pot.In
				outs[i] = pot.Out
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO ctf_pot (deployment_key, no_id, epoch, country_code, role, units, winners, pot_in, pot_out)
				SELECT
					$1::varchar, $2::bigint, $3::bigint, pot.country_code, pot.role, pot.units, pot.winners,
					pot.pot_in::numeric, pot.pot_out::numeric
				FROM unnest($4::varchar[], $5::smallint[], $6::integer[], $7::smallint[], $8::text[], $9::text[])
					AS pot (country_code, role, units, winners, pot_in, pot_out)
				`,
				key, int64(result.NoId), int64(result.Epoch), countryCodes, roles, units, winners, ctfDecimals(ins), ctfDecimals(outs),
			))
		}

		if 0 < len(pending) {
			awardEpochs := make([]int64, len(pending))
			roles := make([]int16, len(pending))
			countryCodes := make([]string, len(pending))
			ranks := make([]int16, len(pending))
			coldkeys := make([][]byte, len(pending))
			amounts := make([]uint256.Int, len(pending))
			for i, award := range pending {
				awardEpochs[i] = int64(award.AwardEpoch)
				roles[i] = int16(award.Role)
				countryCodes[i] = award.CountryCode
				ranks[i] = int16(award.Rank)
				coldkeys[i] = award.Coldkey[:]
				amounts[i] = award.Amount
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO ctf_pending_award (deployment_key, no_id, epoch, award_epoch, role, country_code, rank, coldkey, amount)
				SELECT
					$1::varchar, $2::bigint, $3::bigint, pending.award_epoch, pending.role, pending.country_code,
					pending.rank, pending.coldkey, pending.amount::numeric
				FROM unnest($4::bigint[], $5::smallint[], $6::varchar[], $7::smallint[], $8::bytea[], $9::text[])
					AS pending (award_epoch, role, country_code, rank, coldkey, amount)
				`,
				key, int64(result.NoId), int64(result.Epoch), awardEpochs, roles, countryCodes, ranks, coldkeys, ctfDecimals(amounts),
			))
		}
	})
	return
}

// RemoveCtfEpochResult deletes what an epoch fixed. Only the operator's
// explicit command to rebuild the epoch may call it (§5 "Fixed inputs").
func RemoveCtfEpochResult(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		for _, table := range []string{"ctf_award", "ctf_pot", "ctf_pending_award", "ctf_epoch"} {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM `+table+` WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3`,
				key, int64(noId), int64(epoch),
			))
		}
	})
}

const ctfEpochResultColumns = `
	epoch, ledger_epoch, ledger_hash, census_hash, census_block, census_error, drawn, round, candidates,
	funding::text, recycled::text, owed::text, returned::text, credited::text, matured,
	fresh::text, shortfall::text, slot_base::text, slot_remainder, awards,
	paid_slots::text, paid_pots::text, paid_pending::text, pending_total::text, carried::text,
	awards_ref, pots_ref, pending_ref, report::text, create_time
`

func scanCtfEpochResult(result server.PgResult, epochResult *CtfEpochResult) {
	var epoch, ledgerEpoch, round, candidates int64
	var ledgerHash, censusHash, awardsRef, potsRef, pendingRef []byte
	var censusBlock *int64
	var censusError, report *string
	var funding, recycled, owed, returned, credited, fresh, shortfall, slotBase string
	var paidSlots, paidPots, paidPending, pendingTotal, carried string
	server.Raise(result.Scan(
		&epoch, &ledgerEpoch, &ledgerHash, &censusHash, &censusBlock, &censusError, &epochResult.Drawn, &round, &candidates,
		&funding, &recycled, &owed, &returned, &credited, &epochResult.Matured,
		&fresh, &shortfall, &slotBase, &epochResult.SlotRemainder, &epochResult.Awards,
		&paidSlots, &paidPots, &paidPending, &pendingTotal, &carried,
		&awardsRef, &potsRef, &pendingRef, &report, &epochResult.CreateTime,
	))
	epochResult.Epoch = uint64(epoch)
	epochResult.LedgerEpoch = uint64(ledgerEpoch)
	epochResult.LedgerHash = [32]byte(ledgerHash)
	if len(censusHash) == 32 {
		epochResult.CensusHash = (*[32]byte)(censusHash)
	}
	epochResult.CensusBlock = ctfUint64FromNil(censusBlock)
	if censusError != nil {
		epochResult.CensusError = *censusError
	}
	epochResult.Round = uint64(round)
	epochResult.Candidates = uint64(candidates)
	for _, amount := range []struct {
		value *uint256.Int
		text  string
	}{
		{&epochResult.Funding, funding}, {&epochResult.Recycled, recycled}, {&epochResult.Owed, owed},
		{&epochResult.Returned, returned}, {&epochResult.Credited, credited}, {&epochResult.Fresh, fresh},
		{&epochResult.Shortfall, shortfall}, {&epochResult.SlotBase, slotBase}, {&epochResult.PaidSlots, paidSlots},
		{&epochResult.PaidPots, paidPots}, {&epochResult.PaidPending, paidPending},
		{&epochResult.PendingTotal, pendingTotal}, {&epochResult.Carried, carried},
	} {
		server.Raise(amount.value.SetFromDecimal(amount.text))
	}
	epochResult.AwardsRef = [32]byte(awardsRef)
	epochResult.PotsRef = [32]byte(potsRef)
	epochResult.PendingRef = [32]byte(pendingRef)
	if report != nil {
		epochResult.Report = []byte(*report)
	}
}

// GetCtfEpochResult returns what an epoch fixed, or nil before a live ledger
// covers it.
func GetCtfEpochResult(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *CtfEpochResult {
	results := GetCtfEpochResults(ctx, deploymentKey, noId, epoch, epoch)
	if len(results) == 0 {
		return nil
	}
	return results[0]
}

// GetCtfEpochResults returns what the epochs fromEpoch … toEpoch fixed, by
// epoch.
func GetCtfEpochResults(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, fromEpoch uint64, toEpoch uint64) []*CtfEpochResult {
	key := requireStDeploymentKey(deploymentKey)
	results := []*CtfEpochResult{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfEpochResultColumns+` FROM ctf_epoch
			WHERE deployment_key = $1 AND no_id = $2 AND $3 <= epoch AND epoch <= $4
			ORDER BY epoch`,
			key, int64(noId), int64(fromEpoch), int64(toEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				epochResult := &CtfEpochResult{DeploymentKey: deploymentKey, NoId: noId}
				scanCtfEpochResult(result, epochResult)
				results = append(results, epochResult)
			}
		})
	})
	return results
}

const ctfAwardColumns = `epoch, role, country_code, rank, unit, unit_family, client_id, coldkey, slot_rao::text, pot_rao::text, deferred`

func scanCtfAward(result server.PgResult) *CtfAward {
	award := &CtfAward{}
	var epoch int64
	var role, rank int16
	var unit, coldkey []byte
	var unitFamily *int16
	var slot, pot string
	server.Raise(result.Scan(&epoch, &role, &award.CountryCode, &rank, &unit, &unitFamily, &award.ClientId, &coldkey, &slot, &pot, &award.Deferred))
	award.Epoch = uint64(epoch)
	award.Role = int(role)
	award.Rank = int(rank)
	award.Unit = [32]byte(unit)
	if unitFamily != nil {
		award.UnitFamily = int(*unitFamily)
	}
	award.Coldkey = [32]byte(coldkey)
	server.Raise(award.Slot.SetFromDecimal(slot))
	server.Raise(award.Pot.SetFromDecimal(pot))
	return award
}

// GetCtfAwards returns an epoch's awards, by role, country and rank.
func GetCtfAwards(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfAward {
	key := requireStDeploymentKey(deploymentKey)
	awards := []*CtfAward{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfAwardColumns+` FROM ctf_award
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY role, country_code, rank`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				awards = append(awards, scanCtfAward(result))
			}
		})
	})
	return awards
}

// GetCtfClientAwards returns one client's awards of the epochs from fromEpoch,
// newest first.
func GetCtfClientAwards(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, clientId server.Id, fromEpoch uint64) []*CtfAward {
	key := requireStDeploymentKey(deploymentKey)
	awards := []*CtfAward{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfAwardColumns+` FROM ctf_award
			WHERE deployment_key = $1 AND no_id = $2 AND client_id = $3 AND $4 <= epoch
			ORDER BY epoch DESC, role`,
			key, int64(noId), clientId, int64(fromEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				awards = append(awards, scanCtfAward(result))
			}
		})
	})
	return awards
}

const ctfPotColumns = `epoch, country_code, role, units, winners, pot_in::text, pot_out::text`

func scanCtfPot(result server.PgResult) *CtfPot {
	pot := &CtfPot{}
	var epoch int64
	var role, winners int16
	var in, out string
	server.Raise(result.Scan(&epoch, &pot.CountryCode, &role, &pot.Units, &winners, &in, &out))
	pot.Epoch = uint64(epoch)
	pot.Role = int(role)
	pot.Winners = int(winners)
	server.Raise(pot.In.SetFromDecimal(in))
	server.Raise(pot.Out.SetFromDecimal(out))
	return pot
}

// GetCtfPots returns an epoch's 498 pots, by country and role.
func GetCtfPots(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfPot {
	key := requireStDeploymentKey(deploymentKey)
	pots := []*CtfPot{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfPotColumns+` FROM ctf_pot
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY country_code, role`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				pots = append(pots, scanCtfPot(result))
			}
		})
	})
	return pots
}

// GetCtfCountryPots returns one country's pots of the epochs from fromEpoch,
// newest first, providers before extenders.
func GetCtfCountryPots(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, countryCode string, fromEpoch uint64) []*CtfPot {
	key := requireStDeploymentKey(deploymentKey)
	pots := []*CtfPot{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfPotColumns+` FROM ctf_pot
			WHERE deployment_key = $1 AND no_id = $2 AND country_code = $3 AND $4 <= epoch
			ORDER BY epoch DESC, role`,
			key, int64(noId), countryCode, int64(fromEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				pots = append(pots, scanCtfPot(result))
			}
		})
	})
	return pots
}

// GetCtfPendingAwards returns the pending awards outstanding after an epoch,
// by award epoch, role, country and rank.
func GetCtfPendingAwards(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfPendingAward {
	key := requireStDeploymentKey(deploymentKey)
	pending := []*CtfPendingAward{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT award_epoch, role, country_code, rank, coldkey, amount::text FROM ctf_pending_award
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY award_epoch, role, country_code, rank
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				award := &CtfPendingAward{Epoch: epoch}
				var awardEpoch int64
				var role, rank int16
				var coldkey []byte
				var amount string
				server.Raise(result.Scan(&awardEpoch, &role, &award.CountryCode, &rank, &coldkey, &amount))
				award.AwardEpoch = uint64(awardEpoch)
				award.Role = int(role)
				award.Rank = int(rank)
				award.Coldkey = [32]byte(coldkey)
				server.Raise(award.Amount.SetFromDecimal(amount))
				pending = append(pending, award)
			}
		})
	})
	return pending
}

// ---------------------------------------------------------------------------
// Ledgers
// ---------------------------------------------------------------------------

// A ledger's states: built and not committed, committed (pending in vault2),
// finalized, replaced by a later commit, deleted by a fault, refused (the
// auditor or the chain will never take it), and shadow, a shadow run's, which
// is never committed.
const (
	CtfLedgerStatusBuilt     = "built"
	CtfLedgerStatusCommitted = "committed"
	CtfLedgerStatusFinalized = "finalized"
	CtfLedgerStatusReplaced  = "replaced"
	CtfLedgerStatusDeleted   = "deleted"
	CtfLedgerStatusRefused   = "refused"
	CtfLedgerStatusShadow    = "shadow"
)

// One CTF ledger the driver built (§5): a live root's, or a shadow run's.
type CtfLedger struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	// the root epoch e, and the first epoch it covers
	Epoch      uint64
	FirstEpoch uint64
	LedgerHash [32]byte
	Shadow     bool
	Root       [32]byte
	Total      uint256.Int
	LeafCount  uint64
	Header     []byte
	// the driver's report of the build, JSON
	Report        []byte
	Status        string
	Seq           *uint64
	RootIndex     *uint64
	CommitBlock   *uint64
	FinalizeBlock *uint64
	CreateTime    time.Time
	UpdateTime    time.Time
}

// CreateCtfLedger writes a ledger once. False when it exists.
func CreateCtfLedger(ctx context.Context, ledger *CtfLedger) (created bool) {
	key := requireStDeploymentKey(ledger.DeploymentKey)
	var report *string
	if len(ledger.Report) != 0 {
		text := string(ledger.Report)
		report = &text
	}
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_ledger (
				deployment_key, no_id, epoch, ledger_hash, shadow, first_epoch, root, total, leaf_count,
				header, report, status, seq, root_index, commit_block, finalize_block, create_time, update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11::jsonb, $12, $13, $14, $15, $16, $17, $17)
			ON CONFLICT (deployment_key, no_id, epoch, ledger_hash) DO NOTHING
			`,
			key, int64(ledger.NoId), int64(ledger.Epoch), ledger.LedgerHash[:], ledger.Shadow, int64(ledger.FirstEpoch),
			ledger.Root[:], ledger.Total.Dec(), int64(ledger.LeafCount), ledger.Header, report, ledger.Status,
			ctfUint64OrNil(ledger.Seq), ctfUint64OrNil(ledger.RootIndex), ctfUint64OrNil(ledger.CommitBlock), ctfUint64OrNil(ledger.FinalizeBlock), now,
		)
		server.Raise(err)
		created = tag.RowsAffected() == 1
	})
	return
}

// A ledger's change of state: its status, and whichever of its chain facts are
// known now. A nil fact keeps the one recorded.
type CtfLedgerStatusUpdate struct {
	Status        string
	Seq           *uint64
	RootIndex     *uint64
	CommitBlock   *uint64
	FinalizeBlock *uint64
}

// SetCtfLedgerStatus records a ledger's change of state. False when no such
// ledger exists.
func SetCtfLedgerStatus(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, ledgerHash [32]byte, update *CtfLedgerStatusUpdate) (set bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			UPDATE ctf_ledger
			SET
				status = $5,
				seq = COALESCE($6, seq),
				root_index = COALESCE($7, root_index),
				commit_block = COALESCE($8, commit_block),
				finalize_block = COALESCE($9, finalize_block),
				update_time = $10
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND ledger_hash = $4
			`,
			key, int64(noId), int64(epoch), ledgerHash[:], update.Status,
			ctfUint64OrNil(update.Seq), ctfUint64OrNil(update.RootIndex), ctfUint64OrNil(update.CommitBlock), ctfUint64OrNil(update.FinalizeBlock),
			server.NowUtc(),
		)
		server.Raise(err)
		set = tag.RowsAffected() == 1
	})
	return
}

const ctfLedgerColumns = `
	epoch, first_epoch, ledger_hash, shadow, root, total::text, leaf_count, header, report::text, status,
	seq, root_index, commit_block, finalize_block, create_time, update_time
`

func scanCtfLedger(result server.PgResult, ledger *CtfLedger) {
	var epoch, firstEpoch, leafCount int64
	var ledgerHash, root []byte
	var total string
	var report *string
	var seq, rootIndex, commitBlock, finalizeBlock *int64
	server.Raise(result.Scan(
		&epoch, &firstEpoch, &ledgerHash, &ledger.Shadow, &root, &total, &leafCount, &ledger.Header, &report, &ledger.Status,
		&seq, &rootIndex, &commitBlock, &finalizeBlock, &ledger.CreateTime, &ledger.UpdateTime,
	))
	ledger.Epoch = uint64(epoch)
	ledger.FirstEpoch = uint64(firstEpoch)
	ledger.LedgerHash = [32]byte(ledgerHash)
	ledger.Root = [32]byte(root)
	server.Raise(ledger.Total.SetFromDecimal(total))
	ledger.LeafCount = uint64(leafCount)
	if report != nil {
		ledger.Report = []byte(*report)
	}
	ledger.Seq = ctfUint64FromNil(seq)
	ledger.RootIndex = ctfUint64FromNil(rootIndex)
	ledger.CommitBlock = ctfUint64FromNil(commitBlock)
	ledger.FinalizeBlock = ctfUint64FromNil(finalizeBlock)
}

// GetCtfLedger returns one ledger, or nil.
func GetCtfLedger(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, ledgerHash [32]byte) *CtfLedger {
	key := requireStDeploymentKey(deploymentKey)
	var ledger *CtfLedger
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfLedgerColumns+` FROM ctf_ledger
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND ledger_hash = $4`,
			key, int64(noId), int64(epoch), ledgerHash[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				ledger = &CtfLedger{DeploymentKey: deploymentKey, NoId: noId}
				scanCtfLedger(result, ledger)
			}
		})
	})
	return ledger
}

// GetCtfLedgers returns the ledgers of root epochs from fromEpoch, by epoch
// and creation.
func GetCtfLedgers(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, fromEpoch uint64) []*CtfLedger {
	key := requireStDeploymentKey(deploymentKey)
	ledgers := []*CtfLedger{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+ctfLedgerColumns+` FROM ctf_ledger
			WHERE deployment_key = $1 AND no_id = $2 AND $3 <= epoch
			ORDER BY epoch, create_time, ledger_hash`,
			key, int64(noId), int64(fromEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				ledger := &CtfLedger{DeploymentKey: deploymentKey, NoId: noId}
				scanCtfLedger(result, ledger)
				ledgers = append(ledgers, ledger)
			}
		})
	})
	return ledgers
}

// ---------------------------------------------------------------------------
// The carry contract's returns
// ---------------------------------------------------------------------------

// One Recycled(epoch, amount) log of the carry contract (§4.6).
type CtfRecycle struct {
	BlockNumber uint64
	LogIndex    int
	Epoch       uint64
	Amount      uint256.Int
	TxHash      string
}

// AddCtfRecycles mirrors Recycled logs. A log already mirrored is left as it
// is.
func AddCtfRecycles(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, recycles []*CtfRecycle) {
	key := requireStDeploymentKey(deploymentKey)
	if len(recycles) == 0 {
		return
	}
	blocks := make([]int64, len(recycles))
	logIndexes := make([]int32, len(recycles))
	epochs := make([]int64, len(recycles))
	amounts := make([]uint256.Int, len(recycles))
	txHashes := make([]string, len(recycles))
	for i, recycle := range recycles {
		blocks[i] = int64(recycle.BlockNumber)
		logIndexes[i] = int32(recycle.LogIndex)
		epochs[i] = int64(recycle.Epoch)
		amounts[i] = recycle.Amount
		txHashes[i] = recycle.TxHash
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO ctf_recycle (deployment_key, no_id, block_number, log_index, epoch, amount, tx_hash)
			SELECT $1::varchar, $2::bigint, recycle.block_number, recycle.log_index, recycle.epoch, recycle.amount::numeric, recycle.tx_hash
			FROM unnest($3::bigint[], $4::integer[], $5::bigint[], $6::text[], $7::varchar[])
				AS recycle (block_number, log_index, epoch, amount, tx_hash)
			ON CONFLICT (deployment_key, no_id, block_number, log_index) DO NOTHING
			`,
			key, int64(noId), blocks, logIndexes, epochs, ctfDecimals(amounts), txHashes,
		))
	})
}

// GetCtfRecycledFor returns the carry contract's recycledFor of the epochs
// fromEpoch … toEpoch that have any: the sum of their mirrored Recycled logs.
func GetCtfRecycledFor(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, fromEpoch uint64, toEpoch uint64) map[uint64]uint256.Int {
	key := requireStDeploymentKey(deploymentKey)
	recycled := map[uint64]uint256.Int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT epoch, SUM(amount)::text FROM ctf_recycle
			WHERE deployment_key = $1 AND no_id = $2 AND $3 <= epoch AND epoch <= $4
			GROUP BY epoch
			`,
			key, int64(noId), int64(fromEpoch), int64(toEpoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var epoch int64
				var text string
				server.Raise(result.Scan(&epoch, &text))
				var amount uint256.Int
				server.Raise(amount.SetFromDecimal(text))
				recycled[uint64(epoch)] = amount
			}
		})
	})
	return recycled
}

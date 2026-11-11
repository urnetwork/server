package model

// Capture the flag's presence (sn CAPTURETHEFLAG.md §4.2, ctf_schema.go).
//
// The operator samples every provider and extender of its pool each quarter
// hour, 672 times an epoch. A sample records which miners it found present, P1
// for a provider and X1 with a passing challenge for an extender, and the
// country each was found in. The census then requires a candidate to have been
// present in its country in at least half of the epoch's samples the operator
// does not excuse.
//
// Samples are placed by block, not by clock, so every epoch has exactly 672 of
// them whatever its block times: sample i of an epoch of T blocks starting at
// block s covers the blocks [s + floor(i·T/672), s + floor((i+1)·T/672)), which
// is 75 blocks on mainnet. A sample that was never taken counts as a sample
// that found nobody, unless a declared outage excuses it.
//
// The provider half reads the score export's Online decisions, which the
// export publishes for it (CtfPresenceProviders): P1 is exactly what the export
// decided, an active, top-level, connected provider with a Public provide key,
// a valid location and ClientScore.Online. The extender half reads the extender
// tables (controller/ctf_presence.go).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The roles of a census row (sn ctf.RoleProvider, ctf.RoleExtender).
const (
	CtfRoleProvider = 1
	CtfRoleExtender = 2
)

// The presence samples of an epoch, one per quarter hour (sn
// ctf.SamplesPerEpoch).
const CtfSamplesPerEpoch = 672

// The country code of a miner without one, or with one outside the 249 codes
// (sn ctf.CountryUnknown).
const CtfCountryUnknown = "zz"

// CtfSampleSet is a set of an epoch's sample indexes: bit i, the bit i%8 from
// the least significant of byte i/8, is sample i. It is the layout of
// ctf_presence.samples and of PostgreSQL's set_bit and get_bit on bytea.
type CtfSampleSet [CtfSamplesPerEpoch / 8]byte

// Has reports whether sample i is in the set.
func (self *CtfSampleSet) Has(i int) bool {
	return 0 <= i && i < CtfSamplesPerEpoch && self[i/8]&(1<<(i%8)) != 0
}

// Set adds sample i to the set.
func (self *CtfSampleSet) Set(i int) {
	if i < 0 || CtfSamplesPerEpoch <= i {
		panic(fmt.Errorf("ctf sample %d is not one of an epoch's %d", i, CtfSamplesPerEpoch))
	}
	self[i/8] |= 1 << (i % 8)
}

// Count is the number of samples in the set.
func (self *CtfSampleSet) Count() int {
	count := 0
	for _, b := range self {
		count += bits.OnesCount8(b)
	}
	return count
}

// CountExcept is the number of samples in the set that are not in excused.
func (self *CtfSampleSet) CountExcept(excused *CtfSampleSet) int {
	count := 0
	for i, b := range self {
		count += bits.OnesCount8(b &^ excused[i])
	}
	return count
}

// Indexes lists the samples in the set, ascending.
func (self *CtfSampleSet) Indexes() []int {
	indexes := []int{}
	for i := range CtfSamplesPerEpoch {
		if self.Has(i) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

// CtfSampleIndex is the sample of an epoch of epochBlocks blocks starting at
// startBlock that covers block: floor((block − startBlock)·672 / epochBlocks).
// False for a block outside the epoch.
func CtfSampleIndex(startBlock uint64, epochBlocks uint64, block uint64) (int, bool) {
	if epochBlocks == 0 || block < startBlock || block-startBlock >= epochBlocks {
		return 0, false
	}
	hi, lo := bits.Mul64(block-startBlock, CtfSamplesPerEpoch)
	index, _ := bits.Div64(hi, lo, epochBlocks)
	return int(index), true
}

// CtfSampleFirstBlock is the first block sample i covers. A sample of an epoch
// shorter than 672 blocks may cover none, and is then never taken.
func CtfSampleFirstBlock(startBlock uint64, epochBlocks uint64, i int) uint64 {
	hi, lo := bits.Mul64(uint64(i), epochBlocks)
	offset, _ := bits.Div64(hi, lo, CtfSamplesPerEpoch)
	return startBlock + offset
}

// CtfRequiredSamples is the presence a candidate needs (§4.2): half of the
// epoch's samples the operator did not excuse, rounded up.
func CtfRequiredSamples(excused int) int {
	if excused < 0 || CtfSamplesPerEpoch < excused {
		panic(fmt.Errorf("ctf excused samples %d out of range", excused))
	}
	return (CtfSamplesPerEpoch - excused + 1) / 2
}

// CtfNormalizeCountryCode is a country code as presence and the census store
// it: two lowercase letters, or CtfCountryUnknown for anything else.
func CtfNormalizeCountryCode(countryCode string) string {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	if len(countryCode) != 2 || countryCode[0] < 'a' || 'z' < countryCode[0] || countryCode[1] < 'a' || 'z' < countryCode[1] {
		return CtfCountryUnknown
	}
	return countryCode
}

// ---------------------------------------------------------------------------
// The score export's Online set
// ---------------------------------------------------------------------------

// The Redis key the score export writes its Online set to. One key, so its
// hash tag spreads nothing.
const ctfPresenceProvidersKey = "{ctf_presence}providers"

// How long the export's Online set is kept. Exports run a few minutes apart;
// readers take only a set at most CtfPresenceProvidersMaxAge old.
const ctfPresenceProvidersTtl = time.Hour

// The oldest export a sample or the census takes P1 from. Older, the export
// has stalled, and its Online set is not the providers' state now.
const CtfPresenceProvidersMaxAge = 20 * time.Minute

// A provider the score export decided online, as presence reads it.
type CtfPresenceProvider struct {
	ClientId  server.Id
	NetworkId server.Id
	// the country the provider is published under, normalized
	// (CtfNormalizeCountryCode)
	CountryCode string
}

// The score export's Online set: every provider whose top-level score is
// Online and not network-only, by client id.
type CtfPresenceProviders struct {
	MeasuredAt time.Time
	Providers  []*CtfPresenceProvider
}

// The encoded set: the export's unix milliseconds (8), the count (4), then per
// provider its client id (16), network id (16) and country (2).
const (
	ctfPresenceProvidersHeaderBytes = 12
	ctfPresenceProviderBytes        = 34
)

func encodeCtfPresenceProviders(providers *CtfPresenceProviders) []byte {
	b := make([]byte, 0, ctfPresenceProvidersHeaderBytes+ctfPresenceProviderBytes*len(providers.Providers))
	b = binary.BigEndian.AppendUint64(b, uint64(providers.MeasuredAt.UnixMilli()))
	b = binary.BigEndian.AppendUint32(b, uint32(len(providers.Providers)))
	for _, provider := range providers.Providers {
		b = append(b, provider.ClientId.Bytes()...)
		b = append(b, provider.NetworkId.Bytes()...)
		country := CtfNormalizeCountryCode(provider.CountryCode)
		b = append(b, country[0], country[1])
	}
	return b
}

func decodeCtfPresenceProviders(b []byte) (*CtfPresenceProviders, error) {
	if len(b) < ctfPresenceProvidersHeaderBytes {
		return nil, errors.New("ctf presence providers: truncated header")
	}
	measuredAt := time.UnixMilli(int64(binary.BigEndian.Uint64(b[0:8]))).UTC()
	count := int(binary.BigEndian.Uint32(b[8:12]))
	if len(b) != ctfPresenceProvidersHeaderBytes+count*ctfPresenceProviderBytes {
		return nil, fmt.Errorf("ctf presence providers: %d bytes for %d providers", len(b), count)
	}
	providers := &CtfPresenceProviders{MeasuredAt: measuredAt, Providers: make([]*CtfPresenceProvider, count)}
	for i := range count {
		row := b[ctfPresenceProvidersHeaderBytes+i*ctfPresenceProviderBytes:]
		provider := &CtfPresenceProvider{
			ClientId:    server.Id(row[0:16]),
			NetworkId:   server.Id(row[16:32]),
			CountryCode: string(row[32:34]),
		}
		if CtfNormalizeCountryCode(provider.CountryCode) != provider.CountryCode {
			return nil, fmt.Errorf("ctf presence providers: provider %d has country %q", i, provider.CountryCode)
		}
		providers.Providers[i] = provider
	}
	return providers, nil
}

// The export's Online set from its decided scores: each provider once, from any
// of the location maps it sits in, since every map decides it the same way
// (rankClientScore). A network-only provider is left out: it has no Public
// provide key. The country is the one the provider is published under.
func newCtfPresenceProviders(
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
) *CtfPresenceProviders {
	online := map[server.Id]*CtfPresenceProvider{}
	for _, clientScores := range locationClientScores {
		for clientId, clientScore := range clientScores {
			if _, ok := online[clientId]; ok || !clientScore.Online || clientScore.NetworkOnly {
				continue
			}
			countryCode := CtfCountryUnknown
			if egress := clientIdEgresses[clientId]; egress != nil {
				countryCode = CtfNormalizeCountryCode(normalizeCountryCode(egress.publishedCountryCode))
			}
			online[clientId] = &CtfPresenceProvider{ClientId: clientId, NetworkId: clientScore.NetworkId, CountryCode: countryCode}
		}
	}
	providers := &CtfPresenceProviders{MeasuredAt: measuredAt.UTC(), Providers: make([]*CtfPresenceProvider, 0, len(online))}
	for _, provider := range online {
		providers.Providers = append(providers.Providers, provider)
	}
	slices.SortFunc(providers.Providers, func(a, b *CtfPresenceProvider) int { return a.ClientId.Cmp(b.ClientId) })
	return providers
}

// publishCtfPresenceProviders writes the export's Online set for presence, when
// ctf.yml turns presence on. It runs inside the export and never fails it: an
// error is logged, and the sampler sees a stale set and takes no provider half.
func publishCtfPresenceProviders(
	ctx context.Context,
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	clientIdEgresses map[server.Id]*clientScoreEgress,
) {
	if !CtfPresenceEnabled() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[ctf]publish the export's online providers failed: %v\n", r)
		}
	}()
	providers := newCtfPresenceProviders(measuredAt, locationClientScores, clientIdEgresses)
	value := encodeCtfPresenceProviders(providers)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, ctfPresenceProvidersKey, value, ctfPresenceProvidersTtl).Err())
	})
	glog.Infof("[ctf]published %d online providers of the export at %s\n", len(providers.Providers), providers.MeasuredAt.Format(time.RFC3339))
}

// ReadCtfPresenceProviders returns the newest export's Online set, or nil when
// none is kept.
func ReadCtfPresenceProviders(ctx context.Context) (providers *CtfPresenceProviders, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, ctfPresenceProvidersKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		providers, returnErr = decodeCtfPresenceProviders(value)
	})
	return
}

// Testing_SetCtfPresenceProviders writes an Online set as the export would.
func Testing_SetCtfPresenceProviders(ctx context.Context, providers *CtfPresenceProviders) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, ctfPresenceProvidersKey, encodeCtfPresenceProviders(providers), ctfPresenceProvidersTtl).Err())
	})
}

// ---------------------------------------------------------------------------
// Samples
// ---------------------------------------------------------------------------

// One quarter-hour sample of an epoch.
type CtfPresenceSample struct {
	DeploymentKey StDeploymentKey
	NoId          uint64
	Epoch         uint64
	SampleIndex   int
	SampleTime    time.Time
	HeadBlock     uint64
	// the export whose Online set the provider half read; nil when none was
	// fresh, so the sample found no provider
	ProviderSourceTime *time.Time
	Providers          int
	Extenders          int
}

// One miner a sample found present.
type CtfPresenceObservation struct {
	Role     int
	ClientId server.Id
	// normalized (CtfNormalizeCountryCode)
	CountryCode string
	// the miner's unit in the sample, and its family (4 or 6); nil and 0 when
	// it has none
	Unit       *[32]byte
	UnitFamily int
}

// One miner's presence in one role and country of an epoch.
type CtfPresence struct {
	Role        int
	ClientId    server.Id
	CountryCode string
	Samples     CtfSampleSet
	SampleCount int
	// the latest sample's unit and family, nil and 0 for none
	Unit            *[32]byte
	UnitFamily      int
	LastSampleIndex int
}

// The rows of one statement of a sample's bitmap upsert.
const ctfPresenceBatchRows = 5000

// RecordCtfPresenceSample records a sample and the miners it found, once: a
// sample index already recorded is left as it is, and false is returned. Each
// miner is counted once per role and country.
func RecordCtfPresenceSample(ctx context.Context, sample *CtfPresenceSample, observations []*CtfPresenceObservation) (recorded bool) {
	key := requireStDeploymentKey(sample.DeploymentKey)
	if sample.SampleIndex < 0 || CtfSamplesPerEpoch <= sample.SampleIndex {
		panic(fmt.Errorf("ctf sample %d is not one of an epoch's %d", sample.SampleIndex, CtfSamplesPerEpoch))
	}
	type presenceKey struct {
		role        int
		clientId    server.Id
		countryCode string
	}
	seen := map[presenceKey]bool{}
	roles := []int16{}
	clientIds := []server.Id{}
	countryCodes := []string{}
	units := [][]byte{}
	unitFamilies := []int16{}
	for _, observation := range observations {
		if observation.Role != CtfRoleProvider && observation.Role != CtfRoleExtender {
			panic(fmt.Errorf("ctf presence role %d", observation.Role))
		}
		countryCode := CtfNormalizeCountryCode(observation.CountryCode)
		k := presenceKey{role: observation.Role, clientId: observation.ClientId, countryCode: countryCode}
		if seen[k] {
			continue
		}
		seen[k] = true
		roles = append(roles, int16(observation.Role))
		clientIds = append(clientIds, observation.ClientId)
		countryCodes = append(countryCodes, countryCode)
		if observation.Unit != nil && (observation.UnitFamily == 4 || observation.UnitFamily == 6) {
			units = append(units, observation.Unit[:])
			unitFamilies = append(unitFamilies, int16(observation.UnitFamily))
		} else {
			units = append(units, []byte{})
			unitFamilies = append(unitFamilies, 0)
		}
	}
	var only CtfSampleSet
	only.Set(sample.SampleIndex)

	server.Tx(ctx, func(tx server.PgTx) {
		recorded = false
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO ctf_presence_sample (
				deployment_key, no_id, epoch, sample_index, sample_time, head_block,
				provider_source_time, providers, extenders
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (deployment_key, no_id, epoch, sample_index) DO NOTHING
			`,
			key, int64(sample.NoId), int64(sample.Epoch), int16(sample.SampleIndex), sample.SampleTime.UTC(), int64(sample.HeadBlock),
			utcTimeOrNil(sample.ProviderSourceTime), sample.Providers, sample.Extenders,
		)
		server.Raise(err)
		if tag.RowsAffected() == 0 {
			return
		}
		recorded = true
		for start := 0; start < len(clientIds); start += ctfPresenceBatchRows {
			end := min(start+ctfPresenceBatchRows, len(clientIds))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO ctf_presence (
					deployment_key, no_id, epoch, role, client_id, country_code,
					samples, sample_count, unit, unit_family, last_sample_index, update_time
				)
				SELECT
					$1::varchar, $2::bigint, $3::bigint, presence.role, presence.client_id, presence.country_code,
					$4::bytea, 1,
					CASE WHEN octet_length(presence.unit) = 32 THEN presence.unit END,
					CASE WHEN octet_length(presence.unit) = 32 THEN NULLIF(presence.unit_family, 0) END,
					$5::smallint, $6::timestamp
				FROM unnest($7::smallint[], $8::uuid[], $9::varchar[], $10::bytea[], $11::smallint[])
					AS presence (role, client_id, country_code, unit, unit_family)
				ON CONFLICT (deployment_key, no_id, epoch, role, client_id, country_code) DO UPDATE
				SET
					samples = set_bit(ctf_presence.samples, $12::integer, 1),
					sample_count = ctf_presence.sample_count + 1 - get_bit(ctf_presence.samples, $12::integer),
					unit = CASE WHEN ctf_presence.last_sample_index <= EXCLUDED.last_sample_index THEN EXCLUDED.unit ELSE ctf_presence.unit END,
					unit_family = CASE WHEN ctf_presence.last_sample_index <= EXCLUDED.last_sample_index THEN EXCLUDED.unit_family ELSE ctf_presence.unit_family END,
					last_sample_index = GREATEST(ctf_presence.last_sample_index, EXCLUDED.last_sample_index),
					update_time = EXCLUDED.update_time
				`,
				key, int64(sample.NoId), int64(sample.Epoch),
				only[:], int16(sample.SampleIndex), sample.SampleTime.UTC(),
				roles[start:end], clientIds[start:end], countryCodes[start:end], units[start:end], unitFamilies[start:end],
				int32(sample.SampleIndex),
			))
		}
	})
	return
}

// GetCtfPresenceSamples returns the samples taken of an epoch, by index.
func GetCtfPresenceSamples(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfPresenceSample {
	key := requireStDeploymentKey(deploymentKey)
	samples := []*CtfPresenceSample{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT sample_index, sample_time, head_block, provider_source_time, providers, extenders
			FROM ctf_presence_sample
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY sample_index
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				sample := &CtfPresenceSample{DeploymentKey: deploymentKey, NoId: noId, Epoch: epoch}
				var index int16
				var headBlock int64
				server.Raise(result.Scan(&index, &sample.SampleTime, &headBlock, &sample.ProviderSourceTime, &sample.Providers, &sample.Extenders))
				sample.SampleIndex = int(index)
				sample.HeadBlock = uint64(headBlock)
				samples = append(samples, sample)
			}
		})
	})
	return samples
}

// HasCtfPresenceSample reports whether an epoch's sample was taken.
func HasCtfPresenceSample(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, index int) (taken bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT EXISTS (
				SELECT 1 FROM ctf_presence_sample
				WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3 AND sample_index = $4
			)
			`,
			key, int64(noId), int64(epoch), int16(index),
		).Scan(&taken))
	})
	return
}

// GetCtfPresences returns every miner's presence of an epoch, by role, client
// and country.
func GetCtfPresences(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) []*CtfPresence {
	key := requireStDeploymentKey(deploymentKey)
	presences := []*CtfPresence{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT role, client_id, country_code, samples, sample_count, unit, unit_family, last_sample_index
			FROM ctf_presence
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
			ORDER BY role, client_id, country_code
			`,
			key, int64(noId), int64(epoch),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				presence := &CtfPresence{}
				var role, sampleCount, lastSampleIndex int16
				var samples, unit []byte
				var unitFamily *int16
				server.Raise(result.Scan(&role, &presence.ClientId, &presence.CountryCode, &samples, &sampleCount, &unit, &unitFamily, &lastSampleIndex))
				if len(samples) != len(presence.Samples) {
					panic(fmt.Errorf("ctf presence of %s holds %d sample bytes", presence.ClientId, len(samples)))
				}
				copy(presence.Samples[:], samples)
				presence.Role = int(role)
				presence.SampleCount = int(sampleCount)
				presence.LastSampleIndex = int(lastSampleIndex)
				if len(unit) == 32 && unitFamily != nil {
					presence.Unit = (*[32]byte)(unit)
					presence.UnitFamily = int(*unitFamily)
				}
				presences = append(presences, presence)
			}
		})
	})
	return presences
}

// RemoveCtfPresenceBefore deletes the samples and presence of every epoch
// before epoch, once their census no longer needs them.
func RemoveCtfPresenceBefore(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) (removed int64) {
	key := requireStDeploymentKey(deploymentKey)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		removed = 0
		for _, table := range []string{"ctf_presence", "ctf_presence_sample"} {
			tag, err := tx.Exec(
				ctx,
				`DELETE FROM `+table+` WHERE deployment_key = $1 AND no_id = $2 AND epoch < $3`,
				key, int64(noId), int64(epoch),
			)
			server.Raise(err)
			removed += tag.RowsAffected()
		}
	})
	return
}

// ---------------------------------------------------------------------------
// Declared outages
// ---------------------------------------------------------------------------

// A platform-wide outage the operator declares (§4.2): every sample whose
// quarter hour overlaps it is excused, and the census lists it.
type CtfPresenceOutage struct {
	OutageId      server.Id
	DeploymentKey StDeploymentKey
	NoId          uint64
	StartTime     time.Time
	EndTime       time.Time
	Reason        string
	DeclaredBy    string
	CreateTime    time.Time
}

// DeclareCtfPresenceOutage records a declared outage. An epoch's census
// excuses only the outages declared before it is built.
func DeclareCtfPresenceOutage(ctx context.Context, outage *CtfPresenceOutage) server.Id {
	key := requireStDeploymentKey(outage.DeploymentKey)
	if !outage.StartTime.Before(outage.EndTime) {
		panic(fmt.Errorf("ctf outage ends at %s, not after its start %s", outage.EndTime, outage.StartTime))
	}
	if strings.TrimSpace(outage.Reason) == "" || strings.TrimSpace(outage.DeclaredBy) == "" {
		panic(errors.New("ctf outage needs a reason and who declares it"))
	}
	outageId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO ctf_presence_outage (
				outage_id, deployment_key, no_id, start_time, end_time, reason, declared_by, create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`,
			outageId, key, int64(outage.NoId), outage.StartTime.UTC(), outage.EndTime.UTC(),
			outage.Reason, outage.DeclaredBy, server.NowUtc(),
		))
	})
	return outageId
}

// GetCtfPresenceOutages returns the declared outages that overlap [from, to).
func GetCtfPresenceOutages(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, from time.Time, to time.Time) []*CtfPresenceOutage {
	key := requireStDeploymentKey(deploymentKey)
	outages := []*CtfPresenceOutage{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT outage_id, start_time, end_time, reason, declared_by, create_time
			FROM ctf_presence_outage
			WHERE deployment_key = $1 AND no_id = $2 AND $3 < end_time AND start_time < $4
			ORDER BY start_time, outage_id
			`,
			key, int64(noId), from.UTC(), to.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				outage := &CtfPresenceOutage{DeploymentKey: deploymentKey, NoId: noId}
				server.Raise(result.Scan(&outage.OutageId, &outage.StartTime, &outage.EndTime, &outage.Reason, &outage.DeclaredBy, &outage.CreateTime))
				outages = append(outages, outage)
			}
		})
	})
	return outages
}

// CtfExcusedSamples is the set of an epoch's samples that the outages excuse:
// each sample whose quarter hour overlaps an outage. A sample's quarter hour
// runs from its first block's time to the next sample's, with block times
// spread evenly over the epoch's boundaries [startTime, endTime), which are
// fixed once the epoch ends.
func CtfExcusedSamples(startTime time.Time, endTime time.Time, outages []*CtfPresenceOutage) CtfSampleSet {
	var excused CtfSampleSet
	if !startTime.Before(endTime) {
		return excused
	}
	span := endTime.Sub(startTime)
	at := func(i int) time.Time {
		return startTime.Add(time.Duration(int64(span) / CtfSamplesPerEpoch * int64(i)))
	}
	for i := range CtfSamplesPerEpoch {
		from, to := at(i), at(i+1)
		if i == CtfSamplesPerEpoch-1 {
			to = endTime
		}
		for _, outage := range outages {
			if outage.StartTime.Before(to) && from.Before(outage.EndTime) {
				excused.Set(i)
				break
			}
		}
	}
	return excused
}

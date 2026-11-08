// A zero-carry provider accepts contracts and carries nothing through them.
//
// Old provider builds (2026.4 and before, on Main in November 2026) stay
// connected, pass every gate the pool applies and accept every contract a
// client opens, and then carry no byte through any of them: each contract
// settles with both parties reporting zero usage. In a country where the URL
// probe fails for most providers, the URL-success rule's country guard keeps
// such providers in every bucket and in the counts (egress_online_success.go),
// so a country can list a dozen providers that carry nothing, and hosted
// proxies and clients there never get traffic through. The verdict here is
// measured: what the provider's own contracts carried, not its build.
//
// A provider is zero-carry when, in one evaluation by the taskworker's judge
// (JudgeProviderZeroCarry):
//
//   - its contracts closed in the last day (ProviderZeroCarryWindow), and
//     since its device last reported a new app version, newest first and at
//     most ProviderZeroCarryContractReadLimit of them, hold at least
//     ProviderZeroCarryMinContracts counted contracts from at least
//     ProviderZeroCarryMinNetworks distinct client networks; and
//   - those counted contracts carried at most
//     ProviderZeroCarryMaxCarriedByteCount bytes in all; and
//   - the URL probe measured it at least ProviderOnlineMinMeasuredUrlRuns
//     times in the URL rule's evidence window (ProviderEgressHealthMaxAge)
//     without one success.
//
// A counted contract is resolved with its usage settled from both parties'
// reports (an expiry's unconfirmed snapshot and a retained legacy exclusion
// are no evidence), has the provider as its destination (a companion contract
// carries the provider's own return traffic), comes from another network
// than the provider's, and from no egress prober network: the prober opens a
// fresh network for every pass (prober_shard_run), and its outcome is the URL
// evidence already.
//
// Why these numbers (Main, 2026-11-08, read only):
//
//   - Of 1,000 providers with a URL success in the window, none met the
//     contract rule at 20 contracts from 3 networks; the least any of the 90
//     with that many contracts carried was 46 KB. Four did at 10 contracts,
//     so 20 keeps a margin over an honest provider that clients used lightly.
//   - Three networks is the quorum the client verdicts and the TLS judge's
//     place rule use: one or two networks cannot tell a broken client from a
//     broken provider. The zombies of small countries (np, ge, bw) have only
//     three to eight client networks, so a larger quorum would keep them.
//   - The floor is noise: a few zombies' contracts settle at 3 or 30 bytes.
//     It is 45 times below the least an honest provider carried.
//   - The URL condition is what a client cannot forge: empty contracts from
//     sybil networks never make a provider that passes the server's own probe
//     zero-carry.
//
// The verdict is a hard exclusion (egress_index.go): out of every bucket,
// FindProviders2 answer and count, force_minimum and an explicit client id
// included, so a hosted proxy, an app and a rediscovery round alike stop
// being handed the provider, and the location counts and provider-locations
// stop listing it. It leaves the URL-success rule's country guard alone:
// the guard still counts the provider as measured without a success, so no
// country's exemption, and with it no other provider's place, changes
// because of a verdict.
//
// An excluded provider gets no contracts, so contracts cannot show it carries
// again. The verdict clears instead (clearProviderZeroCarryVerdicts) when:
//
//   - a URL probe succeeds after the verdict was made: the probe keeps
//     measuring the provider, as the URL rule's own exclusions are measured;
//   - its device reports another app version: an upgraded provider returns on
//     the judge's next run, and only contracts closed since the new version
//     count toward a new verdict;
//   - it reaches its expiry (ProviderZeroCarryVerdictLifetime): the provider
//     returns, and a new verdict needs fresh contracts.
//
// The judge reads the connected providers a page at a time in client id
// order, carrying its place in its task's arguments, and reads contracts only
// for the providers the URL condition leaves, a few percent of the fleet, by
// an index range on (destination_id, close_time) bounded by the window and
// the read limit. transfer_contract is large, so nothing here scans it.
package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The evidence window: the contracts a provider closed in the last day.
const ProviderZeroCarryWindow = 24 * time.Hour

// The most contracts one evaluation reads of a provider, newest first. The
// busiest zombie on Main closed about 1,400 a day; most providers the URL
// condition leaves closed none but the prober's, about 120. The bound keeps
// one provider's read short whatever it holds.
const ProviderZeroCarryContractReadLimit = 1000

// The fewest counted contracts that can show a provider carries nothing.
const ProviderZeroCarryMinContracts = 20

// The fewest distinct client networks the counted contracts come from.
const ProviderZeroCarryMinNetworks = 3

// The most bytes the counted contracts may carry in all.
const ProviderZeroCarryMaxCarriedByteCount ByteCount = 1024

// How long a verdict holds unless it clears sooner.
const ProviderZeroCarryVerdictLifetime = 72 * time.Hour

// The bound on each of the judge's page and contract statements. On Main a
// page of 5,000 providers takes about 0.15 s and twenty candidates' contracts
// 0.03 to 0.2 s; a plan that stopped using its index would be cancelled here
// rather than scan transfer_contract.
const providerZeroCarryStatementTimeout = 10 * time.Second

// The judge's bounds, replaceable in tests.
type ProviderZeroCarrySettings struct {
	// connected providers one run reads, in client id order
	PageSize int
	// candidates one contract statement reads
	EvidenceBatchSize int
	// the most contracts read per candidate
	ContractReadLimit int
	// the most verdicts one run reads to clear
	VerdictReadLimit int
}

func DefaultProviderZeroCarrySettings() *ProviderZeroCarrySettings {
	return &ProviderZeroCarrySettings{
		PageSize:          5000,
		EvidenceBatchSize: 20,
		ContractReadLimit: ProviderZeroCarryContractReadLimit,
		VerdictReadLimit:  10000,
	}
}

// What one evaluation read of a provider's contracts.
type ProviderZeroCarryEvidence struct {
	// contracts read, counted or not
	RowCount int
	// counted contracts, the distinct client networks they come from, and
	// what they carried in all
	ContractCount    int
	NetworkCount     int
	CarriedByteCount ByteCount
}

// Whether the evidence shows the provider carries nothing. The URL condition
// is the candidate read's (providerZeroCarryPageSql).
func (self ProviderZeroCarryEvidence) ZeroCarry() bool {
	return ProviderZeroCarryMinContracts <= self.ContractCount &&
		ProviderZeroCarryMinNetworks <= self.NetworkCount &&
		0 <= self.CarriedByteCount &&
		self.CarriedByteCount <= ProviderZeroCarryMaxCarriedByteCount
}

// The first close time an evaluation counts: the window's start, or the time
// the device last reported a new app version when that is later, so a new
// build is judged only on what it carried.
func providerZeroCarryEvidenceFrom(now time.Time, clientVersionTime *time.Time) time.Time {
	from := now.Add(-ProviderZeroCarryWindow).UTC()
	if clientVersionTime != nil && from.Before(clientVersionTime.UTC()) {
		from = clientVersionTime.UTC()
	}
	return from
}

// One stored verdict.
type ProviderZeroCarryVerdict struct {
	ClientId   server.Id
	CreateTime time.Time
	ExpireTime time.Time
	// the app version the device reported when the verdict was made, nil for
	// none
	ClientVersion    *string
	ContractCount    int
	NetworkCount     int
	CarriedByteCount ByteCount
}

// Why a verdict cleared, for the metrics and the log.
const (
	ProviderZeroCarryClearExpired    = "expired"
	ProviderZeroCarryClearUrlSuccess = "url_success"
	ProviderZeroCarryClearUpgrade    = "upgrade"
	// the client is gone or no longer active; nothing offers it either way
	ProviderZeroCarryClearGone = "gone"
)

// Every reason, in label order.
var ProviderZeroCarryClearReasons = []string{
	ProviderZeroCarryClearExpired,
	ProviderZeroCarryClearUrlSuccess,
	ProviderZeroCarryClearUpgrade,
	ProviderZeroCarryClearGone,
}

// A stored verdict with what the clearing rules read about its provider now.
type providerZeroCarryVerdictState struct {
	verdict ProviderZeroCarryVerdict
	// the client exists and is active
	active bool
	// the app version its device reports now, nil for none
	clientVersion *string
	// a URL probe succeeded after the verdict was made
	urlSuccess bool
}

func providerZeroCarryVersionText(version *string) string {
	if version == nil {
		return ""
	}
	return strings.TrimSpace(*version)
}

// Why the verdict clears now, "" while it holds. Expiry first, so a verdict
// past its time clears as expired whatever else changed.
func (self providerZeroCarryVerdictState) clearReason(now time.Time) string {
	switch {
	case !now.Before(self.verdict.ExpireTime):
		return ProviderZeroCarryClearExpired
	case self.urlSuccess:
		return ProviderZeroCarryClearUrlSuccess
	case providerZeroCarryVersionText(self.verdict.ClientVersion) != providerZeroCarryVersionText(self.clientVersion):
		return ProviderZeroCarryClearUpgrade
	case !self.active:
		return ProviderZeroCarryClearGone
	default:
		return ""
	}
}

// The measured URL run predicate over the history rows aliased alias: an
// accepted outcome of the selected policy, the rows the URL-success rule's
// evidence window counts (providerEgressHealthWindowSql). A security recheck
// has no measured outcome and never counts. Read per provider through
// provider_egress_health_history_client_time.
func providerZeroCarryUrlRunSql(alias string) string {
	return fmt.Sprintf(`%[1]s.total_count = 1 AND %[1]s.url_probe_policy_version = %[2]d`,
		alias, SelectedProviderUrlProbePolicyVersion())
}

// The same for a successful run.
func providerZeroCarryUrlSuccessSql(alias string) string {
	return providerZeroCarryUrlRunSql(alias) + fmt.Sprintf(` AND %s.ok_count = 1`, alias)
}

// One page of connected providers after $1 in client id order, at most $2,
// each with its device's version and whether it is a candidate: an active,
// top-level client with a Public provide key ($4) that the URL probe measured
// at least ProviderOnlineMinMeasuredUrlRuns times in ($5, $3] without one
// success, and whether it holds a verdict not yet expired at $3. The page
// is a range of the (valid, connected, client_id) index; every other read is
// a key lookup per row, and the URL reads, ranges of one provider's eight
// hours of history, stop at the first success and the second run.
func providerZeroCarryPageSql() string {
	return fmt.Sprintf(`
		WITH page AS MATERIALIZED (
			SELECT provider_location.client_id
			FROM network_client_location_reliability AS provider_location
			WHERE provider_location.valid AND provider_location.connected AND provider_location.client_id > $1
			ORDER BY provider_location.client_id
			LIMIT $2
		)
		SELECT
			page.client_id,
			provider.client_version,
			provider.client_version_time,
			COALESCE(provider.candidate, false),
			EXISTS (
				SELECT 1 FROM provider_zero_carry AS verdict
				WHERE verdict.client_id = page.client_id AND verdict.expire_time > $3
			)
		FROM page
		LEFT JOIN LATERAL (
			SELECT
				device.client_version,
				device.client_version_time,
				(
					client.active AND client.source_client_id IS NULL
					AND EXISTS (
						SELECT 1 FROM provide_key
						WHERE provide_key.client_id = page.client_id AND provide_key.provide_mode = $4
					)
					AND NOT EXISTS (
						SELECT 1 FROM provider_egress_health_history AS success
						WHERE success.client_id = page.client_id AND %[1]s
						AND success.measured_at > $5 AND success.measured_at <= $3
					)
					AND %[3]d <= (
						SELECT count(*) FROM (
							SELECT 1 FROM provider_egress_health_history AS run
							WHERE run.client_id = page.client_id AND %[2]s
							AND run.measured_at > $5 AND run.measured_at <= $3
							LIMIT %[3]d
						) AS runs
					)
				) AS candidate
			FROM network_client AS client
			LEFT JOIN device ON device.device_id = client.device_id
			WHERE client.client_id = page.client_id
			OFFSET 0
		) AS provider ON true
		ORDER BY page.client_id
	`, providerZeroCarryUrlSuccessSql("success"), providerZeroCarryUrlRunSql("run"), ProviderOnlineMinMeasuredUrlRuns)
}

// One connected provider of a page.
type providerZeroCarryPageRow struct {
	clientId          server.Id
	clientVersion     *string
	clientVersionTime *time.Time
	candidate         bool
	verdict           bool
}

// The counted contract predicate over a transfer_contract row aliased alias.
func providerZeroCarryCountedSql(alias string) string {
	return fmt.Sprintf(`(
		%[1]s.outcome IS NOT NULL
		AND %[1]s.companion_contract_id IS NULL
		AND %[1]s.source_network_id <> %[1]s.destination_network_id
		AND %[1]s.provider_usage IS NOT NULL
		AND %[1]s.provider_usage->'excluded_reason' IS NULL
		AND %[1]s.provider_usage->'legacy_exclusion' IS NULL
		AND NOT %[1]s.usage_unverified
		AND jsonb_typeof(%[1]s.provider_usage->'byte_count') = 'number'
		AND NOT EXISTS (
			SELECT 1 FROM prober_shard_run AS prober
			WHERE prober.network_id = %[1]s.source_network_id
		)
	)`, alias)
}

// The evidence of each candidate in $1 with its first counted close time in
// $2: its contracts closed in [$2, $3], newest first, at most $5, and with ids
// at least $4, the smallest id a contract closed in the window can have. Each
// candidate is one backward range of transfer_contract_destination_id_close_time
// that stops at the limit; the prober check is a unique-index lookup per row.
func providerZeroCarryEvidenceSql() string {
	return `
		SELECT
			candidate.client_id,
			evidence.row_count,
			evidence.contract_count,
			evidence.network_count,
			evidence.carried_byte_count
		FROM unnest($1::uuid[], $2::timestamp[]) AS candidate(client_id, evidence_from)
		CROSS JOIN LATERAL (
			SELECT
				count(*) AS row_count,
				count(*) FILTER (WHERE recent.counted) AS contract_count,
				count(DISTINCT recent.source_network_id) FILTER (WHERE recent.counted) AS network_count,
				COALESCE(sum(recent.byte_count) FILTER (WHERE recent.counted), 0)::bigint AS carried_byte_count
			FROM (
				SELECT
					contract.source_network_id,
					CASE WHEN jsonb_typeof(contract.provider_usage->'byte_count') = 'number'
						THEN (contract.provider_usage->>'byte_count')::numeric END AS byte_count,
					` + providerZeroCarryCountedSql("contract") + ` AS counted
				FROM transfer_contract AS contract
				WHERE contract.destination_id = candidate.client_id
				AND contract.close_time >= candidate.evidence_from
				AND contract.close_time <= $3
				AND contract.contract_id >= $4
				ORDER BY contract.close_time DESC
				LIMIT $5
			) AS recent
		) AS evidence
	`
}

// The smallest contract id a contract that closed at or after from can have:
// one created at most a contract lifetime before it, less the id's clock
// margin (contract_id_bound.go).
func providerZeroCarryContractIdFloor(from time.Time) server.Id {
	lower, _ := ContractIdCreateTimeRange(from.Add(-DefaultContractExpiration), from)
	return lower
}

// The verdicts in force at now of clientIds, or of every provider when
// clientIds is nil. The filter reads it with its other evidence
// (providerCountFilter.zeroCarry).
func getProviderZeroCarryClientIds(ctx context.Context, clientIds []server.Id, now time.Time) map[server.Id]bool {
	zeroCarry := map[server.Id]bool{}
	if clientIds != nil && len(clientIds) == 0 {
		return zeroCarry
	}
	server.Db(ctx, func(conn server.PgConn) {
		read := func(query string, args ...any) {
			rows, err := conn.Query(ctx, query, args...)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					server.Raise(rows.Scan(&clientId))
					zeroCarry[clientId] = true
				}
			})
		}
		if clientIds == nil {
			read(`SELECT client_id FROM provider_zero_carry WHERE expire_time > $1`, now.UTC())
			return
		}
		// one statement however large the cohort, as the TLS findings read
		// does: the table holds a row per verdict, a few hundred on Main
		read(`SELECT client_id FROM provider_zero_carry WHERE client_id = ANY($1) AND expire_time > $2`, clientIds, now.UTC())
	})
	return zeroCarry
}

// The read-through of the hard exclusions (providerHardExclusionsSql): the
// verdicts in force of the candidates in $1, by the database clock.
func providerZeroCarryHardExclusionSql() string {
	return `SELECT client_id FROM provider_zero_carry
			WHERE client_id = ANY($1) AND expire_time > (now() AT TIME ZONE 'UTC')`
}

// What one judge run did.
type ProviderZeroCarryJudgement struct {
	// where the next run starts, the zero id once a pass completed
	Cursor       server.Id
	PassComplete bool
	// connected providers read, the candidates among them, and those whose
	// contracts were read
	PageCount      int
	CandidateCount int
	EvaluatedCount int
	// contracts read
	ContractRowCount int
	// verdicts made, and cleared by reason
	Created int
	Cleared map[string]int
	// verdicts in force after the run, and those of connected providers by
	// published country ("" for none)
	ActiveCount   int
	CountryCounts map[string]int
}

// One run of the judge at now from cursor: clear the verdicts that no longer
// hold, judge one page of connected providers, and count what is in force.
func JudgeProviderZeroCarry(ctx context.Context, now time.Time, cursor server.Id, settings *ProviderZeroCarrySettings) *ProviderZeroCarryJudgement {
	if settings == nil {
		settings = DefaultProviderZeroCarrySettings()
	}
	now = now.UTC()
	judgement := &ProviderZeroCarryJudgement{
		Cleared:       map[string]int{},
		CountryCounts: map[string]int{},
	}
	for _, reason := range ProviderZeroCarryClearReasons {
		judgement.Cleared[reason] = 0
	}

	for reason, count := range clearProviderZeroCarryVerdicts(ctx, now, settings.VerdictReadLimit) {
		judgement.Cleared[reason] += count
	}

	page := readProviderZeroCarryPage(ctx, now, cursor, settings.PageSize)
	judgement.PageCount = len(page)
	if len(page) < settings.PageSize {
		judgement.PassComplete = true
	} else {
		judgement.Cursor = page[len(page)-1].clientId
	}
	candidates := []providerZeroCarryPageRow{}
	for _, row := range page {
		if row.candidate {
			judgement.CandidateCount += 1
			if !row.verdict {
				candidates = append(candidates, row)
			}
		}
	}
	judgement.EvaluatedCount = len(candidates)

	evidence := readProviderZeroCarryEvidence(ctx, now, candidates, settings.EvidenceBatchSize, settings.ContractReadLimit)
	verdicts := []*ProviderZeroCarryVerdict{}
	for _, candidate := range candidates {
		candidateEvidence, ok := evidence[candidate.clientId]
		if !ok {
			continue
		}
		judgement.ContractRowCount += candidateEvidence.RowCount
		if candidateEvidence.ZeroCarry() {
			verdicts = append(verdicts, &ProviderZeroCarryVerdict{
				ClientId:         candidate.clientId,
				CreateTime:       now,
				ExpireTime:       now.Add(ProviderZeroCarryVerdictLifetime),
				ClientVersion:    candidate.clientVersion,
				ContractCount:    candidateEvidence.ContractCount,
				NetworkCount:     candidateEvidence.NetworkCount,
				CarriedByteCount: candidateEvidence.CarriedByteCount,
			})
		}
	}
	judgement.Created = setProviderZeroCarryVerdicts(ctx, verdicts)

	judgement.ActiveCount, judgement.CountryCounts = getProviderZeroCarryCountryCounts(ctx, now)
	return judgement
}

// Runs read in a read-only transaction whose statements the database cancels
// past providerZeroCarryStatementTimeout. The reads tolerate replication
// delay: a verdict made on evidence a moment old is the same verdict.
func providerZeroCarryRead(ctx context.Context, read func(server.PgCanQuery)) {
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		server.Raise(err)
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cleanupCancel()
			_ = tx.Rollback(cleanupCtx)
		}()
		// a local setting ends with the transaction, never leaking to the pool
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = '%dms'`, providerZeroCarryStatementTimeout.Milliseconds())))
		read(tx)
	})
}

// Reads one page of connected providers after cursor.
func readProviderZeroCarryPage(ctx context.Context, now time.Time, cursor server.Id, pageSize int) []providerZeroCarryPageRow {
	page := []providerZeroCarryPageRow{}
	if pageSize <= 0 {
		return page
	}
	providerZeroCarryRead(ctx, func(conn server.PgCanQuery) {
		rows, err := conn.Query(
			ctx,
			providerZeroCarryPageSql(),
			cursor,
			pageSize,
			now,
			ProvideModePublic,
			now.Add(-ProviderEgressHealthMaxAge),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var row providerZeroCarryPageRow
				server.Raise(rows.Scan(&row.clientId, &row.clientVersion, &row.clientVersionTime, &row.candidate, &row.verdict))
				page = append(page, row)
			}
		})
	})
	return page
}

// Reads the evidence of every candidate, batchSize at a time. A candidate
// with no contract in its window has evidence of zero rows.
func readProviderZeroCarryEvidence(
	ctx context.Context,
	now time.Time,
	candidates []providerZeroCarryPageRow,
	batchSize int,
	readLimit int,
) map[server.Id]ProviderZeroCarryEvidence {
	evidence := map[server.Id]ProviderZeroCarryEvidence{}
	if len(candidates) == 0 || batchSize <= 0 || readLimit <= 0 {
		return evidence
	}
	contractIdFloor := providerZeroCarryContractIdFloor(now.Add(-ProviderZeroCarryWindow))
	providerZeroCarryRead(ctx, func(conn server.PgCanQuery) {
		for start := 0; start < len(candidates); start += batchSize {
			batch := candidates[start:min(start+batchSize, len(candidates))]
			clientIds := make([]server.Id, 0, len(batch))
			evidenceFroms := make([]time.Time, 0, len(batch))
			for _, candidate := range batch {
				clientIds = append(clientIds, candidate.clientId)
				evidenceFroms = append(evidenceFroms, providerZeroCarryEvidenceFrom(now, candidate.clientVersionTime))
			}
			rows, err := conn.Query(ctx, providerZeroCarryEvidenceSql(), clientIds, evidenceFroms, now, contractIdFloor, readLimit)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					var candidateEvidence ProviderZeroCarryEvidence
					var carried int64
					server.Raise(rows.Scan(
						&clientId,
						&candidateEvidence.RowCount,
						&candidateEvidence.ContractCount,
						&candidateEvidence.NetworkCount,
						&carried,
					))
					candidateEvidence.CarriedByteCount = ByteCount(carried)
					evidence[clientId] = candidateEvidence
				}
			})
		}
	})
	return evidence
}

// Stores the new verdicts. A provider's verdict replaces only one that has
// expired; one in force is kept. Returns how many were stored.
func setProviderZeroCarryVerdicts(ctx context.Context, verdicts []*ProviderZeroCarryVerdict) int {
	if len(verdicts) == 0 {
		return 0
	}
	stored := 0
	server.Tx(ctx, func(tx server.PgTx) {
		stored = 0
		const chunk = 1000
		for start := 0; start < len(verdicts); start += chunk {
			batch := verdicts[start:min(start+chunk, len(verdicts))]
			clientIds := make([]server.Id, 0, len(batch))
			createTimes := make([]time.Time, 0, len(batch))
			expireTimes := make([]time.Time, 0, len(batch))
			clientVersions := make([]*string, 0, len(batch))
			contractCounts := make([]int32, 0, len(batch))
			networkCounts := make([]int32, 0, len(batch))
			carriedByteCounts := make([]int64, 0, len(batch))
			for _, verdict := range batch {
				clientIds = append(clientIds, verdict.ClientId)
				createTimes = append(createTimes, verdict.CreateTime.UTC())
				expireTimes = append(expireTimes, verdict.ExpireTime.UTC())
				clientVersions = append(clientVersions, verdict.ClientVersion)
				contractCounts = append(contractCounts, int32(verdict.ContractCount))
				networkCounts = append(networkCounts, int32(verdict.NetworkCount))
				carriedByteCounts = append(carriedByteCounts, int64(verdict.CarriedByteCount))
			}
			tag := server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO provider_zero_carry (
					client_id, create_time, expire_time, client_version,
					contract_count, network_count, carried_byte_count
				)
				SELECT * FROM unnest($1::uuid[], $2::timestamp[], $3::timestamp[], $4::varchar[],
					$5::integer[], $6::integer[], $7::bigint[])
				ON CONFLICT (client_id) DO UPDATE SET
					create_time = EXCLUDED.create_time,
					expire_time = EXCLUDED.expire_time,
					client_version = EXCLUDED.client_version,
					contract_count = EXCLUDED.contract_count,
					network_count = EXCLUDED.network_count,
					carried_byte_count = EXCLUDED.carried_byte_count
				WHERE provider_zero_carry.expire_time <= EXCLUDED.create_time`,
				clientIds, createTimes, expireTimes, clientVersions, contractCounts, networkCounts, carriedByteCounts,
			))
			stored += int(tag.RowsAffected())
		}
	})
	return stored
}

// Clears every verdict that no longer holds at now (providerZeroCarryVerdictState.clearReason),
// reading at most limit, and returns how many cleared by reason. The table
// holds one row per verdict, a few hundred on Main; the URL read is one range
// of the provider's history since its verdict, which stops at a success.
func clearProviderZeroCarryVerdicts(ctx context.Context, now time.Time, limit int) map[string]int {
	cleared := map[string]int{}
	if limit <= 0 {
		return cleared
	}
	states := []providerZeroCarryVerdictState{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT
				verdict.client_id,
				verdict.create_time,
				verdict.expire_time,
				verdict.client_version,
				verdict.contract_count,
				verdict.network_count,
				verdict.carried_byte_count,
				COALESCE(client.active, false),
				device.client_version,
				EXISTS (
					SELECT 1 FROM provider_egress_health_history AS success
					WHERE success.client_id = verdict.client_id AND `+providerZeroCarryUrlSuccessSql("success")+`
					AND success.measured_at > verdict.create_time
				)
			FROM provider_zero_carry AS verdict
			LEFT JOIN network_client AS client ON client.client_id = verdict.client_id
			LEFT JOIN device ON device.device_id = client.device_id
			ORDER BY verdict.client_id
			LIMIT $1`, limit)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var state providerZeroCarryVerdictState
				var carried int64
				server.Raise(rows.Scan(
					&state.verdict.ClientId,
					&state.verdict.CreateTime,
					&state.verdict.ExpireTime,
					&state.verdict.ClientVersion,
					&state.verdict.ContractCount,
					&state.verdict.NetworkCount,
					&carried,
					&state.active,
					&state.clientVersion,
					&state.urlSuccess,
				))
				state.verdict.CarriedByteCount = ByteCount(carried)
				states = append(states, state)
			}
		})
	})
	clearClientIds := []server.Id{}
	clearCreateTimes := []time.Time{}
	for _, state := range states {
		if reason := state.clearReason(now); reason != "" {
			cleared[reason] += 1
			clearClientIds = append(clearClientIds, state.verdict.ClientId)
			clearCreateTimes = append(clearCreateTimes, state.verdict.CreateTime.UTC())
		}
	}
	if len(clearClientIds) != 0 {
		server.Tx(ctx, func(tx server.PgTx) {
			// only the verdict this run read: a newer one is the next run's
			server.RaisePgResult(tx.Exec(ctx, `
				DELETE FROM provider_zero_carry AS verdict
				USING unnest($1::uuid[], $2::timestamp[]) AS cleared(client_id, create_time)
				WHERE verdict.client_id = cleared.client_id AND verdict.create_time = cleared.create_time`,
				clearClientIds, clearCreateTimes,
			))
		})
	}
	return cleared
}

// The verdicts in force at now, and those of connected providers by their
// published country, "" for none.
func getProviderZeroCarryCountryCounts(ctx context.Context, now time.Time) (int, map[string]int) {
	active := 0
	countryCounts := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT
				provider_location.connected AND provider_location.valid,
				COALESCE(country.country_code, ''),
				count(*)
			FROM provider_zero_carry AS verdict
			LEFT JOIN network_client_location_reliability AS provider_location
				ON provider_location.client_id = verdict.client_id
			LEFT JOIN location AS country ON country.location_id = provider_location.country_location_id
			WHERE verdict.expire_time > $1
			GROUP BY 1, 2`, now)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var connected *bool
				var countryCode string
				var count int
				server.Raise(rows.Scan(&connected, &countryCode, &count))
				active += count
				if connected != nil && *connected {
					countryCounts[normalizeCountryCode(&countryCode)] += count
				}
			}
		})
	})
	return active, countryCounts
}

// A provider's verdict in force at now, nil for none, for `bringyourctl
// provider inspect` and the tests.
func GetProviderZeroCarryVerdict(ctx context.Context, clientId server.Id, now time.Time) *ProviderZeroCarryVerdict {
	var verdict *ProviderZeroCarryVerdict
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT client_id, create_time, expire_time, client_version, contract_count, network_count, carried_byte_count
			FROM provider_zero_carry
			WHERE client_id = $1 AND expire_time > $2`, clientId, now.UTC())
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				verdict = &ProviderZeroCarryVerdict{}
				var carried int64
				server.Raise(rows.Scan(
					&verdict.ClientId,
					&verdict.CreateTime,
					&verdict.ExpireTime,
					&verdict.ClientVersion,
					&verdict.ContractCount,
					&verdict.NetworkCount,
					&carried,
				))
				verdict.CarriedByteCount = ByteCount(carried)
			}
		})
	})
	return verdict
}

// The country codes of a judgement's counts in order, for logs.
func (self *ProviderZeroCarryJudgement) CountrySummary() string {
	countryCodes := make([]string, 0, len(self.CountryCounts))
	for countryCode := range self.CountryCounts {
		countryCodes = append(countryCodes, countryCode)
	}
	slices.Sort(countryCodes)
	parts := make([]string, 0, len(countryCodes))
	for _, countryCode := range countryCodes {
		label := countryCode
		if label == "" {
			label = "none"
		}
		parts = append(parts, fmt.Sprintf("%s=%d", label, self.CountryCounts[countryCode]))
	}
	return strings.Join(parts, " ")
}

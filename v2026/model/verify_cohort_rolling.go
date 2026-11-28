package model

// The verify cohort's rolling census (sn VERIFY-COHORT-V2.md §2.4-2.6; owner
// approval 2026-11-17). The SN25 validator, from VERIFY-COHORT-V2.md on, drops
// at each settlement fold the carried quality of every provider with no
// assignment in the closing window, unless nobody had one. Its census during
// window v is then within the providers assigned in windows v-1 and v, so the
// operator no longer has to count every provider it ever admitted: it counts a
// member until the validator has folded two windows after the member's last
// assignment, and freed room goes to new providers.
//
// The server never asks the validator who it holds. It keeps a superset by
// construction (§2.5):
//   - A validator window can hold assignments the server labelled one epoch
//     either side, so a member assigned last in server epoch s can be in the
//     validator's windows s-1 to s+1. The first non-empty window w >= s+2 does
//     not hold it, and the validator's fold of w retires it.
//   - The server reads the validator's settlement epoch v* from its signed head
//     summaries (sn_head_summary), which are made only after the fold into
//     their epoch, so v* never runs ahead of the validator.
//   - A member expires once some server epoch w with max(s+2, E0) <= w <= v*-1
//     had an assignment: E0 is the first window the validator folded with the
//     retirement rule, so a member last assigned before it waits for that fold.
//     An epoch with no assignment at all may have been an empty validator
//     window, which retires nobody, so it never counts.
//   - A stale or missing report frees nothing.
//
// The rolling census starts on its own: once the validator's summaries carry a
// census (census_providers) and it has folded a window since the first of them,
// the taskworker records the start in verify_cohort_state and from then on runs
// expiry instead of RotateVerifyCohort. Before that the admission script and
// the taskworker behave exactly as before. While it runs, an interlock keeps
// the validator's reported census plus everything admitted since its last two
// reports under 3,584 (verifyCohortInterlockLimit), so a validator that stops
// retiring stops admissions before its hard bound.
//
// This reads one validator: with several, v* is the lowest of their latest
// epochs and the census the highest of their latest reports, which holds every
// member any of them can hold, and the report counters reset only when the
// slowest of them reports again.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// A latest summary older than this is a validator that stopped reporting; it
// no longer holds v* back once every other validator has reported since.
const verifyCohortReportWindow = 48 * time.Hour

// A report this old is stale: the validator reports once a tempo, about every
// 72 minutes, so this is about two and a half missed tempos. A stale report
// frees nothing and admits no new provider.
const verifyCohortReportFresh = 3 * time.Hour

// Expiry looks back this many server epochs for an epoch with an assignment.
// Older epoch sets may have expired from Redis (verifyCohortEpochTtl, 30 days),
// and an unreadable epoch never counts, so this only bounds the work.
const verifyCohortExpiryLookback = 8

// The most members one expiry script removes.
const verifyCohortExpiryBatch = 512

var (
	verifyCohortRollingMembers = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "rolling_members",
		Help: "Members the verify cohort counts against its limit at the last binding sync: in the rolling census (sn VERIFY-COHORT-V2.md §2.4) the members not yet expired, in the legacy census every provider ever admitted.",
	})
	verifyCohortRollingLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "rolling_limit",
		Help: "The verify cohort's lifetime limit (verify.yml cohort_lifetime_limit), which bounds the rolling census: claimed fleets' members up to it, every other tier up to it less the headroom held for claimed members.",
	})
	verifyCohortRollingMode = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "rolling_mode",
		Help: "1 while the verify cohort runs the rolling census (the validator's head summaries carry its census and it has folded a settlement window since the first of them), 0 while it runs the legacy lifetime census.",
	})
	verifyCohortExpirations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "expirations_total",
		Help: "Members the rolling census stopped counting: the validator has folded a window with an assignment after the member's last assignment, so it no longer holds the member's quality.",
	})
	verifyCohortValidatorCensus = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "validator_census",
		Help: "The validator's measurement census for this operator as its latest head summary reports it (census_providers): the set it bounds by its signed max_providers, 4,096. -1 while no fresh summary reports one.",
	})
	verifyCohortValidatorCensusAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "validator_census_age_seconds",
		Help: "Age of the head summary the validator census was read from. The validator reports once a tempo, about every 72 minutes; past 3 hours the report is stale and the rolling census frees nothing and admits no new provider.",
	})
)

func init() {
	verifyCohortValidatorCensus.Set(-1)
	prometheus.MustRegister(verifyCohortRollingMembers, verifyCohortRollingLimit, verifyCohortRollingMode, verifyCohortExpirations, verifyCohortValidatorCensus, verifyCohortValidatorCensusAge)
}

// What the validator's latest head summaries report.
type VerifyCohortValidatorReport struct {
	// validators with a summary in the window
	Hotkeys int
	// v*: the lowest of their latest settlement epochs
	SettlementEpoch uint64
	// the report's identity: the lowest of their latest native epochs
	NativeEpoch uint64
	// the highest of their latest censuses; nil when any latest summary lacks one
	Census *uint64
	// the oldest of their latest summaries' receipts
	ReceivedAt time.Time
	// the earliest settlement epoch of any retained summary carrying a census
	FirstCensusEpoch *uint64
}

// GetVerifyCohortValidatorReport reads the latest summary of each validator
// that reported in the window, or nil when none did.
func GetVerifyCohortValidatorReport(ctx context.Context, netuid uint16, now time.Time) *VerifyCohortValidatorReport {
	var report *VerifyCohortValidatorReport
	server.Db(ctx, func(conn server.PgConn) {
		report = nil
		rows, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT ON (validator_hotkey)
					native_epoch, settlement_epoch, census_providers, received_at
				FROM sn_head_summary
				WHERE netuid = $1 AND $2 <= received_at
				ORDER BY validator_hotkey, native_epoch DESC
			`,
			int(netuid), now.Add(-verifyCohortReportWindow).UTC(),
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var nativeEpoch, settlementEpoch int64
				var census *int64
				var receivedAt time.Time
				server.Raise(rows.Scan(&nativeEpoch, &settlementEpoch, &census, &receivedAt))
				if nativeEpoch < 0 || settlementEpoch < 0 {
					continue
				}
				if report == nil {
					report = &VerifyCohortValidatorReport{SettlementEpoch: uint64(settlementEpoch), NativeEpoch: uint64(nativeEpoch), ReceivedAt: receivedAt}
					if census != nil && 0 <= *census {
						value := uint64(*census)
						report.Census = &value
					}
				} else {
					report.SettlementEpoch = min(report.SettlementEpoch, uint64(settlementEpoch))
					report.NativeEpoch = min(report.NativeEpoch, uint64(nativeEpoch))
					if receivedAt.Before(report.ReceivedAt) {
						report.ReceivedAt = receivedAt
					}
					if census == nil || *census < 0 || report.Census == nil {
						report.Census = nil
					} else {
						value := max(*report.Census, uint64(*census))
						report.Census = &value
					}
				}
				report.Hotkeys += 1
			}
		})
		if report == nil {
			return
		}
		rows, err = conn.Query(
			ctx,
			`SELECT min(settlement_epoch) FROM sn_head_summary WHERE netuid = $1 AND census_providers IS NOT NULL`,
			int(netuid),
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var first *int64
				server.Raise(rows.Scan(&first))
				if first != nil && 0 <= *first {
					value := uint64(*first)
					report.FirstCensusEpoch = &value
				}
			}
		})
	})
	return report
}

// VerifyCohortRollingSchemaMissing reports whether err is a database without
// the rolling census's migration: its table or columns are absent. Such a
// database runs the legacy census.
func VerifyCohortRollingSchemaMissing(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == pgerrcode.UndefinedTable || pgErr.Code == pgerrcode.UndefinedColumn)
}

// The rolling census's start, once recorded: the first validator epoch whose
// census reflects a fold with the retirement rule.
func getVerifyCohortRollingFrom(ctx context.Context, deploymentKey StDeploymentKey) (rollingFrom uint64, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT rolling_from_epoch FROM verify_cohort_state WHERE deployment_key = $1`, string(deploymentKey))
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var value int64
				server.Raise(rows.Scan(&value))
				if 0 < value {
					rollingFrom, ok = uint64(value), true
				}
			}
		})
	})
	return
}

// Records the rolling census's start once; a start already recorded stands.
func setVerifyCohortRollingFrom(ctx context.Context, deploymentKey StDeploymentKey, rollingFrom uint64, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO verify_cohort_state (deployment_key, rolling_from_epoch, rolling_since)
				VALUES ($1, $2, $3)
				ON CONFLICT (deployment_key) DO NOTHING
			`,
			string(deploymentKey), int64(rollingFrom), now.UTC(),
		))
	})
}

// VerifyCohortRollingStart decides when the rolling census may start: once a
// summary carries a census (the validator runs the retirement rule) and the
// validator has folded a settlement window since the first such summary, which
// that rule retired. The start is the first validator epoch after that fold.
func VerifyCohortRollingStart(report *VerifyCohortValidatorReport) (rollingFrom uint64, ok bool) {
	if report == nil || report.Census == nil || report.FirstCensusEpoch == nil || report.SettlementEpoch < *report.FirstCensusEpoch+1 {
		return 0, false
	}
	return *report.FirstCensusEpoch + 1, true
}

// The cohort's rolling state after one refresh.
type VerifyCohortRollingState struct {
	Rolling bool
	// the first validator epoch whose census reflects a retiring fold
	RollingFrom uint64
	// a fresh report with a census: expiry may run and new providers may enter
	CensusKnown bool
	Census      uint64
	// v*, the validator's settlement epoch
	ValidatorEpoch uint64
	// room held for claimed members: those waiting now plus the members of
	// fleets whose bindings start at the next epoch
	Headroom int
	// claimed members eligible now and not counted
	Waiting int
}

// RefreshVerifyCohortState reads the validator's report, records the rolling
// census's start when it is due, and writes the state the admission script
// reads. upcomingClaimed is the claimed members of fleets whose bindings start
// at the next settlement epoch, at most a per-fleet share of each. The
// taskworker runs it after each binding sync, once the priority set is fresh.
func RefreshVerifyCohortState(ctx context.Context, settings *VerifySettings, report *VerifyCohortValidatorReport, upcomingClaimed int, now time.Time) *VerifyCohortRollingState {
	if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" {
		return nil
	}
	deploymentKey := settings.CohortDeploymentKey
	epoch, ok := verifyCohortEpoch(ctx, deploymentKey)
	if !ok {
		return nil
	}
	keys := newVerifyCohortKeys(deploymentKey, epoch)
	state := &VerifyCohortRollingState{}
	state.RollingFrom, state.Rolling = getVerifyCohortRollingFrom(ctx, deploymentKey)
	if !state.Rolling {
		if rollingFrom, start := VerifyCohortRollingStart(report); start {
			setVerifyCohortRollingFrom(ctx, deploymentKey, rollingFrom, now)
			state.RollingFrom, state.Rolling = getVerifyCohortRollingFrom(ctx, deploymentKey)
			if state.Rolling {
				glog.Infof("[verify]cohort rolling census starts from validator epoch %d (first census at epoch %d, validator at %d)\n", state.RollingFrom, *report.FirstCensusEpoch, report.SettlementEpoch)
			}
		}
	}
	if report != nil {
		state.ValidatorEpoch = report.SettlementEpoch
		if report.Census != nil && now.Sub(report.ReceivedAt) <= verifyCohortReportFresh {
			state.CensusKnown, state.Census = true, *report.Census
		}
	}

	server.Redis(ctx, func(r server.RedisClient) {
		priority, err := r.SMembers(ctx, keys.priority).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		if 0 < len(priority) {
			args := make([]any, len(priority))
			for i, member := range priority {
				args[i] = member
			}
			counted, err := r.SMIsMember(ctx, keys.census, args...).Result()
			server.Raise(err)
			eligible, err := r.SMIsMember(ctx, verifyEligibleKey, args...).Result()
			server.Raise(err)
			for i := range priority {
				if !counted[i] && eligible[i] {
					state.Waiting += 1
				}
			}
		}
		state.Headroom = state.Waiting + max(0, upcomingClaimed)

		rolling := "0"
		if state.Rolling {
			rolling = "1"
		}
		census := ""
		if state.CensusKnown {
			census = strconv.FormatUint(state.Census, 10)
		}
		reportId := ""
		if report != nil {
			reportId = fmt.Sprintf("%d:%d", report.Hotkeys, report.NativeEpoch)
		}
		server.Raise(r.Eval(ctx, verifyCohortStateScript, []string{keys.state},
			rolling, census, reportId, state.Headroom, state.ValidatorEpoch, state.RollingFrom).Err())
	})

	verifyCohortRollingLimit.Set(float64(settings.CohortLifetimeLimit))
	if state.Rolling {
		verifyCohortRollingMode.Set(1)
	} else {
		verifyCohortRollingMode.Set(0)
	}
	if state.CensusKnown {
		verifyCohortValidatorCensus.Set(float64(state.Census))
	} else {
		verifyCohortValidatorCensus.Set(-1)
	}
	if report != nil {
		verifyCohortValidatorCensusAge.Set(now.Sub(report.ReceivedAt).Seconds())
	}
	return state
}

// verifyCohortStateScript writes the rolling state the admission script reads.
// A new report moves the admissions counted since the last report to previous
// and starts since again: the new report's census was measured before it
// arrived, so admissions in between stay counted until the next report, whose
// measurement comes after them. Every value is checked before the first write.
//
//	KEYS[1] state hash
//	ARGV[1] '1' when the rolling census is on, ARGV[2] the reported census or
//	'' when unknown, ARGV[3] the report's identity, ARGV[4] headroom,
//	ARGV[5] v*, ARGV[6] the rolling start
const verifyCohortStateScript = `
local since = redis.call('HGET', KEYS[1], 'since')
if since and not string.match(since, '^%d+$') then
  return redis.error_reply('verify cohort rolling state is not an integer')
end
if redis.call('HGET', KEYS[1], 'report') ~= ARGV[3] then
  redis.call('HSET', KEYS[1], 'previous', since or '0', 'since', '0', 'report', ARGV[3])
end
if ARGV[1] == '1' then
  redis.call('HSET', KEYS[1], 'rolling', '1')
else
  redis.call('HDEL', KEYS[1], 'rolling')
end
if ARGV[2] == '' then
  redis.call('HDEL', KEYS[1], 'census')
else
  redis.call('HSET', KEYS[1], 'census', ARGV[2])
end
redis.call('HSET', KEYS[1], 'headroom', ARGV[4], 'vstar', ARGV[5], 'rolling_from', ARGV[6])
return 1
`

// verifyCohortRollingExpires reports whether a member has left the validator's
// census: the validator, at settlement epoch vstar, has folded a window w with
// an assignment and max(last+2, firstRetiring) <= w <= vstar-1. last is the
// member's last server epoch with an assignment; known is false for a legacy
// member last assigned before the rolling census started, whose only bound is
// firstRetiring, the first window the validator folded with the retirement
// rule. assigned reports whether a server epoch had any assignment; an epoch it
// can't read must answer false.
func verifyCohortRollingExpires(last uint64, known bool, vstar uint64, firstRetiring uint64, assigned func(uint64) bool) bool {
	if vstar == 0 {
		return false
	}
	low := firstRetiring
	if known {
		low = max(low, last+2)
	}
	for w := vstar - 1; low <= w; w-- {
		if vstar-1-w >= verifyCohortExpiryLookback {
			return false
		}
		if assigned(w) {
			return true
		}
		if w == 0 {
			return false
		}
	}
	return false
}

// The outcome of one expiry run.
type VerifyCohortExpiry struct {
	// members counted after the run
	Members int
	Expired int
	// Redis had lost its copy and the durable census was restored
	Restored bool
}

// ExpireVerifyCohort keeps the rolling census durable and stops counting the
// members the validator no longer holds. The taskworker runs it after each
// binding sync while the rolling census is on, in place of RotateVerifyCohort.
func ExpireVerifyCohort(ctx context.Context, settings *VerifySettings, state *VerifyCohortRollingState) *VerifyCohortExpiry {
	if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" || state == nil || !state.Rolling {
		return nil
	}
	deploymentKey := settings.CohortDeploymentKey
	epoch, ok := verifyCohortEpoch(ctx, deploymentKey)
	if !ok {
		return nil
	}
	keys := newVerifyCohortKeys(deploymentKey, epoch)
	result := &VerifyCohortExpiry{}

	// the durable census with each member's last epoch
	durable := map[string]*uint64{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id, last_epoch FROM verify_cohort_census WHERE deployment_key = $1`, string(deploymentKey))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				var last *int64
				server.Raise(rows.Scan(&clientId, &last))
				if last != nil && 0 <= *last {
					value := uint64(*last)
					durable[clientId.String()] = &value
				} else {
					durable[clientId.String()] = nil
				}
			}
		})
	})

	var census []string
	// each member's last server epoch with an assignment, from the epoch sets
	// still in Redis: any recent one, so a member assigned just before the
	// rolling census started is never read as a legacy member
	lastSeen := map[string]uint64{}
	assigned := map[uint64]bool{}
	server.Redis(ctx, func(r server.RedisClient) {
		ready, err := r.Exists(ctx, keys.censusReady).Result()
		server.Raise(err)
		count, err := r.SCard(ctx, keys.census).Result()
		server.Raise(err)
		if ready == 0 || count < int64(len(durable)) {
			// restore what Redis lost, counting every durable member as assigned
			// this epoch: its true last epoch may be lost with the epoch sets
			result.Restored = 0 < len(durable)
			members := make([]string, 0, len(durable))
			for member := range durable {
				members = append(members, member)
			}
			slices.Sort(members)
			for start := 0; start < len(members); start += 512 {
				batch := make([]any, 0, 512)
				for _, member := range members[start:min(start+512, len(members))] {
					batch = append(batch, member)
				}
				server.Raise(r.SAdd(ctx, keys.census, batch...).Err())
				server.Raise(r.SAdd(ctx, keys.lifetime, batch...).Err())
			}
		}
		server.Raise(r.SUnionStore(ctx, keys.census, keys.census, keys.lifetime).Err())
		server.Raise(r.Set(ctx, keys.censusReady, "1", 0).Err())
		census, err = r.SMembers(ctx, keys.census).Result()
		server.Raise(err)
		for x := epoch - min(epoch, verifyCohortExpiryLookback); x <= epoch; x++ {
			for _, member := range verifyCohortSetMembers(ctx, r, newVerifyCohortKeys(deploymentKey, x).epoch) {
				lastSeen[member] = max(lastSeen[member], x)
			}
		}
		// which recent server epochs had an assignment
		low := state.ValidatorEpoch - min(state.ValidatorEpoch, verifyCohortExpiryLookback)
		for w := low; w < state.ValidatorEpoch; w++ {
			size, err := r.SCard(ctx, newVerifyCohortKeys(deploymentKey, w).epoch).Result()
			server.Raise(err)
			assigned[w] = 0 < size
		}
	})
	if result.Restored {
		verifyCohortCensusRestores.Inc()
	}

	// mirror what Postgres lacks, and keep each member's last epoch
	missing := []server.Id{}
	for _, member := range census {
		if _, ok := durable[member]; !ok {
			if clientId, err := server.ParseId(member); err == nil {
				missing = append(missing, clientId)
			}
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if 0 < len(missing) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO verify_cohort_census (deployment_key, client_id, admitted_at, last_epoch)
					SELECT $1, client_id, $3, $4 FROM unnest($2::uuid[]) AS client_id
					ON CONFLICT (deployment_key, client_id) DO NOTHING
				`,
				string(deploymentKey), missing, server.NowUtc(), int64(epoch),
			))
		}
		if result.Restored {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE verify_cohort_census SET last_epoch = $2 WHERE deployment_key = $1 AND (last_epoch IS NULL OR last_epoch < $2)`,
				string(deploymentKey), int64(epoch),
			))
		}
		byEpoch := map[uint64]map[string]bool{}
		for member, x := range lastSeen {
			if byEpoch[x] == nil {
				byEpoch[x] = map[string]bool{}
			}
			byEpoch[x][member] = true
		}
		for x, members := range byEpoch {
			ids := verifyCohortIds(members)
			if len(ids) == 0 {
				continue
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE verify_cohort_census SET last_epoch = $3
					WHERE deployment_key = $1 AND client_id = ANY($2::uuid[]) AND (last_epoch IS NULL OR last_epoch < $3)
				`,
				string(deploymentKey), ids, int64(x),
			))
		}
	})

	result.Members = len(census)
	if !state.CensusKnown || state.RollingFrom == 0 {
		verifyCohortRollingMembers.Set(float64(result.Members))
		verifyCohortCensusSize.Set(float64(result.Members))
		return result
	}
	firstRetiring := state.RollingFrom - 1
	stale := []any{}
	for _, member := range census {
		var last uint64
		known := false
		if value, ok := durable[member]; ok && value != nil {
			last, known = *value, true
		}
		if result.Restored {
			last, known = epoch, true
		}
		if x, ok := lastSeen[member]; ok {
			last, known = max(last, x), true
		}
		if verifyCohortRollingExpires(last, known, state.ValidatorEpoch, firstRetiring, func(w uint64) bool { return assigned[w] }) {
			stale = append(stale, member)
		}
	}

	removed := []server.Id{}
	server.Redis(ctx, func(r server.RedisClient) {
		next := newVerifyCohortKeys(deploymentKey, epoch+1)
		for start := 0; start < len(stale); start += verifyCohortExpiryBatch {
			batch := stale[start:min(start+verifyCohortExpiryBatch, len(stale))]
			values, err := r.Eval(ctx, verifyCohortExpireScript, []string{keys.census, keys.lifetime, keys.epoch, next.epoch}, batch...).StringSlice()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for _, value := range values {
				if clientId, err := server.ParseId(value); err == nil {
					removed = append(removed, clientId)
				}
			}
		}
	})
	if 0 < len(removed) {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM verify_cohort_census WHERE deployment_key = $1 AND client_id = ANY($2::uuid[])`,
				string(deploymentKey), removed,
			))
		})
	}
	result.Expired = len(removed)
	result.Members -= result.Expired
	verifyCohortExpirations.Add(float64(result.Expired))
	verifyCohortRollingMembers.Set(float64(result.Members))
	verifyCohortCensusSize.Set(float64(result.Members))
	return result
}

// verifyCohortExpireScript stops counting the members of ARGV, except one drawn
// again this epoch or the next, so an admission racing the expiry is never lost
// from the count. Returns the members removed.
//
//	KEYS[1] census set, KEYS[2] lifetime set, KEYS[3] this epoch's cohort,
//	KEYS[4] the next epoch's cohort
const verifyCohortExpireScript = `
local removed = {}
for i = 1, #ARGV do
  local member = ARGV[i]
  if redis.call('SISMEMBER', KEYS[3], member) == 0 and redis.call('SISMEMBER', KEYS[4], member) == 0 then
    if redis.call('SREM', KEYS[1], member) == 1 then
      table.insert(removed, member)
    end
    redis.call('SREM', KEYS[2], member)
  end
end
return removed
`

// The members of a set, or none when it is absent.
func verifyCohortSetMembers(ctx context.Context, r server.RedisClient, key string) []string {
	members, err := r.SMembers(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		server.Raise(err)
	}
	return members
}

func verifyCohortIds(members map[string]bool) []server.Id {
	ids := []server.Id{}
	for member := range members {
		if clientId, err := server.ParseId(member); err == nil {
			ids = append(ids, clientId)
		}
	}
	return ids
}

// Writes a new rolling-census member through to Postgres with the epoch it was
// admitted in. A failed write is logged: the next expiry run mirrors the Redis
// census again, counting the member as assigned that epoch.
func recordVerifyCohortRollingCensus(ctx context.Context, deploymentKey StDeploymentKey, clientId server.Id, epoch uint64) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO verify_cohort_census (deployment_key, client_id, admitted_at, last_epoch)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (deployment_key, client_id) DO UPDATE
					SET last_epoch = GREATEST(coalesce(verify_cohort_census.last_epoch, 0), EXCLUDED.last_epoch)
				`,
				string(deploymentKey), clientId, server.NowUtc(), int64(epoch),
			))
		})
	}, func(err error) {
		glog.Infof("[verify]cohort rolling census write-through failed (the next expiry run mirrors it): %s\n", err)
	})
}

// VerifyCohortRollingMode reports whether the rolling census is on, from the
// state the admission script reads. Cached briefly per process: the board fold
// asks once a tempo and the binding sync once a minute.
func VerifyCohortRollingMode(ctx context.Context, settings *VerifySettings) bool {
	if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" {
		return false
	}
	cache := verifyCohortRollingModeCache
	cache.stateLock.Lock()
	if value, readAt := cache.rolling[settings.CohortDeploymentKey], cache.readAt[settings.CohortDeploymentKey]; !readAt.IsZero() && time.Since(readAt) < verifyCohortRollingModeCacheTtl {
		cache.stateLock.Unlock()
		return value
	}
	cache.stateLock.Unlock()
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 0)
	rolling := false
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.HMGet(ctx, keys.state, "rolling", "headroom").Result()
		server.Raise(err)
		rolling, _ = verifyCohortRollingFields(values)
	})
	cache.stateLock.Lock()
	cache.rolling[settings.CohortDeploymentKey], cache.readAt[settings.CohortDeploymentKey] = rolling, time.Now()
	cache.stateLock.Unlock()
	return rolling
}

const verifyCohortRollingModeCacheTtl = 10 * time.Second

var verifyCohortRollingModeCache = &struct {
	stateLock sync.Mutex
	rolling   map[StDeploymentKey]bool
	readAt    map[StDeploymentKey]time.Time
}{rolling: map[StDeploymentKey]bool{}, readAt: map[StDeploymentKey]time.Time{}}

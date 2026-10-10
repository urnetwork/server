// Direct PostgreSQL observations supplement advisory admission and wire errors.
// Finite sampling reports its blind interval; it cannot prove no sub-gap waits.
package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

const legacyCloseDBWaitSamplePeriod = 5 * time.Millisecond
const legacyCloseDBWaitCoverageLimit = 50 * time.Millisecond
const legacyCloseDBWaitQueryTimeout = 200 * time.Millisecond
const legacyCloseDBWaitEventLimit = 64
const legacyCloseDBWaitGapEventLimit = 1024
const legacyCloseDBWaitPreparedName = "legacy_close_lock_wait_observer_v4"

// Every sample is a fresh autocommit statement on a separate direct session.
// Neither query text nor relation/key values leave PostgreSQL. Backends are
// scoped to the disposable database, including both application routes, and
// this observer is excluded. An advisory lock waited on inside PostgreSQL is
// distinct from a nonwaiting pg_try_advisory_* refusal in the model counters.
const legacyCloseDBWaitSQL = `WITH /* legacy_close_lock_wait_observer_v4 */ activity AS MATERIALIZED (
 SELECT pid,state,COALESCE(backend_type,'') AS backend_type,backend_start,
        COALESCE(query='<insufficient privilege>',false) AS permission_denied,
        query_start IS NULL AND state_change IS NULL AS no_activity_timestamps,
        COALESCE(wait_event_type,'') AS wait_type,
        COALESCE(wait_event,'') AS wait_event
 FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid()
   AND (backend_type='client backend' OR backend_type IS NULL)
), waiting AS MATERIALIZED (
 SELECT held_lock.pid,array_agg(DISTINCT held_lock.locktype ORDER BY held_lock.locktype) AS lock_types
 FROM pg_locks AS held_lock JOIN activity USING(pid)
 WHERE NOT held_lock.granted GROUP BY held_lock.pid
), classified AS MATERIALIZED (
 SELECT activity.pid,COALESCE(state,'') AS state,backend_type,backend_start,
        permission_denied,no_activity_timestamps,wait_type,wait_event,
        COALESCE(waiting.lock_types,ARRAY[]::text[]) AS lock_types,
        COALESCE(state='active',false) OR wait_type='Lock' OR waiting.pid IS NOT NULL AS probe_edges
 FROM activity LEFT JOIN waiting USING(pid)
)
SELECT pid,state,wait_type,wait_event,
       CASE WHEN probe_edges THEN pg_blocking_pids(pid) ELSE NULL::integer[] END,
       lock_types,probe_edges,backend_type,backend_start,permission_denied,no_activity_timestamps
FROM classified`

// Opaque backend ids exist only in the in-memory sample and the synthetic
// positive-control edge. Reports contain counts and finite event names alone.
type legacyCloseDBWaitBackend struct {
	pid                  int32
	state                string
	waitType             string
	waitEvent            string
	blockers             []int32
	lockTypes            []string
	edgeProbed           bool
	backendType          string
	backendStart         *time.Time
	permissionDenied     bool
	noActivityTimestamps bool
}

// Backend identity never enters a report. The start timestamp prevents a reused
// PID from becoming apparent evidence that an unavailable backend recovered.
type legacyCloseDBWaitIdentity struct {
	pid     int32
	started int64
}

func (self legacyCloseDBWaitBackend) identity() (legacyCloseDBWaitIdentity, bool) {
	if self.backendStart == nil || self.backendType == "" {
		return legacyCloseDBWaitIdentity{}, false
	}
	return legacyCloseDBWaitIdentity{pid: self.pid, started: self.backendStart.UnixMicro()}, true
}

type legacyCloseDBWaitVisibilityEvent struct {
	Sample                 int64 `json:"sample"`
	SinceStartNs           int64 `json:"since_start_ns"`
	StateNull              bool  `json:"state_null"`
	StateDisabled          bool  `json:"state_disabled"`
	BackendTypeUnavailable bool  `json:"backend_type_unavailable"`
	PermissionDenied       bool  `json:"permission_denied"`
	NoActivityTimestamps   bool  `json:"no_activity_timestamps"`
	PreviouslySeenIdentity bool  `json:"previously_seen_identity"`
}

type legacyCloseDBWaitGapEvent struct {
	Sample                         int64 `json:"sample"`
	PreviousQueryStartSinceStartNs int64 `json:"previous_query_start_since_start_ns"`
	QueryStartSinceStartNs         int64 `json:"query_start_since_start_ns"`
	QueryFinishSinceStartNs        int64 `json:"query_finish_since_start_ns"`
	QueryStartedUTCUnixNs          int64 `json:"query_started_utc_unix_ns"`
	QueryFinishedUTCUnixNs         int64 `json:"query_finished_utc_unix_ns"`
	QueryWallNs                    int64 `json:"query_wall_ns"`
	BlindIntervalNs                int64 `json:"blind_interval_ns"`
}

type legacyCloseDBWaitEvent struct {
	Sample             int64    `json:"sample"`
	SinceStartNs       int64    `json:"since_start_ns"`
	WaitType           string   `json:"wait_type"`
	WaitEvent          string   `json:"wait_event"`
	BlockingEdges      int      `json:"blocking_edges"`
	UngrantedLockTypes []string `json:"ungranted_lock_types"`
}

type legacyCloseDBWaitReport struct {
	Method                            string                             `json:"method"`
	NominalPeriodNs                   int64                              `json:"nominal_period_ns"`
	CoverageLimitNs                   int64                              `json:"coverage_limit_ns"`
	QueryTimeoutNs                    int64                              `json:"query_timeout_ns"`
	DirectObserverConnections         int                                `json:"direct_observer_connections"`
	PreparedStatement                 bool                               `json:"prepared_statement"`
	SetupPrepareWallNs                int64                              `json:"setup_prepare_wall_ns"`
	Samples                           int64                              `json:"samples"`
	SampleErrors                      int64                              `json:"sample_errors"`
	WindowWallNs                      int64                              `json:"window_wall_ns"`
	WindowStartedUTCUnixNs            int64                              `json:"window_started_utc_unix_ns"`
	QueryWallTotalNs                  int64                              `json:"query_wall_total_ns"`
	QueryWallMaxNs                    int64                              `json:"query_wall_max_ns"`
	SampleStartGapMaxNs               int64                              `json:"sample_start_gap_max_ns"`
	ConservativeBlindIntervalNs       int64                              `json:"conservative_blind_interval_ns"`
	CoverageExceededIntervals         int64                              `json:"coverage_exceeded_intervals"`
	CoverageGapEvents                 []legacyCloseDBWaitGapEvent        `json:"coverage_gap_events"`
	OmittedCoverageGapEvents          int64                              `json:"omitted_coverage_gap_events"`
	BackendSamples                    int64                              `json:"backend_samples"`
	UnobservableBackendSamples        int64                              `json:"unobservable_backend_samples"`
	StateNullBackendSamples           int64                              `json:"state_null_backend_samples"`
	StateDisabledBackendSamples       int64                              `json:"state_disabled_backend_samples"`
	BackendTypeUnavailableSamples     int64                              `json:"backend_type_unavailable_samples"`
	PermissionDeniedBackendSamples    int64                              `json:"permission_denied_backend_samples"`
	StartupCompatibleNullSamples      int64                              `json:"startup_compatible_null_samples"`
	PreviouslySeenNullSamples         int64                              `json:"previously_seen_null_samples"`
	NullStateFollowedByVisibleSamples int64                              `json:"null_state_followed_by_visible_samples"`
	NullStateIdentityAbsentNextSample int64                              `json:"null_state_identity_absent_next_sample"`
	FirstVisibilityEvents             []legacyCloseDBWaitVisibilityEvent `json:"first_visibility_events"`
	OmittedVisibilityEvents           int64                              `json:"omitted_visibility_events"`
	ActiveBackendSamples              int64                              `json:"active_backend_samples"`
	WaitTypeBackendSamples            map[string]int64                   `json:"wait_type_backend_samples"`
	InternalWaitEventSamples          map[string]int64                   `json:"internal_wait_event_backend_samples"`
	BlockingEdgeProbeScope            string                             `json:"blocking_edge_probe_scope"`
	EdgeProbedBackendSamples          int64                              `json:"edge_probed_backend_samples"`
	EdgeUnprobedBackendSamples        int64                              `json:"edge_unprobed_backend_samples"`
	UnprobedBlockingEdgeSamples       *int64                             `json:"unprobed_blocking_edge_samples"`
	LockWaitBackendSamples            int64                              `json:"lock_wait_backend_samples"`
	SQLLockBackendSamples             int64                              `json:"sql_lock_backend_samples"`
	AdvisoryLockBackendSamples        int64                              `json:"postgres_advisory_lock_backend_samples"`
	BlockingBackendSamples            int64                              `json:"blocking_backend_samples"`
	BlockingEdgeSamples               int64                              `json:"blocking_edge_samples"`
	UngrantedLockBackendSamples       int64                              `json:"ungranted_lock_backend_samples"`
	OtherWaitBackendSamples           int64                              `json:"other_wait_backend_samples"`
	WaitEvents                        []legacyCloseDBWaitEvent           `json:"first_wait_events"`
	OmittedWaitEvents                 int64                              `json:"omitted_wait_events"`
	Joined                            bool                               `json:"joined"`
	ConnectionClosed                  bool                               `json:"connection_closed"`
	CloseFailed                       bool                               `json:"close_failed"`
	StopCause                         string                             `json:"stop_cause"`
	Qualifications                    []string                           `json:"qualifications"`
}

// One goroutine owns the direct session after start. A bounded final sample
// joins before reporting and connection close. The mutex protects aggregates
// only; it is never held across a database operation.
type legacyCloseDBWaitObserver struct {
	ctx              context.Context
	conn             *pgx.Conn
	stopRequest      chan struct{}
	done             chan struct{}
	closeOnce        sync.Once
	started          time.Time
	previous         time.Time
	stateLock        sync.Mutex
	value            legacyCloseDBWaitReport
	expectedEdge     [2]int32
	matchedEdge      int64
	previousBackends map[legacyCloseDBWaitIdentity]legacyCloseDBWaitBackend
}

func newLegacyCloseDBWaitObserver(t testing.TB, ctx context.Context) *legacyCloseDBWaitObserver {
	t.Helper()
	var config *pgx.ConnConfig
	// Clone the direct maintenance endpoint before protocol rebinding. This
	// extra session consumes no application pool slot and receives no modeled
	// Ready delay. Its actual query cost remains in the measured workload.
	server.MaintenanceDb(ctx, func(pooled server.PgConn) { config = pooled.Conn().Config().Copy() }, server.OptNoRetry())
	config.Tracer = nil
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["application_name"] = "urn_close_wait_observer"
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["statement_timeout"] = "200ms"
	config.RuntimeParams["lock_timeout"] = "200ms"
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	connect, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connect, config)
	if err != nil {
		t.Fatal("direct close wait observer could not connect")
	}
	// The static observer statement is prepared once before arming. Every
	// execution still receives a fresh autocommit snapshot; only parse/plan
	// work is reused. Querying the explicit name avoids reparsing this SQL on
	// every5ms sample, including with the connection's Exec query mode.
	prepareStarted := time.Now()
	_, err = conn.Prepare(connect, legacyCloseDBWaitPreparedName, legacyCloseDBWaitSQL)
	prepareWall := time.Since(prepareStarted)
	if err != nil {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = conn.Close(closeCtx)
		cancelClose()
		t.Fatal("direct close wait observer could not prepare its fixed statement")
	}
	return &legacyCloseDBWaitObserver{ctx: ctx, conn: conn, stopRequest: make(chan struct{}), done: make(chan struct{}),
		value: legacyCloseDBWaitReport{
			Method:          "same_database_activity_ungranted_locks_and_scoped_blocking_edges_v4",
			NominalPeriodNs: int64(legacyCloseDBWaitSamplePeriod), CoverageLimitNs: int64(legacyCloseDBWaitCoverageLimit),
			QueryTimeoutNs: int64(legacyCloseDBWaitQueryTimeout), DirectObserverConnections: 1,
			PreparedStatement: true, SetupPrepareWallNs: prepareWall.Nanoseconds(),
			WaitTypeBackendSamples:   map[string]int64{},
			InternalWaitEventSamples: map[string]int64{},
			BlockingEdgeProbeScope:   "active OR wait_event_type=Lock OR an ungranted lock was sampled; all other backend edges are unprobed and unknown",
			Qualifications: []string{
				"Zero means no Lock wait or ungranted lock was sampled in any visible backend, and no blocking edge was found among explicitly probed backends. Unprobed edges are unknown, not zero; waits entirely between samples may be missed.",
				"The conservative blind interval spans the previous query start through the current query finish, and includes query latency plus scheduler delay; qualification rejects intervals above50ms.",
				"The5ms cadence and50ms coverage ceiling qualify the sampler, not application I/O timeouts. A failed or incomplete observer fails this gate.",
				"SQL row/relation/transaction waits and PostgreSQL advisory waits are reported separately. Model nonwaiting advisory refusals remain separate existing counters.",
				"Application pool queue time is not measured by pg_stat_activity. Other wait types, including ClientRead and LWLock, are reported as samples rather than SQL lock waits.",
				"System views and blocking-edge functions can change during one observation. These are backend sample counts, never distinct wait counts or lock-duration measurements; missing activity visibility fails qualification.",
				"Rows whose backend type is hidden are retained and fail visibility qualification. NULL state, disabled activity tracking and explicit permission denial are reported separately. A new NULL-state backend with no activity timestamps is startup-compatible, not proven safe; every unavailable sample still fails this gate.",
				"Adjacent-sample backend continuity uses private PID and backend-start identity. A later visible state or absence cannot prove what happened during an unavailable sample. Coverage-gap timestamps allow independent CPU-throttling correlation but establish no causal attribution on their own.",
				"The fixed query materializes activity and ungranted locks once each. Blocking-edge functions run only for sampled active backends, Lock waiters or backends with ungranted locks; a backend that becomes blocked after classification may be unprobed until a later sample. That limitation is explicit and the50ms coverage ceiling remains unchanged.",
				"One named statement is prepared before arming; its setup wall is reported separately. Each execution still uses a fresh autocommit snapshot; actual blocked and released controls test that freshness. Query wall counts include execution/protocol/scheduling, not preparation or PostgreSQL CPU attribution.",
				"One extra direct session performs the observer queries. Query wall totals are an overlapping cost indicator and cannot be subtracted from concurrent workload wall to invent an unobserved throughput result.",
				"Baseline and candidate must use this identical observer and resources. Sampling can perturb the workload; no absolute zero-wait or unobserved-performance claim is supported.",
			},
		}}
}

func (self *legacyCloseDBWaitObserver) start(t testing.TB) {
	t.Helper()
	self.started = time.Now()
	self.value.WindowStartedUTCUnixNs = self.started.UnixNano()
	if !self.sample() {
		close(self.done)
		failed := self.snapshot()
		t.Fatalf("direct close wait observer could not establish initial coverage: samples=%d errors=%d query_wall_ns=%d max_blind_interval_ns=%d",
			failed.Samples, failed.SampleErrors, failed.QueryWallTotalNs, failed.ConservativeBlindIntervalNs)
	}
	go func() {
		defer close(self.done)
		tick := time.NewTicker(legacyCloseDBWaitSamplePeriod)
		defer tick.Stop()
		for {
			select {
			case <-self.stopRequest:
				self.sample()
				self.stateLock.Lock()
				self.value.Joined = true
				self.value.StopCause = "final_sample_joined"
				self.value.WindowWallNs = time.Since(self.started).Nanoseconds()
				self.stateLock.Unlock()
				return
			case <-self.ctx.Done():
				self.stateLock.Lock()
				self.value.StopCause = "parent_context_done"
				self.value.WindowWallNs = time.Since(self.started).Nanoseconds()
				self.stateLock.Unlock()
				return
			case <-tick.C:
				if !self.sample() {
					self.stateLock.Lock()
					self.value.StopCause = "sample_failed"
					self.value.WindowWallNs = time.Since(self.started).Nanoseconds()
					self.stateLock.Unlock()
					return
				}
			}
		}
	}()
}

func (self *legacyCloseDBWaitObserver) sample() bool {
	started := time.Now()
	queryCtx, cancel := context.WithTimeout(self.ctx, legacyCloseDBWaitQueryTimeout)
	defer cancel()
	rows, err := self.conn.Query(queryCtx, legacyCloseDBWaitPreparedName)
	var backends []legacyCloseDBWaitBackend
	if err == nil {
		for rows.Next() {
			var row legacyCloseDBWaitBackend
			if err = rows.Scan(&row.pid, &row.state, &row.waitType, &row.waitEvent, &row.blockers, &row.lockTypes, &row.edgeProbed,
				&row.backendType, &row.backendStart, &row.permissionDenied, &row.noActivityTimestamps); err != nil {
				break
			}
			backends = append(backends, row)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	finished := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	queryWall := finished.Sub(started).Nanoseconds()
	self.value.QueryWallTotalNs += queryWall
	self.value.QueryWallMaxNs = max(self.value.QueryWallMaxNs, queryWall)
	blind := queryWall
	if !self.previous.IsZero() {
		self.value.SampleStartGapMaxNs = max(self.value.SampleStartGapMaxNs, started.Sub(self.previous).Nanoseconds())
		blind = finished.Sub(self.previous).Nanoseconds()
	}
	self.value.ConservativeBlindIntervalNs = max(self.value.ConservativeBlindIntervalNs, blind)
	if blind > int64(legacyCloseDBWaitCoverageLimit) {
		self.value.CoverageExceededIntervals++
		if len(self.value.CoverageGapEvents) < legacyCloseDBWaitGapEventLimit {
			previous := self.previous
			if previous.IsZero() {
				previous = started
			}
			self.value.CoverageGapEvents = append(self.value.CoverageGapEvents, legacyCloseDBWaitGapEvent{
				Sample:                         self.value.Samples + self.value.SampleErrors + 1,
				PreviousQueryStartSinceStartNs: previous.Sub(self.started).Nanoseconds(),
				QueryStartSinceStartNs:         started.Sub(self.started).Nanoseconds(), QueryFinishSinceStartNs: finished.Sub(self.started).Nanoseconds(),
				QueryStartedUTCUnixNs: started.UnixNano(), QueryFinishedUTCUnixNs: finished.UnixNano(),
				QueryWallNs: queryWall, BlindIntervalNs: blind,
			})
		} else {
			self.value.OmittedCoverageGapEvents++
		}
	}
	self.previous = started
	if err != nil {
		self.value.SampleErrors++
		return false
	}
	self.value.Samples++
	currentBackends := map[legacyCloseDBWaitIdentity]legacyCloseDBWaitBackend{}
	for _, row := range backends {
		self.value.BackendSamples++
		identity, identifiable := row.identity()
		_, previouslySeen := self.previousBackends[identity]
		previouslySeen = identifiable && previouslySeen
		if identifiable {
			currentBackends[identity] = row
		}
		if row.state == "" {
			self.value.StateNullBackendSamples++
			if previouslySeen {
				self.value.PreviouslySeenNullSamples++
			}
			if !previouslySeen && identifiable && !row.permissionDenied && row.noActivityTimestamps {
				self.value.StartupCompatibleNullSamples++
			}
		}
		if row.state == "disabled" {
			self.value.StateDisabledBackendSamples++
		}
		if row.backendType == "" {
			self.value.BackendTypeUnavailableSamples++
		}
		if row.permissionDenied {
			self.value.PermissionDeniedBackendSamples++
		}
		if row.state == "" || row.state == "disabled" || row.backendType == "" || row.permissionDenied {
			self.value.UnobservableBackendSamples++
			if len(self.value.FirstVisibilityEvents) < legacyCloseDBWaitEventLimit {
				self.value.FirstVisibilityEvents = append(self.value.FirstVisibilityEvents, legacyCloseDBWaitVisibilityEvent{
					Sample: self.value.Samples, SinceStartNs: finished.Sub(self.started).Nanoseconds(),
					StateNull: row.state == "", StateDisabled: row.state == "disabled", BackendTypeUnavailable: row.backendType == "",
					PermissionDenied: row.permissionDenied, NoActivityTimestamps: row.noActivityTimestamps, PreviouslySeenIdentity: previouslySeen,
				})
			} else {
				self.value.OmittedVisibilityEvents++
			}
		}
		if row.state == "active" {
			self.value.ActiveBackendSamples++
		}
		waitType := row.waitType
		if waitType == "" {
			waitType = "none"
		}
		self.value.WaitTypeBackendSamples[waitType]++
		if row.waitType == "LWLock" || row.waitType == "IO" {
			self.value.InternalWaitEventSamples[row.waitType+":"+row.waitEvent]++
		}
		if row.edgeProbed {
			self.value.EdgeProbedBackendSamples++
		} else {
			self.value.EdgeUnprobedBackendSamples++
		}
		isLock := row.waitType == "Lock"
		isAdvisory := strings.EqualFold(row.waitEvent, "advisory") || slices.Contains(row.lockTypes, "advisory")
		if isLock {
			self.value.LockWaitBackendSamples++
		}
		if len(row.blockers) > 0 {
			self.value.BlockingBackendSamples++
			self.value.BlockingEdgeSamples += int64(len(row.blockers))
		}
		if len(row.lockTypes) > 0 {
			self.value.UngrantedLockBackendSamples++
		}
		if isLock || len(row.blockers) > 0 || len(row.lockTypes) > 0 {
			if isAdvisory {
				self.value.AdvisoryLockBackendSamples++
			} else {
				self.value.SQLLockBackendSamples++
			}
			if row.pid == self.expectedEdge[0] && slices.Contains(row.blockers, self.expectedEdge[1]) {
				self.matchedEdge++
			}
			if len(self.value.WaitEvents) < legacyCloseDBWaitEventLimit {
				self.value.WaitEvents = append(self.value.WaitEvents, legacyCloseDBWaitEvent{
					Sample: self.value.Samples, SinceStartNs: finished.Sub(self.started).Nanoseconds(),
					WaitType: row.waitType, WaitEvent: row.waitEvent, BlockingEdges: len(row.blockers), UngrantedLockTypes: slices.Clone(row.lockTypes),
				})
			} else {
				self.value.OmittedWaitEvents++
			}
		} else if row.waitType != "" {
			self.value.OtherWaitBackendSamples++
		}
	}
	for identity, previous := range self.previousBackends {
		if previous.state != "" {
			continue
		}
		if current, present := currentBackends[identity]; !present {
			self.value.NullStateIdentityAbsentNextSample++
		} else if current.state != "" && current.state != "disabled" && !current.permissionDenied {
			self.value.NullStateFollowedByVisibleSamples++
		}
	}
	self.previousBackends = currentBackends
	return true
}

func (self *legacyCloseDBWaitObserver) close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		if !self.started.IsZero() {
			close(self.stopRequest)
			<-self.done
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(self.ctx), time.Second)
		defer cancel()
		err := self.conn.Close(closeCtx)
		self.stateLock.Lock()
		self.value.ConnectionClosed = self.conn.IsClosed()
		self.value.CloseFailed = err != nil
		self.stateLock.Unlock()
	})
}

func (self *legacyCloseDBWaitObserver) snapshot() legacyCloseDBWaitReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := self.value
	result.WaitEvents = slices.Clone(result.WaitEvents)
	result.CoverageGapEvents = slices.Clone(result.CoverageGapEvents)
	result.FirstVisibilityEvents = slices.Clone(result.FirstVisibilityEvents)
	result.WaitTypeBackendSamples = map[string]int64{}
	for name, count := range self.value.WaitTypeBackendSamples {
		result.WaitTypeBackendSamples[name] = count
	}
	result.InternalWaitEventSamples = map[string]int64{}
	for name, count := range self.value.InternalWaitEventSamples {
		result.InternalWaitEventSamples[name] = count
	}
	return result
}

func (self legacyCloseDBWaitReport) zeroWaitError() error {
	if !self.Joined || !self.ConnectionClosed || self.CloseFailed || self.StopCause != "final_sample_joined" || self.Samples < 3 || self.SampleErrors != 0 ||
		self.UnobservableBackendSamples != 0 || self.WindowWallNs <= 0 || self.QueryWallTotalNs <= 0 ||
		self.StateNullBackendSamples != 0 || self.StateDisabledBackendSamples != 0 || self.BackendTypeUnavailableSamples != 0 || self.PermissionDeniedBackendSamples != 0 ||
		self.EdgeProbedBackendSamples+self.EdgeUnprobedBackendSamples != self.BackendSamples || self.UnprobedBlockingEdgeSamples != nil ||
		self.CoverageExceededIntervals != 0 || self.ConservativeBlindIntervalNs > int64(legacyCloseDBWaitCoverageLimit) {
		return fmt.Errorf("direct PostgreSQL lock observer has incomplete or insufficient sampling coverage")
	}
	if self.LockWaitBackendSamples != 0 || self.BlockingBackendSamples != 0 || self.BlockingEdgeSamples != 0 ||
		self.UngrantedLockBackendSamples != 0 || self.SQLLockBackendSamples != 0 || self.AdvisoryLockBackendSamples != 0 || self.OmittedWaitEvents != 0 {
		return fmt.Errorf("direct PostgreSQL lock observer sampled an actual wait or blocking edge")
	}
	return nil
}

func TestContractCloseDBWaitObserverDetectsRealRowWait(t *testing.T) {
	legacyCloseDBWaitRequireBlockedControl(t, false)
}

func TestContractCloseDBWaitObserverDetectsRealAdvisoryWait(t *testing.T) {
	legacyCloseDBWaitRequireBlockedControl(t, true)
}

// Disabled tracking suppresses current wait information. It must not become
// an idle/zero-wait success merely because no lock flag was reported.
func TestContractCloseDBWaitObserverRejectsDisabledActivity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		backend := acquireContractLifecycleTestConnection(t, ctx)
		defer backend.Release()
		server.RaisePgResult(backend.Exec(ctx, `SET track_activities=off`))
		defer func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			server.RaisePgResult(backend.Exec(cleanup, `RESET track_activities`))
		}()
		observer := newLegacyCloseDBWaitObserver(t, ctx)
		defer observer.close()
		observer.start(t)
		legacyCloseDBWaitRequireThreeSamples(t, ctx, observer)
		observer.close()
		result := observer.snapshot()
		if !result.Joined || !result.ConnectionClosed || result.SampleErrors != 0 || result.StateDisabledBackendSamples < 3 || result.UnobservableBackendSamples < 3 {
			t.Fatal("direct observer lost the real disabled-activity control")
		}
		if result.zeroWaitError() == nil {
			t.Fatal("disabled activity tracking passed the zero-wait gate")
		}
	})
}

// PostgreSQL hides backend_type as well as state from an unauthorized reader.
// Keep those rows in scope. SET ROLE affects this test's direct session only;
// the built-in signal role has no statistics visibility over ordinary owners.
func TestContractCloseDBWaitObserverRejectsHiddenBackendType(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		backend := acquireContractLifecycleTestConnection(t, ctx)
		defer backend.Release()
		server.RaisePgResult(backend.Exec(ctx, `SELECT 1`))
		observer := newLegacyCloseDBWaitObserver(t, ctx)
		defer observer.close()
		server.RaisePgResult(observer.conn.Exec(ctx, `SET ROLE pg_signal_backend`))
		observer.start(t)
		legacyCloseDBWaitRequireThreeSamples(t, ctx, observer)
		observer.close()
		result := observer.snapshot()
		if !result.Joined || !result.ConnectionClosed || result.SampleErrors != 0 || result.BackendTypeUnavailableSamples < 3 || result.PermissionDeniedBackendSamples < 3 || result.UnobservableBackendSamples < 3 {
			t.Fatal("direct observer excluded real permission-hidden backend rows")
		}
		if result.StartupCompatibleNullSamples != 0 || result.zeroWaitError() == nil {
			t.Fatal("hidden activity became a startup exception or zero-wait success")
		}
	})
}

func legacyCloseDBWaitRequireThreeSamples(t testing.TB, ctx context.Context, observer *legacyCloseDBWaitObserver) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for observer.snapshot().Samples < 3 {
		select {
		case <-tick.C:
		case <-observer.done:
			t.Fatal("direct observer stopped before the visibility-control barrier")
		case <-ctx.Done():
			t.Fatal("direct observer did not complete the visibility-control barrier")
		}
	}
}

// A real holder stays locked until at least two exact waiter/holder edges are
// sampled. A nonwaiting probe first proves that refusal alone is not a wait.
// No sleep duration or task progress stands in for that actual database edge.
func legacyCloseDBWaitRequireBlockedControl(t *testing.T, advisory bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		waiter := acquireContractLifecycleTestConnection(t, ctx)
		defer waiter.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		var releaseErr error
		release := func() {
			releaseOnce.Do(func() {
				cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				defer cancelCleanup()
				releaseErr = held.Rollback(cleanupCtx)
			})
		}
		defer release()
		if advisory {
			server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(2048,6408)`))
			var admitted bool
			server.Raise(waiter.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(2048,6408)`).Scan(&admitted))
			if admitted {
				t.Fatal("synthetic nonwaiting advisory refusal unexpectedly acquired ownership")
			}
		} else {
			server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, fixture.balanceId))
			var found int
			server.Raise(waiter.QueryRow(ctx, `SELECT count(*) FROM (SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE SKIP LOCKED) AS probe`, fixture.balanceId).Scan(&found))
			if found != 0 {
				t.Fatal("synthetic nonwaiting row probe acquired the held row")
			}
		}
		observer := newLegacyCloseDBWaitObserver(t, ctx)
		defer observer.close()
		observer.expectedEdge = [2]int32{int32(waiter.Conn().PgConn().PID()), int32(holder.Conn().PgConn().PID())}
		observer.start(t)
		first := observer.snapshot()
		if first.LockWaitBackendSamples != 0 || first.BlockingEdgeSamples != 0 || first.UngrantedLockBackendSamples != 0 {
			t.Fatal("nonwaiting refusal or an idle held lock was mislabeled as an actual wait")
		}
		if first.EdgeUnprobedBackendSamples == 0 || first.UnprobedBlockingEdgeSamples != nil ||
			first.EdgeProbedBackendSamples+first.EdgeUnprobedBackendSamples != first.BackendSamples {
			t.Fatal("idle held-owner edges were not retained as explicitly unprobed and unknown")
		}
		done := make(chan error, 1)
		go func() {
			done <- func() (returnErr error) {
				waiting, err := waiter.Begin(ctx)
				if err != nil {
					return err
				}
				defer func() {
					cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					defer cancelCleanup()
					if err := waiting.Rollback(cleanupCtx); returnErr == nil {
						returnErr = err
					}
				}()
				if _, err = waiting.Exec(ctx, `SET LOCAL statement_timeout='10s'; SET LOCAL lock_timeout='10s'`); err != nil {
					return err
				}
				if advisory {
					_, err = waiting.Exec(ctx, `SELECT pg_advisory_xact_lock(2048,6408)`)
				} else {
					_, err = waiting.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, fixture.balanceId)
				}
				return err
			}()
		}()
		joined := false
		defer func() {
			release()
			if !joined {
				cancel()
				<-done
			}
		}()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			observer.stateLock.Lock()
			matched := observer.matchedEdge
			observer.stateLock.Unlock()
			if matched >= 2 {
				break
			}
			select {
			case <-tick.C:
			case <-observer.done:
				t.Fatal("direct observer stopped before identifying the real blocked edge")
			case <-done:
				joined = true
				t.Fatal("synthetic waiter ended before its exact blocked edge was sampled")
			case <-ctx.Done():
				t.Fatal("direct observer did not identify the held waiter before the control deadline")
			}
		}
		release()
		if releaseErr != nil {
			t.Fatal("synthetic holder did not acknowledge release")
		}
		if err := <-done; err != nil {
			joined = true
			t.Fatal("released synthetic waiter did not complete")
		}
		joined = true
		observer.close()
		result := observer.snapshot()
		if !result.Joined || result.SampleErrors != 0 || result.BlockingEdgeSamples < 2 || result.LockWaitBackendSamples < 2 || result.UngrantedLockBackendSamples < 2 {
			t.Fatal("direct observer lost the positive lock-wait control")
		}
		if result.EdgeProbedBackendSamples < 2 || result.EdgeUnprobedBackendSamples == 0 || result.UnprobedBlockingEdgeSamples != nil {
			t.Fatal("direct observer lost performed-edge probes or fabricated unprobed edges")
		}
		if advisory && (result.AdvisoryLockBackendSamples < 2 || result.SQLLockBackendSamples != 0) ||
			!advisory && (result.SQLLockBackendSamples < 2 || result.AdvisoryLockBackendSamples != 0) {
			t.Fatal("direct observer merged SQL and PostgreSQL advisory wait classes")
		}
		if result.zeroWaitError() == nil {
			t.Fatal("actual blocked control incorrectly passed the healthy zero-wait gate")
		}
		// A separately armed window after release must report clean current
		// state; the first observer's cumulative counts must remain positive.
		clean := newLegacyCloseDBWaitObserver(t, ctx)
		defer clean.close()
		clean.start(t)
		for clean.snapshot().Samples < 3 {
			select {
			case <-tick.C:
			case <-clean.done:
				t.Fatal("released-state observer stopped before its coverage barrier")
			case <-ctx.Done():
				t.Fatal("released-state observer did not complete its coverage barrier")
			}
		}
		clean.close()
		if err := clean.snapshot().zeroWaitError(); err != nil {
			t.Fatal("released-state observer did not pass its fresh zero-wait window", err)
		}
		t.Logf("close_db_wait_positive_control advisory=%t exact_edges_observed=true refused_probe_not_wait=true released_window_zero=true query_wall_ns=%d max_blind_interval_ns=%d; this finite control does not prove absence of sub-gap waits",
			advisory, result.QueryWallTotalNs, result.ConservativeBlindIntervalNs)
	})
}

// Absence of errors or sampled locks cannot turn a blind or unfinished window
// into acceptance. Fixed synthetic summaries make these gate controls timeless.
func TestContractCloseDBWaitObserverRejectsIncompleteCoverage(t *testing.T) {
	good := legacyCloseDBWaitReport{Joined: true, ConnectionClosed: true, StopCause: "final_sample_joined", Samples: 3,
		WindowWallNs: int64(20 * time.Millisecond), QueryWallTotalNs: int64(time.Millisecond),
		ConservativeBlindIntervalNs: int64(legacyCloseDBWaitSamplePeriod)}
	if good.zeroWaitError() != nil {
		t.Fatal("qualified synthetic zero window did not pass")
	}
	for _, corrupt := range []func(*legacyCloseDBWaitReport){
		func(r *legacyCloseDBWaitReport) { r.Joined = false },
		func(r *legacyCloseDBWaitReport) { r.ConnectionClosed = false },
		func(r *legacyCloseDBWaitReport) { r.CloseFailed = true },
		func(r *legacyCloseDBWaitReport) { r.StopCause = "parent_context_done" },
		func(r *legacyCloseDBWaitReport) { r.Samples = 2 },
		func(r *legacyCloseDBWaitReport) { r.SampleErrors = 1 },
		func(r *legacyCloseDBWaitReport) { r.UnobservableBackendSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.StateNullBackendSamples = 1; r.StartupCompatibleNullSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.StateDisabledBackendSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.BackendTypeUnavailableSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.PermissionDeniedBackendSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.BackendSamples = 1 },
		func(r *legacyCloseDBWaitReport) { var fabricated int64; r.UnprobedBlockingEdgeSamples = &fabricated },
		func(r *legacyCloseDBWaitReport) { r.CoverageExceededIntervals = 1 },
		func(r *legacyCloseDBWaitReport) {
			r.ConservativeBlindIntervalNs = int64(legacyCloseDBWaitCoverageLimit) + 1
		},
		func(r *legacyCloseDBWaitReport) { r.LockWaitBackendSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.BlockingEdgeSamples = 1 },
		func(r *legacyCloseDBWaitReport) { r.UngrantedLockBackendSamples = 1 },
	} {
		bad := good
		corrupt(&bad)
		if bad.zeroWaitError() == nil {
			t.Fatal("incomplete or nonzero synthetic observer summary passed")
		}
	}
}

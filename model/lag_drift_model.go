package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// lag.dev drift tests (lag.dev/DRIFTTEST.md P, E1, K3, R1).
//
//   - lag_drift_test is one token an Embed network allocated (P2). The token
//     is kept only as its SHA-256 (K5). The visitor's address is kept only as
//     the server's peppered hash of its /29 (/56), server.ClientIpHashForAddr,
//     and only while a run can still start: the hash goes when the token
//     closes or expires (K3, R1). A start's address is compared with it in
//     memory and never stored. The row also holds the verdict over the
//     token's window, replaced after each completed run (P4, V7).
//   - lag_drift_run is one run (P6): a lag_track of kind drift (E1), whose
//     tickets and reports are lag.dev's own (A6, A7), with the run's state,
//     the browser's stop and the one the reports support (V10), and the
//     extenders it was ticketed with, placed as they were then (E2).
//   - lag_drift_network is a network's allowed parent origins (P3).
//
// A drift track's client fields are a lag track's (A5), looked up from the
// start request. Tests and runs are kept for the drift retention, 7 days, and
// their tracks, tickets and results follow the lag retention (R1). Timestamps
// are naive columns holding utc, as everywhere else in this schema.

// What a lag_track is: a lag.dev visitor's track, or a drift test's run (E1).
const (
	LagTrackKindLag   = "lag"
	LagTrackKindDrift = "drift"
)

// A run's states (P6): measuring from start, processing from its finish (or
// processing_after without one), complete once its reports are in or the
// report wait is over.
const (
	LagDriftRunStateStarted    = "started"
	LagDriftRunStateProcessing = "processing"
	LagDriftRunStateComplete   = "complete"
)

// What processing made of a run of a test that failed by an internal error
// (P4): it is closed with the test, so no sweep waits on it.
const LagDriftRunOutcomeFailed = "failed"

// Why a start was refused by the token's own state (P5).
const (
	// unknown, or never started before its expiry
	LagDriftRefusalExpired = "expired"
	// out of runs, past the retest window, or failed
	LagDriftRefusalClosed = "closed"
	// another run of the token is still measuring
	LagDriftRefusalBusy = "busy"
)

// One drift test as stored (P2).
type LagDriftTest struct {
	DriftTestId server.Id
	NetworkId   server.Id
	// SHA-256 of the token text
	TokenHash []byte
	// the peppered hash of client_ip's /29 (/56); nil once no run can start
	ClientIpHash []byte
	// 4 or 6, client_ip's family
	ClientIpFamily int
	// client_ip's country, looked up at allocation, which every run is judged
	// against (K3); "" when GeoLite2 places it nowhere
	ClientIpCountryCode string
	ClientIpCountryName string
	// the origins the token narrows the network's list to; nil for none
	ParentOrigins []string
	Confidence    float64
	Reference     string
	CreatedAt     time.Time
	// the first run must start before this
	ExpiresAt      time.Time
	FirstStartedAt *time.Time
	RetestUntil    *time.Time
	RunCount       int
	UpdatedAt      time.Time
	FailedAt       *time.Time
	// an operator revoked the token: it starts no more runs
	RevokeTime *time.Time
	// the verdict over the window as json (P4), nil before any run completed
	ResultJson []byte
	ResultAt   *time.Time
}

// One extender a run was ticketed with, placed where the sample placed it
// when the run started (E2, V3).
type LagDriftRunExtender struct {
	Position     int       `json:"position"`
	ExtenderId   server.Id `json:"extender_id"`
	CountryCode  string    `json:"country_code"`
	Latitude     float64   `json:"latitude"`
	Longitude    float64   `json:"longitude"`
	PlaceErrorKm float64   `json:"place_error_km"`
}

// One run as stored (P6).
type LagDriftRun struct {
	DriftTestId server.Id
	Run         int
	TrackId     server.Id
	State       string
	StartedAt   time.Time
	// when the run began processing: its finish, or processing_after
	// without one
	FinishedAt  *time.Time
	CompletedAt *time.Time
	// the browser's stop (V10), "" without a finish
	Stop string
	// the family the start came from
	IpFamily int
	// nil when the start's family is not client_ip's (K3)
	ClientIpMatches *bool
	IpCountryCode   string
	IpCountryName   string
	Extenders       []*LagDriftRunExtender
	// once complete: counted, or why the run stays out of the window
	Outcome string
	// once complete: the stop the reports support, "" for none
	SupportedStop string
	Answered      *int
	Usable        *int
	MeasuredMs    *int
	// the region's k and k_early the run is judged at, from its start (V11);
	// nil for a run started before them
	K      *float64
	KEarly *float64
}

func (self *LagDriftRun) Complete() bool {
	return self.State == LagDriftRunStateComplete
}

// A test row's columns, in the order scanLagDriftTest reads them.
const lagDriftTestColumns = `
	drift_test_id,
	network_id,
	token_hash,
	client_ip_hash,
	client_ip_family,
	client_ip_country_code,
	client_ip_country_name,
	parent_origins,
	confidence,
	reference,
	created_at,
	expires_at,
	first_started_at,
	retest_until,
	run_count,
	updated_at,
	failed_at,
	revoke_time,
	result_json,
	result_at
`

func scanLagDriftTest(result server.PgResult) *LagDriftTest {
	test := &LagDriftTest{}
	var reference, clientIpCountryCode, clientIpCountryName *string
	server.Raise(result.Scan(
		&test.DriftTestId,
		&test.NetworkId,
		&test.TokenHash,
		&test.ClientIpHash,
		&test.ClientIpFamily,
		&clientIpCountryCode,
		&clientIpCountryName,
		&test.ParentOrigins,
		&test.Confidence,
		&reference,
		&test.CreatedAt,
		&test.ExpiresAt,
		&test.FirstStartedAt,
		&test.RetestUntil,
		&test.RunCount,
		&test.UpdatedAt,
		&test.FailedAt,
		&test.RevokeTime,
		&test.ResultJson,
		&test.ResultAt,
	))
	if reference != nil {
		test.Reference = *reference
	}
	if clientIpCountryCode != nil {
		test.ClientIpCountryCode = *clientIpCountryCode
	}
	if clientIpCountryName != nil {
		test.ClientIpCountryName = *clientIpCountryName
	}
	return test
}

// A run row's columns, in the order scanLagDriftRun reads them.
const lagDriftRunColumns = `
	drift_test_id,
	run,
	track_id,
	state,
	started_at,
	finished_at,
	completed_at,
	stop,
	ip_family,
	client_ip_matches,
	ip_country_code,
	ip_country_name,
	extenders_json,
	outcome,
	supported_stop,
	answered,
	usable,
	measured_ms,
	k,
	k_early
`

func scanLagDriftRun(result server.PgResult) *LagDriftRun {
	run := &LagDriftRun{}
	var stop, ipCountryCode, ipCountryName, outcome, supportedStop *string
	var extendersJson []byte
	server.Raise(result.Scan(
		&run.DriftTestId,
		&run.Run,
		&run.TrackId,
		&run.State,
		&run.StartedAt,
		&run.FinishedAt,
		&run.CompletedAt,
		&stop,
		&run.IpFamily,
		&run.ClientIpMatches,
		&ipCountryCode,
		&ipCountryName,
		&extendersJson,
		&outcome,
		&supportedStop,
		&run.Answered,
		&run.Usable,
		&run.MeasuredMs,
		&run.K,
		&run.KEarly,
	))
	for _, field := range []struct {
		value  *string
		target *string
	}{
		{stop, &run.Stop},
		{ipCountryCode, &run.IpCountryCode},
		{ipCountryName, &run.IpCountryName},
		{outcome, &run.Outcome},
		{supportedStop, &run.SupportedStop},
	} {
		if field.value != nil {
			*field.target = *field.value
		}
	}
	if 0 < len(extendersJson) {
		server.Raise(json.Unmarshal(extendersJson, &run.Extenders))
	}
	// json null reads as a nil slice: empty, as for no json
	if run.Extenders == nil {
		run.Extenders = []*LagDriftRunExtender{}
	}
	return run
}

// Stores a new drift test (P2).
func CreateLagDriftTest(ctx context.Context, test *LagDriftTest) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_drift_test (
				drift_test_id,
				network_id,
				token_hash,
				client_ip_hash,
				client_ip_family,
				parent_origins,
				confidence,
				reference,
				created_at,
				expires_at,
				updated_at,
				client_ip_country_code,
				client_ip_country_name
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $9, $11, $12)
			`,
			test.DriftTestId,
			test.NetworkId,
			test.TokenHash,
			test.ClientIpHash,
			test.ClientIpFamily,
			test.ParentOrigins,
			test.Confidence,
			nullString(test.Reference),
			test.CreatedAt.UTC(),
			test.ExpiresAt.UTC(),
			nullString(test.ClientIpCountryCode),
			nullString(test.ClientIpCountryName),
		))
	})
}

// One drift test, or nil when there is none (never allocated, or past
// retention).
func GetLagDriftTest(ctx context.Context, driftTestId server.Id) *LagDriftTest {
	var test *LagDriftTest
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+lagDriftTestColumns+` FROM lag_drift_test WHERE drift_test_id = $1`,
			driftTestId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				test = scanLagDriftTest(result)
			}
		})
	})
	return test
}

// The drift test of a token, by the token's SHA-256, or nil.
func GetLagDriftTestByTokenHash(ctx context.Context, tokenHash []byte) *LagDriftTest {
	var test *LagDriftTest
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+lagDriftTestColumns+` FROM lag_drift_test WHERE token_hash = $1`,
			tokenHash,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				test = scanLagDriftTest(result)
			}
		})
	})
	return test
}

// The drift test, locked for the rest of the transaction, or nil.
func GetLagDriftTestForUpdateInTx(ctx context.Context, tx server.PgTx, driftTestId server.Id) *LagDriftTest {
	var test *LagDriftTest
	result, err := tx.Query(
		ctx,
		`SELECT `+lagDriftTestColumns+` FROM lag_drift_test WHERE drift_test_id = $1 FOR UPDATE`,
		driftTestId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			test = scanLagDriftTest(result)
		}
	})
	return test
}

// Every run of a drift test, by run number.
func GetLagDriftRuns(ctx context.Context, driftTestId server.Id) []*LagDriftRun {
	runs := []*LagDriftRun{}
	server.Db(ctx, func(conn server.PgConn) {
		runs = getLagDriftRunsInConn(ctx, conn, driftTestId)
	})
	return runs
}

// Every run of a drift test, by run number, in the caller's transaction.
func GetLagDriftRunsInTx(ctx context.Context, tx server.PgTx, driftTestId server.Id) []*LagDriftRun {
	return getLagDriftRunsInConn(ctx, tx, driftTestId)
}

func getLagDriftRunsInConn(ctx context.Context, conn server.PgCanQuery, driftTestId server.Id) []*LagDriftRun {
	runs := []*LagDriftRun{}
	result, err := conn.Query(
		ctx,
		`SELECT `+lagDriftRunColumns+` FROM lag_drift_run WHERE drift_test_id = $1 ORDER BY run`,
		driftTestId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			runs = append(runs, scanLagDriftRun(result))
		}
	})
	return runs
}

// The run a track is, or nil when the track is no drift run.
func GetLagDriftRunByTrack(ctx context.Context, trackId server.Id) *LagDriftRun {
	var run *LagDriftRun
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+lagDriftRunColumns+` FROM lag_drift_run WHERE track_id = $1`,
			trackId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				run = scanLagDriftRun(result)
			}
		})
	})
	return run
}

// What a run's start asks of its drift test (P5, P6, K3).
type AddLagDriftRunArgs struct {
	DriftTestId server.Id
	// the start address's peppered hash and family, compared with the
	// token's in memory and never stored (K3)
	StartIpHash   [32]byte
	StartIpFamily int
	// the run's track, its client fields and its tickets (E1); the track's
	// network and client are the drift test's network and id
	Track         *LagTrack
	Tickets       []*LagTicket
	IpCountryCode string
	IpCountryName string
	Extenders     []*LagDriftRunExtender
	// the region's k and k_early for the run (V11)
	K            float64
	KEarly       float64
	Now          time.Time
	MaxRuns      int
	RetestWindow time.Duration
	// a run started this long ago measures no more (P6)
	ProcessingAfter time.Duration
}

// The run a start added, or why it was refused.
type AddLagDriftRunResult struct {
	// 0 when refused
	Run      int
	RunsLeft int
	Refusal  string
	// nil when the start's family is not client_ip's
	ClientIpMatches *bool
	RetestUntil     time.Time
}

// Adds the next run of a drift test, with its track and tickets, in one
// transaction on the locked test (P5, P6, E1): refused as expired when the
// first run comes after the token's expiry, as closed when the token is out
// of runs, past its retest window or failed, and as busy while another run is
// still measuring. The start's address is compared with client_ip by their
// peppered hashes, within one family (K3). The first run sets the retest
// window; the last run the token allows deletes its client_ip hash, since no
// run can start after it.
func AddLagDriftRun(ctx context.Context, args *AddLagDriftRunArgs) *AddLagDriftRunResult {
	var added *AddLagDriftRunResult
	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		added = &AddLagDriftRunResult{}
		now := args.Now.UTC()

		test := GetLagDriftTestForUpdateInTx(ctx, tx, args.DriftTestId)
		if test == nil {
			added.Refusal = LagDriftRefusalExpired
			return
		}
		if refusal := lagDriftStartRefusal(test, getLagDriftRunsInConn(ctx, tx, test.DriftTestId), now, args.MaxRuns, args.ProcessingAfter); refusal != "" {
			added.Refusal = refusal
			return
		}

		run := test.RunCount + 1
		if test.ClientIpFamily == args.StartIpFamily {
			matches := test.ClientIpHash != nil && bytes.Equal(test.ClientIpHash, args.StartIpHash[:])
			added.ClientIpMatches = &matches
		}
		retestUntil := now.Add(args.RetestWindow)
		if test.RetestUntil != nil {
			retestUntil = *test.RetestUntil
		}

		track := args.Track
		client := track.Client
		if client == nil {
			client = &LagTrackClient{}
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_track (
				track_id,
				network_id,
				client_id,
				started_at,
				client_asn,
				client_asn_org,
				client_country_code,
				client_region,
				client_city,
				client_latitude,
				client_longitude,
				automated,
				automated_reason,
				source,
				kind,
				drift_test_id,
				drift_run
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			`,
			track.TrackId,
			track.NetworkId,
			track.ClientId,
			track.StartedAt.UTC(),
			client.Asn,
			nullString(client.AsnOrg),
			nullString(client.CountryCode),
			nullString(client.Region),
			nullString(client.City),
			client.Latitude,
			client.Longitude,
			track.Automated(),
			nullString(track.AutomatedReason),
			lagTrackSource(track.Source),
			LagTrackKindDrift,
			test.DriftTestId,
			run,
		))
		insertLagTicketsInTx(ctx, tx, args.Tickets)

		extendersJson, err := json.Marshal(args.Extenders)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_drift_run (
				drift_test_id,
				run,
				track_id,
				state,
				started_at,
				ip_family,
				client_ip_matches,
				ip_country_code,
				ip_country_name,
				extenders_json,
				k,
				k_early
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`,
			test.DriftTestId,
			run,
			track.TrackId,
			LagDriftRunStateStarted,
			now,
			args.StartIpFamily,
			added.ClientIpMatches,
			nullString(args.IpCountryCode),
			nullString(args.IpCountryName),
			extendersJson,
			args.K,
			args.KEarly,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_test
			SET
				run_count = $2,
				first_started_at = COALESCE(first_started_at, $3),
				retest_until = $4,
				client_ip_hash = CASE WHEN $5 THEN NULL ELSE client_ip_hash END,
				updated_at = $3
			WHERE drift_test_id = $1
			`,
			test.DriftTestId,
			run,
			now,
			retestUntil,
			args.MaxRuns <= run,
		))

		added.Run = run
		added.RunsLeft = max(args.MaxRuns-run, 0)
		added.RetestUntil = retestUntil
	})
	return added
}

// Why a drift test can start no run at `now`, or "" when it can (P5, P6):
// expired when an operator revoked it or it has no run and is past its
// expiry, as an unknown token is, closed when it has failed, is out of runs
// or is past its retest window, and busy while a run started less than
// processingAfter ago is still measuring.
func lagDriftStartRefusal(test *LagDriftTest, runs []*LagDriftRun, now time.Time, maxRuns int, processingAfter time.Duration) string {
	switch {
	case test.RevokeTime != nil:
		return LagDriftRefusalExpired
	case test.FailedAt != nil:
		return LagDriftRefusalClosed
	case test.RunCount == 0 && !now.Before(test.ExpiresAt):
		return LagDriftRefusalExpired
	case maxRuns <= test.RunCount:
		return LagDriftRefusalClosed
	case test.RetestUntil != nil && !now.Before(*test.RetestUntil):
		return LagDriftRefusalClosed
	}
	for _, run := range runs {
		if run.State == LagDriftRunStateStarted && now.Before(run.StartedAt.Add(processingAfter)) {
			return LagDriftRefusalBusy
		}
	}
	return ""
}

// LagDriftStartRefusal is lagDriftStartRefusal for a read without a lock, so
// a start is refused before it does any work; the run is added under the
// test's lock, which checks again.
func LagDriftStartRefusal(test *LagDriftTest, runs []*LagDriftRun, now time.Time, maxRuns int, processingAfter time.Duration) string {
	return lagDriftStartRefusal(test, runs, now, maxRuns, processingAfter)
}

// Records the browser's end of a run (P5): a started run is processing from
// now, with the browser's stop. A run already processing or complete keeps
// its first stop, so finish is idempotent. Answers the run, or nil when the
// test has no such run.
func FinishLagDriftRun(ctx context.Context, driftTestId server.Id, run int, stop string, now time.Time) *LagDriftRun {
	var finished *LagDriftRun
	server.Tx(ctx, func(tx server.PgTx) {
		finished = nil
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_run
			SET
				state = $3,
				finished_at = $4,
				stop = $5
			WHERE drift_test_id = $1 AND run = $2 AND state = $6
			`,
			driftTestId,
			run,
			LagDriftRunStateProcessing,
			now.UTC(),
			stop,
			LagDriftRunStateStarted,
		))
		result, err := tx.Query(
			ctx,
			`SELECT `+lagDriftRunColumns+` FROM lag_drift_run WHERE drift_test_id = $1 AND run = $2`,
			driftTestId,
			run,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				finished = scanLagDriftRun(result)
			}
		})
	})
	return finished
}

// Moves a started run that never finished to processing, as of finishedAt
// (P6).
func SetLagDriftRunProcessingInTx(ctx context.Context, tx server.PgTx, driftTestId server.Id, run int, finishedAt time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE lag_drift_run
		SET state = $3, finished_at = $4
		WHERE drift_test_id = $1 AND run = $2 AND state = $5
		`,
		driftTestId,
		run,
		LagDriftRunStateProcessing,
		finishedAt.UTC(),
		LagDriftRunStateStarted,
	))
}

// What processing found of a run (E5, V10, K4).
type LagDriftRunCompletion struct {
	Outcome       string
	SupportedStop string
	Answered      int
	Usable        int
	// nil when no report shows how long the run measured
	MeasuredMs *int
}

// Completes a processing run with what processing found of it.
func CompleteLagDriftRunInTx(
	ctx context.Context,
	tx server.PgTx,
	driftTestId server.Id,
	run int,
	completion *LagDriftRunCompletion,
	now time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE lag_drift_run
		SET
			state = $3,
			completed_at = $4,
			outcome = $5,
			supported_stop = $6,
			answered = $7,
			usable = $8,
			measured_ms = $9
		WHERE drift_test_id = $1 AND run = $2
		`,
		driftTestId,
		run,
		LagDriftRunStateComplete,
		now.UTC(),
		completion.Outcome,
		nullString(completion.SupportedStop),
		completion.Answered,
		completion.Usable,
		completion.MeasuredMs,
	))
}

// Replaces a drift test's verdict (P4).
func SetLagDriftResultInTx(ctx context.Context, tx server.PgTx, driftTestId server.Id, resultJson []byte, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE lag_drift_test
		SET result_json = $2, result_at = $3, updated_at = $3
		WHERE drift_test_id = $1
		`,
		driftTestId,
		resultJson,
		now.UTC(),
	))
}

// Marks a drift test failed after an internal error (P4): it starts no more
// runs, its client_ip hash goes, since no run can start, and its open runs
// are closed as failed, so neither the sweep nor the stuck runs' count waits
// on them.
func MarkLagDriftTestFailed(ctx context.Context, driftTestId server.Id, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_test
			SET
				failed_at = COALESCE(failed_at, $2),
				client_ip_hash = NULL,
				updated_at = $2
			WHERE drift_test_id = $1
			`,
			driftTestId,
			now.UTC(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_run
			SET
				state = $3,
				completed_at = $2,
				outcome = $4
			WHERE drift_test_id = $1 AND state <> $3
			`,
			driftTestId,
			now.UTC(),
			LagDriftRunStateComplete,
			LagDriftRunOutcomeFailed,
		))
	})
}

// One report of a run, as processing reads it (A5, E5): the minute of
// pings, with the family the session used and nothing of the address.
type LagDriftReport struct {
	Position         int
	ExtenderId       server.Id
	IpFamily         int
	SessionStartTime time.Time
	WindowStartTime  time.Time
	PingOffsetMs     []int32
	// 0 is a lost ping
	PingMicros []int32
	CreatedAt  time.Time
}

// Every report of a track, by position, in the caller's transaction.
func GetLagDriftReportsInTx(ctx context.Context, tx server.PgTx, trackId server.Id) []*LagDriftReport {
	reports := []*LagDriftReport{}
	result, err := tx.Query(
		ctx,
		`
		SELECT
			track_position,
			extender_id,
			extender_ip_family,
			session_start_time,
			window_start_time,
			ping_offset_ms,
			ping_micros,
			created_at
		FROM lag_result
		WHERE track_id = $1
		ORDER BY track_position, created_at
		`,
		trackId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			report := &LagDriftReport{}
			server.Raise(result.Scan(
				&report.Position,
				&report.ExtenderId,
				&report.IpFamily,
				&report.SessionStartTime,
				&report.WindowStartTime,
				&report.PingOffsetMs,
				&report.PingMicros,
				&report.CreatedAt,
			))
			reports = append(reports, report)
		}
	})
	return reports
}

// A drift run's ticket as a prompt report's admission reads it
// (lag.dev/DRIFTTEST.md E4, review F-08): the ticket, and whether its one
// report (A7) is stored already.
type LagDriftReportTicket struct {
	Ticket   *LagTicket
	Reported bool
}

// The ticket when the server issued it for a drift run, nil for a ticket of
// lag.dev's own tracks or none.
func GetLagDriftReportTicket(ctx context.Context, ticketId server.Id) *LagDriftReportTicket {
	var driftTicket *LagDriftReportTicket
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				lag_ticket.track_id,
				lag_ticket.position,
				lag_ticket.client_id,
				lag_ticket.network_id,
				lag_ticket.extender_id,
				lag_ticket.issued_at,
				lag_ticket.expires_at,
				EXISTS (
					SELECT 1 FROM lag_result
					WHERE lag_result.ticket_id = lag_ticket.ticket_id
				)
			FROM lag_ticket
			INNER JOIN lag_track ON lag_track.track_id = lag_ticket.track_id
			WHERE lag_ticket.ticket_id = $1 AND lag_track.kind = 'drift'
			`,
			ticketId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				ticket := &LagTicket{TicketId: ticketId}
				driftTicket = &LagDriftReportTicket{Ticket: ticket}
				server.Raise(result.Scan(
					&ticket.TrackId,
					&ticket.Position,
					&ticket.ClientId,
					&ticket.NetworkId,
					&ticket.ExtenderId,
					&ticket.IssuedAt,
					&ticket.ExpiresAt,
					&driftTicket.Reported,
				))
			}
		})
	})
	return driftTicket
}

// The positions a track's reports cover, and how many reports it has.
func GetLagDriftReportedPositions(ctx context.Context, trackId server.Id) map[int]bool {
	positions := map[int]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT DISTINCT track_position FROM lag_result WHERE track_id = $1`,
			trackId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var position int
				server.Raise(result.Scan(&position))
				positions[position] = true
			}
		})
	})
	return positions
}

// What a drift track keeps of its start address (A5) that processing reads:
// the city point, nil when the place named no city the place list holds, and
// the network, for its type (LagDriftNetworkType).
type LagDriftTrackClient struct {
	Latitude  *float64
	Longitude *float64
	Asn       *int64
	AsnOrg    string
}

// The client fields of a drift track, in the caller's transaction; empty when
// the track is gone.
func GetLagDriftTrackClientInTx(ctx context.Context, tx server.PgTx, trackId server.Id) *LagDriftTrackClient {
	client := &LagDriftTrackClient{}
	result, err := tx.Query(
		ctx,
		`SELECT client_latitude, client_longitude, client_asn, client_asn_org FROM lag_track WHERE track_id = $1`,
		trackId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var asnOrg *string
			server.Raise(result.Scan(&client.Latitude, &client.Longitude, &client.Asn, &asnOrg))
			if asnOrg != nil {
				client.AsnOrg = *asnOrg
			}
		}
	})
	return client
}

// The drift runs still open, started or processing, that started before
// startedBefore: runs a working sweep would have completed by now (E5). A
// failed test's runs are closed with it, and never counted.
func CountStuckLagDriftRuns(ctx context.Context, startedBefore time.Time) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COUNT(*)
			FROM lag_drift_run
			INNER JOIN lag_drift_test ON lag_drift_test.drift_test_id = lag_drift_run.drift_test_id
			WHERE
				lag_drift_run.state <> $1 AND
				lag_drift_run.started_at < $2 AND
				lag_drift_test.failed_at IS NULL
			`,
			LagDriftRunStateComplete,
			startedBefore.UTC(),
		).Scan(&count))
	})
	return count
}

// The drift tests with a run that may be due (P6, E5): one started at or
// before startedBefore and still started, or one processing since at or
// before finishedBefore. A failed test waits on nothing. At most limit,
// oldest first.
func GetDueLagDriftTests(ctx context.Context, startedBefore time.Time, finishedBefore time.Time, limit int) []server.Id {
	driftTestIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT lag_drift_run.drift_test_id
			FROM lag_drift_run
			INNER JOIN lag_drift_test ON lag_drift_test.drift_test_id = lag_drift_run.drift_test_id
			WHERE
				lag_drift_run.state <> $1 AND
				(
					(lag_drift_run.state = $2 AND lag_drift_run.started_at <= $3) OR
					(lag_drift_run.state = $4 AND lag_drift_run.finished_at <= $5)
				) AND
				lag_drift_test.failed_at IS NULL
			GROUP BY lag_drift_run.drift_test_id
			ORDER BY MIN(lag_drift_run.started_at)
			LIMIT $6
			`,
			LagDriftRunStateComplete,
			LagDriftRunStateStarted,
			startedBefore.UTC(),
			LagDriftRunStateProcessing,
			finishedBefore.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var driftTestId server.Id
				server.Raise(result.Scan(&driftTestId))
				driftTestIds = append(driftTestIds, driftTestId)
			}
		})
	})
	return driftTestIds
}

// Deletes the client_ip hash of every drift test that can start no run at
// `now` (K3, R1): one with no run past its expiry, one past its retest
// window, one out of runs, one that failed, and one revoked. Answers how many
// it deleted.
func RemoveClosedLagDriftClientIpHashes(ctx context.Context, now time.Time, maxRuns int) int {
	var count int
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			UPDATE lag_drift_test
			SET client_ip_hash = NULL
			WHERE
				client_ip_hash IS NOT NULL AND
				(
					(first_started_at IS NULL AND expires_at <= $1) OR
					retest_until <= $1 OR
					$2 <= run_count OR
					failed_at IS NOT NULL OR
					revoke_time IS NOT NULL
				)
			`,
			now.UTC(),
			maxRuns,
		)
		server.Raise(err)
		count = int(tag.RowsAffected())
	})
	return count
}

// What one drift retention pass removed.
type RemoveExpiredLagDriftTestsResult struct {
	Tests int `json:"tests"`
	Runs  int `json:"runs"`
}

// Deletes, in bounded batches, every drift test created before minCreatedAt
// with its runs (R1). The runs' tracks, tickets and results follow the lag
// retention (A5).
func RemoveExpiredLagDriftTests(ctx context.Context, minCreatedAt time.Time) RemoveExpiredLagDriftTestsResult {
	removed := RemoveExpiredLagDriftTestsResult{}
	for {
		var tests int
		var runs int
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			driftTestIds := []server.Id{}
			result, err := tx.Query(
				ctx,
				`
				SELECT drift_test_id
				FROM lag_drift_test
				WHERE created_at < $1
				ORDER BY created_at, drift_test_id
				LIMIT $2
				`,
				minCreatedAt.UTC(),
				lagRetentionBatchSize,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var driftTestId server.Id
					server.Raise(result.Scan(&driftTestId))
					driftTestIds = append(driftTestIds, driftTestId)
				}
			})
			tag, err := tx.Exec(ctx, `DELETE FROM lag_drift_run WHERE drift_test_id = ANY($1)`, driftTestIds)
			server.Raise(err)
			runs = int(tag.RowsAffected())
			tag, err = tx.Exec(ctx, `DELETE FROM lag_drift_test WHERE drift_test_id = ANY($1)`, driftTestIds)
			server.Raise(err)
			tests = int(tag.RowsAffected())
		})
		removed.Tests += tests
		removed.Runs += runs
		if tests < lagRetentionBatchSize || ctx.Err() != nil {
			return removed
		}
	}
}

// Marks a drift test revoked by an operator (`bringyourctl lag drift-test
// --revoke`): it keeps its runs and its verdict, starts no more runs, and its
// client_ip hash goes at once, since no run can start (K3). Revoking it again
// keeps the first revoke time. False when there is no such test.
func RevokeLagDriftTest(ctx context.Context, driftTestId server.Id, now time.Time) (revoked bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			UPDATE lag_drift_test
			SET
				revoke_time = COALESCE(revoke_time, $2),
				client_ip_hash = NULL,
				updated_at = $2
			WHERE drift_test_id = $1
			`,
			driftTestId,
			now.UTC(),
		)
		server.Raise(err)
		revoked = tag.RowsAffected() == 1
	})
	return
}

// A network's drift test settings (P3): its allowed parent origins, nil when
// it has set none, which allows any https origin, a local http one and an
// unknown one (Q5, W5); and whether an operator switched its drift tests off.
type LagDriftNetwork struct {
	ParentOrigins []string
	Disabled      bool
}

// A network's drift test settings; a network that never set them has none.
func GetLagDriftNetwork(ctx context.Context, networkId server.Id) *LagDriftNetwork {
	network := &LagDriftNetwork{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT parent_origins, disable_time IS NOT NULL FROM lag_drift_network WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&network.ParentOrigins, &network.Disabled))
			}
		})
	})
	return network
}

// A network's allowed parent origins for its drift tests (P3), nil when it
// has set none.
func GetLagDriftNetworkOrigins(ctx context.Context, networkId server.Id) []string {
	return GetLagDriftNetwork(ctx, networkId).ParentOrigins
}

// Sets a network's allowed parent origins (P3), or clears them with none.
// The origins are stored as given; the caller normalizes them.
func SetLagDriftNetworkOrigins(ctx context.Context, networkId server.Id, origins []string) error {
	if len(origins) == 0 {
		origins = nil
	}
	return upsertLagDriftNetwork(ctx, networkId, `parent_origins = $2`, origins)
}

// Switches a network's drift tests off or on again (P3): while off, the
// network allocates none, and its tokens start no more runs.
func SetLagDriftNetworkDisabled(ctx context.Context, networkId server.Id, disabled bool) error {
	var disableTime *time.Time
	if disabled {
		now := server.NowUtc()
		disableTime = &now
	}
	return upsertLagDriftNetwork(ctx, networkId, `disable_time = CASE WHEN $2::timestamp IS NULL THEN NULL ELSE COALESCE(lag_drift_network.disable_time, $2) END`, disableTime)
}

// Sets one of a network's drift settings, `assignment` to $2, in its row,
// which it creates on first use. ErrNetworkNotFound for no such network.
func upsertLagDriftNetwork(ctx context.Context, networkId server.Id, assignment string, value any) error {
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		if !networkExistsInTx(ctx, tx, networkId) {
			returnErr = ErrNetworkNotFound
			return
		}
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_drift_network (network_id, update_time)
			VALUES ($1, $2)
			ON CONFLICT (network_id) DO NOTHING
			`,
			networkId,
			now,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE lag_drift_network SET `+assignment+`, update_time = $3 WHERE network_id = $1`,
			networkId,
			value,
			now,
		))
	})
	return returnErr
}

// Counts one attempt of `name` by a network against limit per window, as the
// lag limits count (lag_rate_limit.go).
func CheckLagDriftNetworkRateLimit(
	ctx context.Context,
	name string,
	networkId server.Id,
	limit int,
	window time.Duration,
) (LagRateLimitResult, error) {
	key := fmt.Sprintf("{lag_%s_network_%s}", name, networkId)
	return checkLagRateLimit(ctx, key, limit, window)
}

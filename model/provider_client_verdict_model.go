package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The blackhole reasons a client may report. This is a CLOSED set, derived from
// the three conditions that make connect's detectBlackhole fire
// (ip_remote_multi_client.go, `blackhole := func() bool`):
//
//   - sendAckCount <= 0 within BlackholeTimeout of the first un-acked send
//   - receiveAckCount <= 0 within the same window
//   - receiveSynCount <= 0 within BlackholeConnectTimeout of the first syn
//
// It is an allowlist rather than free text because the reason is stored and
// read by humans: an open string column fills with whatever a client build
// happens to send, and then nothing can be counted by it.
//
// Note what the reason is NOT used for: aggregation keys on receive_ack_count
// alone (see ProviderClientVerdictQuorumMet). A client and a server that
// disagree about reason names must not be able to silently break the quorum.
const (
	// the provider acknowledged nothing the client sent
	ProviderClientVerdictReasonNoSendAck = "no-send-ack"
	// the client sent and was acknowledged, but nothing came back -- the
	// egress-dead case, and the only one the quorum counts
	ProviderClientVerdictReasonNoReceiveAck = "no-receive-ack"
	// no syn came back inside the connect timeout
	ProviderClientVerdictReasonNoReceiveSyn = "no-receive-syn"
)

var providerClientVerdictReasons = map[string]bool{
	ProviderClientVerdictReasonNoSendAck:    true,
	ProviderClientVerdictReasonNoReceiveAck: true,
	ProviderClientVerdictReasonNoReceiveSyn: true,
}

// IsProviderClientVerdictReason reports whether reason is one of the known
// blackhole reasons.
func IsProviderClientVerdictReason(reason string) bool {
	return providerClientVerdictReasons[reason]
}

const (
	// ProviderClientVerdictQuorum is how many DISTINCT reporter networks must
	// call a provider egress-dead inside the window before anything happens.
	//
	// Three mirrors the client-side dial-strike shape (3 strikes / 60s / any
	// success clears). It is small on purpose: the consequence of a met quorum
	// is a probe, not a punishment. NetworkCreateDailyLimit is 5, so three
	// sybil networks cost a griefer roughly fifteen minutes -- which is exactly
	// why a met quorum must never do more than schedule a probe.
	ProviderClientVerdictQuorum = 3

	// ProviderClientVerdictWindow is how long a verdict counts for. Outside it
	// a verdict has decayed and contributes nothing, so a provider cannot
	// accumulate a quorum out of unrelated reports spread over a day.
	ProviderClientVerdictWindow = 15 * time.Minute

	// providerClientVerdictScanLimit bounds the window read. The table is
	// append-only and unbounded per reporter, so without a limit one network
	// writing in a loop would make every aggregation read its whole flood.
	//
	// The rows are read OLDEST FIRST, so the limit can only ever suppress a
	// quorum, never manufacture one: eviction drops the newest reports, and the
	// count is of distinct networks, which no volume of writes from one network
	// can increase. Suppression is the safe direction -- it costs a probe that
	// would have been scheduled, not an honest provider's traffic.
	providerClientVerdictScanLimit = 4096
)

// ProviderClientVerdict is one client-reported blackhole verdict.
//
// ReporterNetworkId is always taken from the reporting session. Nothing may
// populate it from a request body -- see SubmitProviderClientVerdict.
type ProviderClientVerdict struct {
	ProviderClientId  server.Id
	ReporterNetworkId server.Id
	Reason            string
	SendAckCount      int64
	SendAckBytes      int64
	ReceiveAckCount   int64
	ReceiveAckBytes   int64
	WindowSeconds     int
	CreateTime        time.Time
}

// AddProviderClientVerdict appends one verdict row.
//
// ON CONFLICT DO NOTHING because the primary key includes create_time: two
// reports from the same reporter about the same provider inside the same clock
// tick would otherwise raise a duplicate-key error inside server.Tx, which
// retries the failed commit blindly for a minute before surfacing a 500. A
// dropped duplicate is exactly right anyway: the second one could not have
// counted for anything the first did not already count for.
func AddProviderClientVerdict(ctx context.Context, verdict *ProviderClientVerdict) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_client_verdict (
				provider_client_id,
				reporter_network_id,
				reason,
				send_ack_count,
				send_ack_bytes,
				receive_ack_count,
				receive_ack_bytes,
				window_seconds,
				create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (provider_client_id, reporter_network_id, create_time) DO NOTHING
			`,
			verdict.ProviderClientId,
			verdict.ReporterNetworkId,
			verdict.Reason,
			verdict.SendAckCount,
			verdict.SendAckBytes,
			verdict.ReceiveAckCount,
			verdict.ReceiveAckBytes,
			verdict.WindowSeconds,
			verdict.CreateTime.UTC(),
		))
	})
}

// GetProviderClientVerdictsInWindow reads the verdicts about one provider that
// are recent enough to still count, oldest first.
//
// This is a bounded READ ONLY. It applies no policy beyond the window: not the
// egress-dead test, and above all not the one-per-reporter cap. Those live in
// ProviderClientVerdictQuorumMet, in Go, in one place, where they are unit
// testable and where breaking them fails a test rather than quietly changing an
// index plan. The database's job here is to hand back a bounded number of rows.
//
// create_time is a naive `timestamp` holding utc, so the cutoff is computed in
// Go and bound as a parameter rather than compared against sql now(), which
// would cast through the session timezone.
func GetProviderClientVerdictsInWindow(
	ctx context.Context,
	providerClientId server.Id,
	now time.Time,
) []ProviderClientVerdict {
	verdicts := []ProviderClientVerdict{}
	minCreateTime := now.UTC().Add(-ProviderClientVerdictWindow)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				reporter_network_id,
				reason,
				send_ack_count,
				send_ack_bytes,
				receive_ack_count,
				receive_ack_bytes,
				window_seconds,
				create_time
			FROM provider_client_verdict
			WHERE
				provider_client_id = $1 AND
				$2 <= create_time

			-- oldest first: see providerClientVerdictScanLimit. A flood of
			-- writes must not be able to evict the reports that arrived before
			-- it, because that is the only direction in which the limit could
			-- change a quorum into a non-quorum for a real provider.
			ORDER BY create_time ASC
			LIMIT $3
			`,
			providerClientId,
			minCreateTime,
			providerClientVerdictScanLimit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				verdict := ProviderClientVerdict{
					ProviderClientId: providerClientId,
				}
				server.Raise(result.Scan(
					&verdict.ReporterNetworkId,
					&verdict.Reason,
					&verdict.SendAckCount,
					&verdict.SendAckBytes,
					&verdict.ReceiveAckCount,
					&verdict.ReceiveAckBytes,
					&verdict.WindowSeconds,
					&verdict.CreateTime,
				))
				verdict.CreateTime = verdict.CreateTime.UTC()
				verdicts = append(verdicts, verdict)
			}
		})
	})

	return verdicts
}

// ProviderClientVerdictQuorumMet is the whole aggregation policy: pure, read
// time, and the only place any of these three rules exists.
//
//  1. egress-dead only. receive_ack_count == 0 means the client sent and got
//     nothing back. A verdict with any receive acks describes a provider that
//     carried traffic, whatever reason string it came with, so it counts for
//     nothing. The test is on the COUNTS, never on the reason string: a client
//     build and a server that disagree about reason names must not be able to
//     silently disable the quorum.
//  2. decay. A verdict older than ProviderClientVerdictWindow is gone. Without
//     this, verdicts accumulate forever and every provider eventually reaches
//     quorum from unrelated incidents months apart.
//  3. ONE VERDICT PER REPORTER NETWORK. This is the anti-griefing property, and
//     it is why `reporters` is a SET and not a counter. One network can write
//     as many rows as it likes -- the table is append-only on purpose -- and
//     still move the count by exactly one. Turning this back into a counter
//     lets a single network manufacture a quorum on its own, which is the
//     precise failure this function exists to prevent.
//
// # What a met quorum is allowed to do
//
// Reprioritise the provider for probing. Nothing else. It must never demote,
// exclude, or touch filter sets, scores, PassesMinimums or find-providers2.
//
// That is a deliberate amendment to the source spec, which had a met quorum
// exclude the provider outright. Client verdicts are the harder signal to game
// -- real destinations, real vantage points, many uncoordinated reporters --
// but the spec paired immediate exclusion with "the prober rehabilitates
// immediately", and that pair is a laundering mechanism: blackhole real users,
// get reported, get probed by the one prober you already special-case, pass,
// get rehabilitated, repeat. And with NetworkCreateDailyLimit = 5 a three
// network quorum is about fifteen minutes of sybil work, so immediate exclusion
// would also let a griefer take an honest provider offline for the price of
// three accounts.
//
// Separating the trigger (client verdicts, fast, hard to fake in aggregate)
// from the punishment (the prober, the sole authority) makes griefing cost one
// probe and nothing more -- which is what the spec wanted to be true. If the
// prober then confirms, the existing probation machinery does the gating; this
// adds no new gate.
func ProviderClientVerdictQuorumMet(verdicts []ProviderClientVerdict, now time.Time) bool {
	minCreateTime := now.UTC().Add(-ProviderClientVerdictWindow)

	reporters := map[server.Id]bool{}
	for _, verdict := range verdicts {
		if verdict.ReceiveAckCount != 0 {
			// the provider carried traffic back
			continue
		}
		if verdict.CreateTime.UTC().Before(minCreateTime) {
			// decayed
			continue
		}
		// a set, not a counter: the cap is one counted verdict per reporter
		// network per provider per window
		reporters[verdict.ReporterNetworkId] = true
	}

	return ProviderClientVerdictQuorum <= len(reporters)
}

// providerClientVerdictProbeDueAge is how far back a quorum-met provider's
// observed_at is moved: far enough to be due for a probe, not so far that
// anything else changes.
//
// The column has two thresholds on it, and the target has to sit strictly
// between them:
//
//   - ProviderEgressLocationMaxAge / 2 (3.5 days) -- the due cutoff
//     (providerEgressDueAge in api/handlers). Older than this and the prober is
//     offered the provider.
//   - ProviderEgressLocationMaxAge (7 days) -- past this the stored location
//     stops being trusted at all (GetFreshProviderEgressLocation*, which falls
//     back to the mmdb lookup) and RemoveExpiredProviderEgressLocations deletes
//     the row.
//
// Backdating past the second one would turn a met quorum into a selection-path
// effect plus data loss, which is exactly what this design forbids. Three
// quarters of the max age is comfortably inside both, and is derived from the
// one constant so it cannot drift away from them.
//
// api/handlers may not be imported here (it imports model), so the due cutoff
// cannot be referenced directly; the handler-side test asserts the provider is
// actually due using the handler's own arithmetic, so a future change to either
// constant breaks a test instead of silently disabling this.
const providerClientVerdictProbeDueAge = ProviderEgressLocationMaxAge * 3 / 4

// ReprioritiseProviderEgressProbe brings a provider's next egress probe
// forward, by moving its stored observed_at back to
// providerClientVerdictProbeDueAge ago. It reports whether a row moved.
//
// This is the ONLY effect a met client-verdict quorum has. It touches exactly
// one meaningful column of one row. No filter set, no score, no PassesMinimums,
// no find-providers2 path reads observed_at -- the selection path reads the
// location through GetFreshProviderEgressLocation*, which still resolves the
// same location, because the new observed_at is still inside
// ProviderEgressLocationMaxAge.
//
// `$2 < observed_at` makes this idempotent and, more importantly, non
// ratcheting: repeated quorums cannot walk a provider's observed_at further and
// further back until the row expires out of the freshness window. A row already
// at or past the target is left exactly where it is.
//
// A provider with no provider_egress_location row at all is not a miss: it has
// never been probed successfully, and the due queue already sorts it ahead of
// every probed provider (GetProviderEgressLocationDue pass 1). There is nothing
// to bring forward.
//
// What this deliberately does NOT touch is provider_egress_probe_attempt. A
// provider tried within ProviderEgressProbeAttemptBackoff (6h) stays deferred,
// so the most a quorum can buy -- honest or manufactured -- is one probe per
// provider per backoff window. That bound is the point: it is what keeps the
// cost of griefing at one probe.
func ReprioritiseProviderEgressProbe(
	ctx context.Context,
	providerClientId server.Id,
	now time.Time,
) bool {
	moved := false
	dueAt := now.UTC().Add(-providerClientVerdictProbeDueAge)

	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			UPDATE provider_egress_location
			SET
				observed_at = $2,
				update_time = $3
			WHERE
				client_id = $1 AND
				$2 < observed_at
			`,
			providerClientId,
			dueAt,
			server.NowUtc(),
		)
		server.Raise(err)
		moved = 0 < tag.RowsAffected()
	})

	return moved
}

// SubmitProviderClientVerdictArgs is one client-reported blackhole verdict as
// the reporting client sends it.
//
// There is deliberately no reporter_network_id field. The reporter is the
// authenticated session and nothing else; declaring the field would both invite
// the lie and, with strict decoding, turn an honest client that echoes it into
// a 400.
//
// SynSent/SynReceived are accepted and validated but not stored: the schema
// carries the ack counters the aggregation actually keys on, and a column
// nothing reads is a column that drifts. They are declared here because the
// client sends them -- with strict decoding, an undeclared field is a 400, so
// silently dropping them from the struct would reject every real submission.
type SubmitProviderClientVerdictArgs struct {
	ExitClientId    server.Id `json:"exit_client_id"`
	Reason          string    `json:"reason"`
	SendAckCount    int64     `json:"send_ack_count"`
	SendAckBytes    int64     `json:"send_ack_bytes"`
	ReceiveAckCount int64     `json:"receive_ack_count"`
	ReceiveAckBytes int64     `json:"receive_ack_bytes"`
	SynSent         int64     `json:"syn_sent"`
	SynReceived     int64     `json:"syn_received"`
	WindowSeconds   int       `json:"window_seconds"`
}

// UnmarshalJSON decodes strictly: an unknown field is an error, which the
// router turns into a 400 before the impl ever runs.
//
// The router's generic decoder is a plain json.Unmarshal shared by every
// endpoint, so strictness has to be attached to the type rather than switched
// on globally -- tightening the shared decoder would change the behaviour of
// every deployed endpoint at once.
//
// Strictness matters here for the same reason it does on the health ingest: the
// body is a set of counts that are read as a judgement. A misspelled
// receive_ack_count decodes to zero, and zero is precisely the value that means
// "egress dead" -- so a typo would not be a malformed report, it would be a
// counting one.
func (self *SubmitProviderClientVerdictArgs) UnmarshalJSON(b []byte) error {
	// a defined type with the same layout, minus the methods, so decoding it
	// does not recurse back into this function
	type strictArgs SubmitProviderClientVerdictArgs

	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()

	var parsed strictArgs
	if err := decoder.Decode(&parsed); err != nil {
		return err
	}

	*self = SubmitProviderClientVerdictArgs(parsed)
	return nil
}

// SubmitProviderClientVerdictResult is deliberately empty.
//
// Telling the reporter whether its verdict met the quorum would hand a griefer
// a progress bar for its sybil campaign -- and would tell any client how many
// other networks are currently reporting a given provider. The reporter needs
// to know its report was accepted, which is the 200.
type SubmitProviderClientVerdictResult struct {
}

// SubmitProviderClientVerdict records one client-reported blackhole verdict and,
// if the report completes a quorum, brings the provider's next egress probe
// forward.
//
// # The reporter is the session
//
// reporter_network_id comes from the authenticated jwt. This is the security
// property the whole aggregation rests on: the quorum counts distinct networks,
// networks cost account creation (NetworkCreateDailyLimit = 5 per day), and a
// body-supplied reporter id would make the count free to fake.
//
// # Validation happens before any store
//
// Every rule below returns 400 without writing. A stored-then-flagged row is
// not an option for a table whose only consumer is a count: a row that is in
// the table is a row that counts.
//
// # A met quorum only reprioritises
//
// See ProviderClientVerdictQuorumMet for the full reasoning. Nothing here
// touches filter sets, scores, PassesMinimums or find-providers2, and this adds
// no new gate: if the prober confirms the provider is dead, the existing
// probation machinery is what acts on it.
func SubmitProviderClientVerdict(
	verdict *SubmitProviderClientVerdictArgs,
	clientSession *session.ClientSession,
) (*SubmitProviderClientVerdictResult, error) {
	// the route is wrapped in RequireAuth, so a session without a jwt cannot
	// reach here. Checked anyway: the reporter identity is the security
	// property, and a nil ByJwt must fail closed rather than panic or, worse,
	// write a zero reporter network that every other zero reporter dedups with.
	if clientSession == nil || clientSession.ByJwt == nil {
		return nil, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	reporterNetworkId := clientSession.ByJwt.NetworkId

	if verdict.ExitClientId == (server.Id{}) {
		return nil, fmt.Errorf("%d Missing exit_client_id.", http.StatusBadRequest)
	}
	if !IsProviderClientVerdictReason(verdict.Reason) {
		return nil, fmt.Errorf("%d Unknown verdict reason.", http.StatusBadRequest)
	}
	// a negative count is not a measurement. It is also the one input that
	// could make the egress-dead test read strangely if it were ever changed
	// from `!= 0` to `<= 0`, so it is rejected at the door.
	if verdict.SendAckCount < 0 ||
		verdict.SendAckBytes < 0 ||
		verdict.ReceiveAckCount < 0 ||
		verdict.ReceiveAckBytes < 0 ||
		verdict.SynSent < 0 ||
		verdict.SynReceived < 0 {
		return nil, fmt.Errorf("%d Counts must be non-negative.", http.StatusBadRequest)
	}
	if verdict.WindowSeconds < 0 {
		return nil, fmt.Errorf("%d window_seconds must be non-negative.", http.StatusBadRequest)
	}

	// the server stamps the time, exactly as the operator ingest endpoints do.
	// A client-supplied timestamp would be one more thing to validate and one
	// more way for a skewed or hostile clock to place a verdict inside a window
	// it does not belong to -- and the window is the decay rule.
	now := server.NowUtc()

	AddProviderClientVerdict(clientSession.Ctx, &ProviderClientVerdict{
		ProviderClientId:  verdict.ExitClientId,
		ReporterNetworkId: reporterNetworkId,
		Reason:            verdict.Reason,
		SendAckCount:      verdict.SendAckCount,
		SendAckBytes:      verdict.SendAckBytes,
		ReceiveAckCount:   verdict.ReceiveAckCount,
		ReceiveAckBytes:   verdict.ReceiveAckBytes,
		WindowSeconds:     verdict.WindowSeconds,
		CreateTime:        now,
	})

	// read-time aggregation, evaluated on the submit that might have completed
	// the quorum. No scheduler and no second cadence: the only moment the
	// answer can change is a write.
	inWindow := GetProviderClientVerdictsInWindow(clientSession.Ctx, verdict.ExitClientId, now)
	if ProviderClientVerdictQuorumMet(inWindow, now) {
		ReprioritiseProviderEgressProbe(clientSession.Ctx, verdict.ExitClientId, now)
	}

	return &SubmitProviderClientVerdictResult{}, nil
}

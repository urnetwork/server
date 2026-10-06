// Mature deficit diagnostics reuse the census, never a hypothesis-shaped head.
package model

import (
	"encoding/json"
	"fmt"

	"github.com/urnetwork/server"
)

const ProviderUrlProbeMatureDeficitSampleLimit = 128

// Only fixed aggregate values leave the query decoder. Each array has a closed
// vocabulary shared with the collector; no identity or stored error is exposed.
type ProviderUrlProbeMatureDeficitDiagnostics struct {
	ContractVersion      int
	PolicyVersion        int
	SampleLimit          int
	Selected             int
	TotalMatureDeficient int
	RunsNeeded           int
	Capped               int
	AcceptedSuccesses    int
	AcceptedFailures     int
	// One missing credit, two, three through nine, or all ten.
	MissingCredits [4]int
	// Due, future through90s, through360s, through900s, or beyond900s.
	Deadline  [5]int
	HintFalse int
	// Never issued, missing current receipt, pending, or completed.
	Claim                 [4]int
	PendingExact900       int
	CompletedExact900     int
	CompletedLocalFailure int
	// Missing, local setup/publication, empty failure, or other failure.
	Attempt         [4]int
	RetainedFewer10 int
	// Age since the newest retained expired receipt expired: through90s,
	// through360s, or older. Providers without such a receipt do not enter.
	ExpiredAge [3]int
	// No expired receipt, missing/future claim, before the six-minute
	// headroom, inside it, or issued at/after that receipt's expiry.
	ClaimVsExpiry [5]int
	// Completed before expiry, at/after expiry, or absent/unknown anchor.
	CompletionVsExpiry [3]int
	// No accepted receipt, latest at most90s old, or older.
	LatestAccepted     [3]int
	PriorityReady      int
	SecurityException  int
	CompletedCountGE10 int
	ClaimClockFuture   int
	AttemptClockFuture int
	// Maxima over present nonnegative lags only; zero does not imply that
	// all rows had an observation. These are not health-ingestion clocks.
	CompletionReceiveLagMaxSeconds float64
	AttemptUpdateLagMaxSeconds     float64
}

// Field names are compact because at most128 rows carry ten receipt timings.
// Identifiers remain SQL join keys and never enter this representation.
type providerUrlProbeMatureDeficitRow struct {
	Runs              int                                    `json:"n"`
	Hint              bool                                   `json:"e"`
	PriorityReady     bool                                   `json:"p"`
	Security          bool                                   `json:"s"`
	UnknownSecurity   bool                                   `json:"u"`
	DeadlineSeconds   float64                                `json:"d"`
	LatestResultAge   *float64                               `json:"r"`
	CompletedRuns     int64                                  `json:"c"`
	NextExpirySeconds *float64                               `json:"x"`
	ClaimState        int                                    `json:"q"`
	ClaimAge          *float64                               `json:"a"`
	ExactClaimLease   bool                                   `json:"l"`
	ReportedAge       *float64                               `json:"t"`
	CompletedAge      *float64                               `json:"f"`
	ReceivedAge       *float64                               `json:"v"`
	CompletionFailure string                                 `json:"k"`
	AttemptAge        *float64                               `json:"b"`
	AttemptUpdatedAge *float64                               `json:"w"`
	AttemptFailure    string                                 `json:"z"`
	History           []providerUrlProbeMatureDeficitHistory `json:"h"`
}

type providerUrlProbeMatureDeficitHistory struct {
	AgeSeconds float64 `json:"a"`
	Current    bool    `json:"c"`
	Success    bool    `json:"s"`
}

// Missing and empty failures are distinguished by the accompanying claim or
// attempt presence. Arbitrary stored text is never returned by this query.
func providerUrlProbeMatureDeficitFailureSql(expression string) string {
	return fmt.Sprintf(`CASE %s WHEN '' THEN 'none'
		WHEN 'tunnel_failed' THEN 'tunnel_failed'
		WHEN 'health_not_run' THEN 'health_not_run'
		WHEN 'run_not_measured' THEN 'run_not_measured'
		WHEN 'no_exit_ip' THEN 'no_exit_ip'
		WHEN 'submit_failed' THEN 'submit_failed'
		ELSE 'other' END`, expression)
}

// All cohort gates and rolling counts were computed by the existing census.
// Only maturity and measured deficit select the head. In particular, hints,
// deadlines, failures, and issued/completed claims never restrict membership.
func providerUrlProbeMatureDeficitCtesSql(policy int) string {
	return fmt.Sprintf(`,
		mature_deficit_head AS MATERIALIZED (
			SELECT client_id,run_count,security_exception,unknown_security_target
			FROM cohort WHERE cycle_started_at <= $4 AND run_count < $3
			ORDER BY client_id LIMIT %d
		), mature_deficit_details AS MATERIALIZED (
			SELECT head.client_id,jsonb_build_object(
				'n',head.run_count,'e',cycle.eligible,'p',cycle.completed_priority_ready,
				's',head.security_exception,'u',head.unknown_security_target,
				'd',EXTRACT(EPOCH FROM (cycle.next_attempt_at-$1::timestamp)),
				'r',EXTRACT(EPOCH FROM ($1::timestamp-cycle.latest_result_at)),
				'c',cycle.completed_run_count,
				'x',EXTRACT(EPOCH FROM (cycle.completed_next_expiry_at-$1::timestamp)),
				'q',CASE WHEN cycle.claim_ordinal=0 THEN 0 WHEN claim.claimed_at IS NULL THEN 1
					WHEN claim.completed_at IS NULL THEN 2 ELSE 3 END,
				'a',EXTRACT(EPOCH FROM ($1::timestamp-claim.claimed_at)),
				'l',COALESCE(cycle.next_attempt_at=claim.claimed_at+interval '15 minutes',false),
				't',EXTRACT(EPOCH FROM ($1::timestamp-claim.reported_completed_at)),
				'f',EXTRACT(EPOCH FROM ($1::timestamp-claim.completed_at)),
				'v',EXTRACT(EPOCH FROM ($1::timestamp-claim.received_at)),
				'k',%s,
				'b',EXTRACT(EPOCH FROM ($1::timestamp-attempt.attempt_at)),
				'w',EXTRACT(EPOCH FROM ($1::timestamp-attempt.update_time)),
				'z',%s,
				'h',history.receipts
			) AS diagnostic
			FROM mature_deficit_head AS head
			CROSS JOIN LATERAL (
				SELECT * FROM provider_egress_probe_cycle AS cycle WHERE cycle.client_id=head.client_id OFFSET 0
			) AS cycle
			LEFT JOIN LATERAL (
				SELECT claimed_at,reported_completed_at,completed_at,received_at,probe_failure
				FROM provider_url_probe_run AS claim
				WHERE claim.client_id=head.client_id AND claim.claim_ordinal=cycle.claim_ordinal OFFSET 0
			) AS claim ON true
			LEFT JOIN LATERAL (
				SELECT attempt_at,update_time,probe_failure FROM provider_egress_probe_attempt AS attempt
				WHERE attempt.client_id=head.client_id OFFSET 0
			) AS attempt ON true
			CROSS JOIN LATERAL (
				SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'a',EXTRACT(EPOCH FROM ($1::timestamp-measured_at)),
					'c',measured_at>$4,'s',ok_count=1
				) ORDER BY measured_at DESC),'[]'::jsonb) AS receipts
				FROM (
					SELECT measured_at,ok_count FROM provider_egress_health_history
					WHERE client_id=head.client_id AND url_probe AND url_probe_policy_version=%d
					AND total_count=1 AND (ok_count=0 OR ok_count=1) AND measured_at<=$1
					ORDER BY measured_at DESC LIMIT 10
				) AS accepted
			) AS history
		)`, ProviderUrlProbeMatureDeficitSampleLimit,
		providerUrlProbeMatureDeficitFailureSql("claim.probe_failure"),
		providerUrlProbeMatureDeficitFailureSql("attempt.probe_failure"), policy)
}

func providerUrlProbeMatureDeficitRows(raw []byte) []providerUrlProbeMatureDeficitRow {
	var rows []providerUrlProbeMatureDeficitRow
	server.Raise(json.Unmarshal(raw, &rows))
	if len(rows) > ProviderUrlProbeMatureDeficitSampleLimit {
		panic("mature URL deficit sample exceeded its bound")
	}
	for _, row := range rows {
		if row.Runs < 0 || ProviderUrlProbeRunTarget <= row.Runs || len(row.History) > ProviderUrlProbeRunTarget {
			panic("invalid mature URL deficit history")
		}
		current := 0
		for _, receipt := range row.History {
			if receipt.Current {
				current++
			}
		}
		if current != row.Runs {
			panic("mature URL deficit history does not match its census")
		}
	}
	return rows
}

func providerUrlProbeMatureDeficitDiagnostics(raw []byte, policy, total int) ProviderUrlProbeMatureDeficitDiagnostics {
	rows := providerUrlProbeMatureDeficitRows(raw)
	diagnostic := ProviderUrlProbeMatureDeficitDiagnostics{
		ContractVersion: 1, PolicyVersion: policy, SampleLimit: ProviderUrlProbeMatureDeficitSampleLimit,
		Selected: len(rows), TotalMatureDeficient: total,
	}
	if len(rows) != min(total, ProviderUrlProbeMatureDeficitSampleLimit) {
		panic("mature URL deficit head does not match its census")
	}
	if len(rows) < total {
		diagnostic.Capped = 1
	}
	for _, row := range rows {
		missing := ProviderUrlProbeRunTarget - row.Runs
		diagnostic.RunsNeeded += missing
		switch missing {
		case 1:
			diagnostic.MissingCredits[0]++
		case 2:
			diagnostic.MissingCredits[1]++
		case 10:
			diagnostic.MissingCredits[3]++
		default:
			diagnostic.MissingCredits[2]++
		}
		switch {
		case row.DeadlineSeconds <= 0:
			diagnostic.Deadline[0]++
		case row.DeadlineSeconds <= 90:
			diagnostic.Deadline[1]++
		case row.DeadlineSeconds <= 360:
			diagnostic.Deadline[2]++
		case row.DeadlineSeconds <= 900:
			diagnostic.Deadline[3]++
		default:
			diagnostic.Deadline[4]++
		}
		if !row.Hint {
			diagnostic.HintFalse++
		}
		if row.PriorityReady {
			diagnostic.PriorityReady++
		}
		if row.Security {
			diagnostic.SecurityException++
		}
		if row.CompletedRuns >= ProviderUrlProbeRunTarget {
			diagnostic.CompletedCountGE10++
		}
		if row.ClaimState < 0 || len(diagnostic.Claim) <= row.ClaimState {
			panic("invalid mature URL deficit claim state")
		}
		diagnostic.Claim[row.ClaimState]++
		if row.DeadlineSeconds > 0 && row.ExactClaimLease {
			if row.ClaimState == 2 {
				diagnostic.PendingExact900++
			} else if row.ClaimState == 3 {
				diagnostic.CompletedExact900++
			}
		}
		localFailure := func(failure string) bool {
			switch failure {
			case "tunnel_failed", "health_not_run", "run_not_measured", "no_exit_ip", "submit_failed":
				return true
			default:
				return false
			}
		}
		if row.ClaimState == 3 && localFailure(row.CompletionFailure) {
			diagnostic.CompletedLocalFailure++
		}
		if row.AttemptAge == nil {
			diagnostic.Attempt[0]++
		} else if localFailure(row.AttemptFailure) {
			diagnostic.Attempt[1]++
		} else {
			switch row.AttemptFailure {
			case "none":
				diagnostic.Attempt[2]++
			default:
				diagnostic.Attempt[3]++
			}
		}
		future := func(ages ...*float64) bool {
			for _, age := range ages {
				if age != nil && *age < 0 {
					return true
				}
			}
			return false
		}
		if future(row.ClaimAge, row.ReportedAge, row.CompletedAge, row.ReceivedAge) {
			diagnostic.ClaimClockFuture++
		}
		if future(row.AttemptAge, row.AttemptUpdatedAge) {
			diagnostic.AttemptClockFuture++
		}
		if row.CompletedAge != nil && row.ReceivedAge != nil {
			diagnostic.CompletionReceiveLagMaxSeconds = max(diagnostic.CompletionReceiveLagMaxSeconds, *row.CompletedAge-*row.ReceivedAge)
		}
		if row.AttemptAge != nil && row.AttemptUpdatedAge != nil {
			diagnostic.AttemptUpdateLagMaxSeconds = max(diagnostic.AttemptUpdateLagMaxSeconds, *row.AttemptAge-*row.AttemptUpdatedAge)
		}
		if len(row.History) < ProviderUrlProbeRunTarget {
			diagnostic.RetainedFewer10++
		}
		latestAge := -1.0
		expiredAge := -1.0
		for _, receipt := range row.History {
			if latestAge < 0 || receipt.AgeSeconds < latestAge {
				latestAge = receipt.AgeSeconds
			}
			if receipt.Current {
				if receipt.Success {
					diagnostic.AcceptedSuccesses++
				} else {
					diagnostic.AcceptedFailures++
				}
			} else if expiredAge < 0 || receipt.AgeSeconds-ProviderEgressProbeRefreshAge.Seconds() < expiredAge {
				expiredAge = receipt.AgeSeconds - ProviderEgressProbeRefreshAge.Seconds()
			}
		}
		switch {
		case latestAge < 0:
			diagnostic.LatestAccepted[0]++
		case latestAge <= 90:
			diagnostic.LatestAccepted[1]++
		default:
			diagnostic.LatestAccepted[2]++
		}
		switch {
		case expiredAge < 0:
			diagnostic.ClaimVsExpiry[0]++
		case row.ClaimAge == nil || *row.ClaimAge < 0:
			diagnostic.ClaimVsExpiry[1]++
		case *row.ClaimAge > expiredAge+ProviderUrlProbeRenewalHeadroom.Seconds():
			diagnostic.ClaimVsExpiry[2]++
		case *row.ClaimAge > expiredAge:
			diagnostic.ClaimVsExpiry[3]++
		default:
			diagnostic.ClaimVsExpiry[4]++
		}
		if expiredAge >= 0 {
			switch {
			case expiredAge <= 90:
				diagnostic.ExpiredAge[0]++
			case expiredAge <= 360:
				diagnostic.ExpiredAge[1]++
			default:
				diagnostic.ExpiredAge[2]++
			}
		}
		switch {
		case expiredAge < 0 || row.CompletedAge == nil || *row.CompletedAge < 0 || row.ReceivedAge == nil || *row.ReceivedAge < 0:
			diagnostic.CompletionVsExpiry[2]++
		case *row.CompletedAge > expiredAge:
			diagnostic.CompletionVsExpiry[0]++
		default:
			diagnostic.CompletionVsExpiry[1]++
		}
	}
	return diagnostic
}

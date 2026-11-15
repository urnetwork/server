package model

// §21's tunables, one struct with one default each (CODESTYLE's settings rule).
// Statutes stay constants where they are used: §2703(f)'s 90 days, §2258A's year,
// DSA Art. 20's six months (server.AbuseAppealWindowMinMonths). Everything else
// a person could tune is here, with its unit and why it has its default: the
// ladders' and clocks' (AbusePolicySettings, embedded), §21.7's standing windows,
// probation's caps and the standing pass, coalescing, the action pass, retention,
// the abuse role's lookup cap and the report joins' bounds. The gates' memory has
// its own (AbuseGateSettings, AbuseHardDenySettings), as network_verified's cache
// does (NetworkVerifiedSettings).
//
// The environment's part comes from abuse.yml (server.GetAbuseConfig):
// ApplyAbuseConfig lays its protected names over a copy, and AbuseReviewHoursOf
// its calendar over DefaultAbuseReviewHours.

import (
	"time"

	"github.com/urnetwork/server"
)

// Tunables of §21's model, controller and worker.
type AbuseSettings struct {
	// The ladders and the review clocks (abuse_policy.go, abuse_clock.go).
	AbusePolicySettings

	// A network younger than this is on probation (§21.7): 7 days. Non-positive
	// puts no network on probation.
	ProbationAge time.Duration
	// How long a host, label or network kill, or a network halt, keeps a network
	// restricted (§21.7, plan-ABUSE-small-readings #4): 30 days. Non-positive
	// restricts on no action.
	RestrictedWindow time.Duration
	// How long a Team must have paid, in window, to be trusted (§21.7,
	// plan-ABUSE-small-readings #18): 30 days.
	TrustedPaidWindow time.Duration
	// How long a trusted network must be free of any action not lifted as wrong
	// (§21.7): 90 days.
	TrustedCleanWindow time.Duration

	// The most TLS endpoints a network on probation or restricted may hold,
	// whatever its tier (§21.7's "1 endpoint"; C-20, QUOTA-M5-01).
	ProbationTlsEndpoints int64
	// The longest a publish by a network on probation or restricted may run
	// (§21.7's "publishes ≤24 h"), below §13.9's own bound.
	ProbationPublishMaxDuration time.Duration

	// How often AbuseStandingPass re-derives the restricted rows past their
	// until_time from the action history (QD-194's daily re-evaluation, §21.10's
	// "only while current"): an hour, so a row every reader already ignores
	// leaves the table within the hour.
	StandingPassInterval time.Duration
	// The most rows one pass reads at a time, by a cursor.
	StandingPassBatchSize int
	// One pass's bound; a pass that runs out leaves the rest to the next.
	StandingPassMaxTime time.Duration
	// The task runner's bound on one pass, past StandingPassMaxTime.
	StandingPassTaskMaxTime time.Duration

	// A report on the network, target and category of a case opened this
	// recently joins it, adding no row (OpenAbuseCaseInTx). A day: long enough
	// that an hourly scan of one stale listing opens one case, short enough that
	// a fresh incident a week on gets its own (abuse-core-policy-readings #7).
	// Non-positive opens a case for every report.
	CaseCoalesceWindow time.Duration

	// How often AbuseActionPass retries actions whose effect or record is owed:
	// each minute, which bounds how long a crash between commit and effect, or
	// before the mail, leaves one owed.
	ActionPassInterval time.Duration
	// An owed action younger than this is left to the request that committed
	// it, which runs its effect and mail straight after, so the pass does not
	// race it. Past it the pass takes over.
	ActionPassGrace time.Duration
	// The most owed actions one pass takes, oldest first, by a cursor.
	ActionPassBatchSize int
	// One pass's bound; a pass that runs out leaves the rest to the next.
	ActionPassMaxTime time.Duration
	// The task runner's bound on one pass, past ActionPassMaxTime, so a pass
	// ends itself before the runner ends it.
	ActionPassTaskMaxTime time.Duration
	// The least time between two default-level log reports of a failed
	// effect, record mail, hint or intake action: a minute, so an outage
	// writes a line a minute with the count it suppressed, not one per action.
	// Non-positive reports every failure.
	ActionReportInterval time.Duration

	// Cases, actions and appeals are kept this many calendar months after the
	// case last closes (§21.10, §2.8 api-14, plan-ABUSE-small-readings #9): 13,
	// §2.8's published ttl, so a longer value breaks that promise.
	CaseRetentionMonths int
	// Legal requests, productions and the access log are kept this many
	// calendar years (§21.10, api-14): 3, published as the case ttl is.
	LegalRetentionYears int
	// The most rows one retention sweep deletes in a transaction.
	RetentionBatchSize int

	// The most rows one abuse-role lookup returns, by case id or join key
	// (§21.10): never in bulk. A lookup past it answers the first rows and says
	// more exist.
	LookupRowLimit int
	// The most bytes of a review's, a lift's or an appeal decision's reasons:
	// 2048, the width of abuse_action.review_reason and
	// abuse_appeal.decision_reason, so a longer one is refused rather than cut.
	ReviewReasonMaxByteCount int

	// How long after an edge connection ends a report may join by its
	// connection_id (§21.8, tls-3): 24 hours. Non-positive joins none.
	ConnectionJoinWindow time.Duration
	// The most bytes of a reported url or host a join reads. 2048: past any
	// host name, and a longer one is dropped unjoined rather than cut, since a
	// cut url could name another host.
	ReportUrlMaxByteCount int
	// The longest host name and label a join looks up: RFC 1035's 253 and 63.
	HostNameMaxByteCount  int
	HostLabelMaxByteCount int
}

// §21's numbers. The protected names stay empty: they are the environment's,
// and without them every ladder refuses every target.
func DefaultAbuseSettings() *AbuseSettings {
	return &AbuseSettings{
		AbusePolicySettings: *DefaultAbusePolicySettings(),
		ProbationAge:        7 * 24 * time.Hour,
		RestrictedWindow:    30 * 24 * time.Hour,
		TrustedPaidWindow:   30 * 24 * time.Hour,
		TrustedCleanWindow:  90 * 24 * time.Hour,

		ProbationTlsEndpoints:       1,
		ProbationPublishMaxDuration: 24 * time.Hour,
		StandingPassInterval:        time.Hour,
		StandingPassBatchSize:       1000,
		StandingPassMaxTime:         5 * time.Minute,
		StandingPassTaskMaxTime:     10 * time.Minute,

		CaseCoalesceWindow:       24 * time.Hour,
		ActionPassInterval:       time.Minute,
		ActionPassGrace:          time.Minute,
		ActionPassBatchSize:      100,
		ActionPassMaxTime:        5 * time.Minute,
		ActionPassTaskMaxTime:    10 * time.Minute,
		ActionReportInterval:     time.Minute,
		CaseRetentionMonths:      13,
		LegalRetentionYears:      3,
		RetentionBatchSize:       1000,
		LookupRowLimit:           100,
		ReviewReasonMaxByteCount: 2048,
		ConnectionJoinWindow:     24 * time.Hour,
		ReportUrlMaxByteCount:    2048,
		HostNameMaxByteCount:     253,
		HostLabelMaxByteCount:    63,
	}
}

// Lays abuse.yml's protected names over settings (abuse-core-protected-names):
// what the ladders read as the roots and platform names. A config with none
// leaves both empty, so every target is refused.
func ApplyAbuseConfig(settings *AbuseSettings, config *server.AbuseConfig) {
	settings.RootNames = config.RootNames()
	settings.PlatformNames = config.PlatformNames()
}

// The review calendar abuse.yml names: DefaultAbuseReviewHours with each key the
// file sets laid over it (abuse-core-policy-readings #16), so a key left out
// keeps its default. NewAbuseCalendar still refuses a bad block, and its
// caller keeps the default calendar then. Holidays are not in the calendar yet.
func AbuseReviewHoursOf(config *server.AbuseConfig) AbuseReviewHours {
	hours := DefaultAbuseReviewHours()
	businessHours := config.BusinessHours()
	if businessHours.Mode != "" {
		hours.Mode = businessHours.Mode
	}
	if businessHours.Zone != "" {
		hours.Zone = businessHours.Zone
	}
	if 0 < len(businessHours.Days) {
		hours.Days = businessHours.Days
	}
	if businessHours.Open != "" {
		hours.Open = businessHours.Open
	}
	if businessHours.Close != "" {
		hours.Close = businessHours.Close
	}
	return hours
}

package model

// §21's tunables, one struct with one default each (CODESTYLE's settings rule).
// Statutes stay constants where they are used: §2703(f)'s 90 days, §2258A's year,
// DSA Art. 20's six months (server.AbuseAppealWindowMinMonths). Everything else
// a person could tune is here, with its unit and why it has its default: the
// ladders' and clocks' (AbusePolicySettings, embedded), §21.7's standing windows,
// coalescing, the action pass, retention and the abuse role's lookup cap.
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

	// A network younger than this is on probation (§21.7): 7 days.
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
}

// §21's numbers. The protected names stay empty: they are the environment's,
// and without them every ladder refuses every target.
func DefaultAbuseSettings() *AbuseSettings {
	return &AbuseSettings{
		AbusePolicySettings:   *DefaultAbusePolicySettings(),
		ProbationAge:          7 * 24 * time.Hour,
		RestrictedWindow:      30 * 24 * time.Hour,
		TrustedPaidWindow:     30 * 24 * time.Hour,
		TrustedCleanWindow:    90 * 24 * time.Hour,
		CaseCoalesceWindow:    24 * time.Hour,
		ActionPassInterval:    time.Minute,
		ActionPassGrace:       time.Minute,
		ActionPassBatchSize:   100,
		ActionPassMaxTime:     5 * time.Minute,
		ActionPassTaskMaxTime: 10 * time.Minute,
		CaseRetentionMonths:   13,
		LegalRetentionYears:   3,
		RetentionBatchSize:    1000,
		LookupRowLimit:        100,
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

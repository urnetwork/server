package monitor

import (
	"github.com/urnetwork/server/pager"
)

// §19.8's page classes (D-OPS-9, owner decision 143). Only an alert of a class
// in this table, at page severity, reaches the rota's pager: every other
// SeverityPage alert stays on Slack as it always has.
//
// A class joins by a row here and a pageAlertClass line in alertClassKinds, in
// the change that starts paging it. TestEveryPageClassHasOneTableRow fails a
// page class with no row, and a row whose class is not a page class.
//
// The M3 rows are here ahead of their signals, so a signal only has to report
// its class to page: EVSB-M3-01 reports the file-cluster and write-budget
// classes, and OPS-M3-02 the burn and primary classes. M5's rows: dns-canary
// reports both dns edges failing and the canary signature (DNS-M5-17),
// lb-split-probe an interface's split probe (OPS-M5-05), and §21.8's notices
// page through pager.PageNotice from taskworker's abuse worker, never through
// a signal, so their row's words are pager's and this table holds them only to
// stay §19.8's whole table. M6's: lb-map-stale reports an interface map-stale
// and a custom host pending past 20 min (OPS-M6-01). A refused drop job, at
// M10, is evs-format2's (EVS2-M10-34).
//
// DESIGN-COMPUTE §14.7's pages are the compute rows (CSRV-M12-06): each reported by its family's signal (SIGNALS.md
// §32) once monitor.yml's compute block enables it, on the two-person out-of-hours rota (owner decision 143), the
// realized margin's in business hours, as the compute runbook has it; a job's alarm reaches its row as a counter the
// signal reads.
//
// A row's hours say when its page may reach the rota (QD-754): any, the
// default, pages at once at any hour, and business is §19.8's business row,
// which the notifier holds until the business calendar opens. The business
// rows' signals report them at page severity, so on Slack they read PAGE at
// any hour, and only the pager waits: filecluster-wear's two (EVSB-M3-01),
// filecluster-cohort's three (EVSB-M3-02, g0-recheck-0-hdd-trigger #1) and
// dns-canary's one NS failing an hour (DNS-M5-17, plan-DNS-running-ns).
//
// A row's text leaves the site in the page's summary, so it names a condition
// and an action and never a target (I-13). The rota's acknowledgment time is
// the same 30 minutes on every row, so it belongs to the pager service. A row
// marked bothAtOnce pages both rota members at once, as §19.8's rota line asks
// of the notices, rather than the second only when the first does not
// acknowledge.

// When a row's page may reach the rota.
type pageHours int

const (
	// at once, at any hour
	pageHoursAny pageHours = iota
	// held until the business calendar opens
	pageHoursBusiness
)

func (self pageHours) String() string {
	if self == pageHoursBusiness {
		return "business"
	}
	return "any"
}

// One alert class and the §19.8 row it pages under.
type pageClass struct {
	class string
	// the condition, as §19.8's row names it
	row string
	// the milestone the row pages from
	firstMilestone string
	// §19.8's first action, what the person paged does
	action string
	// page both rota members at once
	bothAtOnce bool
	// when the page may go; the zero value is any hour
	hours pageHours
}

// §19.8 gives a set at risk and a cluster refusing writes one row, and its
// business row gives a cluster's projected exhaustion and its cohort limit one
// clause.
const (
	pageRowClusterAtRisk    = "a set at_risk, or a cluster refusing writes"
	pageActionClusterAtRisk = "the cold spare within 1 h of the page (19.10); restored within 4 h"
	pageRowCohort           = "a metadata cluster's projected exhaustion within its move tool's lead time, or the cluster at its cohort limit"
	pageActionCohort        = "19.9's cohorts row: date the move tool by the earliest projection, and confirm deletions and erasures pass"
	pageRowLbMap            = "an interface map-stale, or a custom host pending past 20 min (W1)"
	pageActionLbMap         = "that interface's lbmap and reload logs; else drain it (W1), naming its hosts"
)

var pageClasses = []pageClass{
	{class: "filecluster-set-at-risk", row: pageRowClusterAtRisk, firstMilestone: "M3", action: pageActionClusterAtRisk},
	{class: "filecluster-refusing-writes", row: pageRowClusterAtRisk, firstMilestone: "M3", action: pageActionClusterAtRisk},
	{
		class:          "postgres-primary-down",
		row:            "the Postgres primary down (the token exchange stops)",
		firstMilestone: "M3",
		action:         "repair or restore (19.10)",
	},
	{
		class:          "slo-burn-page",
		row:            "2% of a 30-day budget burned in 1 h",
		firstMilestone: "M3",
		action:         "roll back or switch off (D-OPS-10)",
	},
	{
		class:          "write-budget-p0-past-refill",
		row:            "P0 past the refill: a metadata cluster's P0 bytes over the last hour above that hour's refill",
		firstMilestone: "M3",
		action:         "19.10's P0 row: capacity, placement, the networks; P0 is never paced or refused",
	},
	{
		class:          "filecluster-key-held",
		row:            "a file-cluster key held over 1 min by a write that cannot settle",
		firstMilestone: "M3",
		action:         "19.10's held-key row: the host back, or its drives replaced",
	},
	{
		class:          "dns-ns-down",
		row:            "both dns edges failing probes",
		firstMilestone: "M5",
		action:         "§12.6's runbook",
	},
	{
		class:          "dns-canary-signature",
		row:            "the canary signature failing",
		firstMilestone: "M5",
		action:         "§12.6's runbook",
	},
	{
		class:          lbSplitProbeClassFailing,
		row:            "an interface's split probe failing (W1)",
		firstMilestone: "M5",
		action:         "that interface's prior lb version",
	},
	{
		class:          lbMapClassStale,
		row:            pageRowLbMap,
		firstMilestone: "M6",
		action:         pageActionLbMap,
	},
	{
		class:          lbMapClassHostPending,
		row:            pageRowLbMap,
		firstMilestone: "M6",
		action:         pageActionLbMap,
	},
	{
		class:          "evs-drop-refused",
		row:            "a drop job refused {verify} or {mismatch} (storage integrity)",
		firstMilestone: "M10",
		action:         "19.10's drop rows: on {verify}, format2's drop to observe (19.11)",
	},
	{
		// pager.NoticePageClass, named here as every class is named, by its literal
		class:          "abuse-notice",
		row:            pager.NoticePageRow,
		firstMilestone: "M5",
		action:         pager.NoticePageAction,
		bothAtOnce:     true,
	},
	// §19.8's business row
	{
		class:          "filecluster-drive-dwpd-business",
		row:            "a metadata drive's 7-day mean over 0.75 DWPD",
		firstMilestone: "M3",
		action:         "19.10's file cluster row: confirm the counters, then order the next metadata cluster (19.9)",
		hours:          pageHoursBusiness,
	},
	{
		class:          "filecluster-drive-rating-short",
		row:            "a metadata drive projected, at its 30-day rate, to spend its rated endurance before five years of service",
		firstMilestone: "M3",
		action:         "19.6's wear row: record the wear against the rating, then order the next metadata cluster (19.9)",
		hours:          pageHoursBusiness,
	},
	{class: "filecluster-cohort-exhaustion", row: pageRowCohort, firstMilestone: "M3", action: pageActionCohort, hours: pageHoursBusiness},
	{class: "filecluster-cohort-limit", row: pageRowCohort, firstMilestone: "M3", action: pageActionCohort, hours: pageHoursBusiness},
	{
		class:          "filecluster-hdd-trigger",
		row:            "a segment cluster's projected drive operations crossing half its measured capacity within the segment placement's lead time",
		firstMilestone: "M3",
		action:         "19.9's hdd row: order the segment placement and its HDD capacity before the crossing",
		hours:          pageHoursBusiness,
	},
	{
		class:          "dns-ns-failing",
		row:            "one dns edge failing probes for an hour",
		firstMilestone: "M5",
		action:         "§12.6's runbook",
		hours:          pageHoursBusiness,
	},
	// DESIGN-COMPUTE §14.7's pages (SIGNALS.md §32), each first action the compute runbook's (xops' COMPUTE-OPERATIONS.md)
	{
		class:          "compute-provider-failing",
		row:            "a compute provider failing every call for 5 minutes",
		firstMilestone: "M12",
		action:         "the provider's status and compute_canary's last run; calls fail with a named error, never another model; a suspended account is the provider adapters' runbook",
	},
	{
		class:          "compute-attestation-failures",
		row:            "attestation failures above 5% for 10 minutes",
		firstMilestone: "M12",
		action:         "the failing check in the metric's reason; a provider's new image not yet in the policy is a sitting the same day; never widen a profile to pass it",
	},
	{
		class:          "compute-settlement-lag",
		row:            "compute settlement lag over 60 s",
		firstMilestone: "M12",
		action:         "compute_settle's last run, Postgres's health, a network fence held; restart the job's chain, never edit a hold in Redis",
	},
	{
		class:          "compute-provider-balance-low",
		row:            "a compute provider balance under a day",
		firstMilestone: "M12",
		action:         "the treasury's status and compute_gamma_mint's and compute_gamma_spend's last runs; the week's transfer early if the float is short",
	},
	{
		class:          "compute-gamma-unknown-spend",
		row:            "an unknown GammaSpent from our hotkeys",
		firstMilestone: "M12",
		action:         "the deposit's cap is 0: pause the provider in the treasury, rotate K_P and reconcile the provider's VM list",
	},
	{
		class:          "compute-policy-expiring",
		row:            "the compute policy within 14 days of its not_after",
		firstMilestone: "M12",
		action:         "call a sitting of the compute-policy key's holders",
	},
	{
		class:          "compute-ticket-horizon",
		row:            "the newest signed ticket within 7 days of its horizon",
		firstMilestone: "M12",
		action:         "call the tickets' sitting; past the horizon remote calls, enclave creates and searches stop",
	},
	{
		class:          "compute-reconcile-gap",
		row:            "a compute reconciliation gap over 1%",
		firstMilestone: "M12",
		action:         "compute_reconcile's report: the provider's bill against our charges and spends; raise a billing fault before the next week's transfer",
	},
	{
		class:          "compute-rollup-mismatch",
		row:            "a day's spend rollup differing from its charges, or a network's totals from its day rows and pruned sums",
		firstMilestone: "M12",
		action:         "compute_rollup_day's alarm names the day; take no partition by hand, and file the difference with the rows that show it",
	},
	{
		class:          "compute-search-lapses",
		row:            "lapsed search authorizations above 5% for 10 minutes",
		firstMilestone: "M13",
		action:         "the search signer's Base sources and the facilitator's settlements; holds lapse on their own through compute_search_tickets",
	},
	{
		class:          "compute-search-float-low",
		row:            "the search payer float under one day of searches",
		firstMilestone: "M13",
		action:         "compute_search_float's last refill and Coinbase's withdrawal to the payer address",
	},
	{
		class:          "compute-search-failure-rate",
		row:            "the search provider's failure rate above 5% in an hour of at least 100 searches",
		firstMilestone: "M13",
		action:         "the provider's status; nothing is retried elsewhere; the month's rates go to the owner",
	},
	{
		class:          "compute-search-payer-transfer",
		row:            "a USDC transfer from the payer that settles no authorization the signer issued",
		firstMilestone: "M13",
		action:         "signing and refills have stopped: treat the payer key as leaked, the search payer's runbook",
	},
	{
		class:          "compute-search-nonce-unknown",
		row:            "a used nonce the search signer did not issue",
		firstMilestone: "M13",
		action:         "signing and refills have stopped: treat the payer key as leaked, the search payer's runbook",
	},
	{
		class:          "compute-search-sanctions-record",
		row:            "a payee the screen refused, or an authorization settled outside the facilitator",
		firstMilestone: "M13",
		action:         "keep the sanctions record, tell counsel, and suspend the network's searches if its sidecar sent it",
	},
	{
		class:          "compute-search-screen-stale",
		row:            "the search screen's lists unfetched for 24 hours",
		firstMilestone: "M13",
		action:         "compute_sanctions_list's fetch and each list's source; at 72 hours signing stops on its own",
	},
	{
		class:          "compute-search-base-disagree",
		row:            "Base's two sources disagreeing",
		firstMilestone: "M13",
		action:         "find the source that is behind or forked and let finality settle it; never pick one by hand",
	},
	{
		class:          "compute-dispute-rate",
		row:            "compute disputes from at least 3 networks past 0.5% of a provider's requests in a day",
		firstMilestone: "M12",
		action:         "compare the provider's usage with the devices'; a provider over-reporting leaves the catalog by the policy's next line",
	},
	{
		class:          "compute-canary-gap",
		row:            "a compute canary gap over 2%",
		firstMilestone: "M12",
		action:         "compute_canary's run against the provider's usage and its bill; as the disputes row",
	},
	{
		class:          "compute-margin-low",
		row:            "a compute provider's realized margin under 5% for a week",
		firstMilestone: "M12",
		action:         "compute_reconcile's weekly margins; a price change is the owner's, through the catalog's next prices",
		hours:          pageHoursBusiness,
	},
	{
		class:          "compute-direct-worst-case-rise",
		row:            "a direct compute provider's costliest worst case rising",
		firstMilestone: "M12",
		action:         "compute_provider_prices' exposure rule: hold the provider out of direct at the next sitting if the rise outruns the holds",
	},
	{
		class:          "compute-op-stale",
		row:            "a compute operation row open past 24 hours, or settled and unbooked past 60 s",
		firstMilestone: "M12",
		action:         "compute_op_recover's and compute_settle's last runs; a row open 24 hours is released at no charge: let the job do it",
	},
	{
		class:          "compute-search-row-retained",
		row:            "a web search row past 72 hours",
		firstMilestone: "M13",
		action:         "compute_search_purge's last run: no per-search row may outlive 72 hours",
	},
	{
		class:          "compute-search-unattributed",
		row:            "a day's unattributed search value above $10 or 1% of its settled value, or a search day that does not balance",
		firstMilestone: "M13",
		action:         "the signer's day totals against the broker's charges for the day; unattributed settlements are URnetwork's cost",
	},
	{
		class:          "compute-search-unsettled-high",
		row:            "the search signer's value not yet final past 80% of max_unsettled",
		firstMilestone: "M13",
		action:         "the signer stops at max_unsettled on its own; read Base's finalized head and the facilitator's backlog",
	},
	{
		class:          "compute-direct-absorbed-high",
		row:            "absorbed direct-call charges above $50 in a day across networks",
		firstMilestone: "M12",
		action:         "compute_direct_settle: a provider whose lookups fail keeps its holds pending; raise its usage lookup with it",
	},
}

// The class's row; false for a class that does not page the rota.
func pageClassOf(class string) (pageClass, bool) {
	for _, row := range pageClasses {
		if row.class == class {
			return row, true
		}
	}
	return pageClass{}, false
}

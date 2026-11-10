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
// stay §19.8's whole table. §19.8's later rows join with their tasks: an
// interface map-stale or a custom host pending at M6, a refused drop job at
// M10.
//
// A row's text leaves the site in the page's summary, so it names a condition
// and an action and never a target (I-13). The rota's acknowledgment time is
// the same 30 minutes on every row, so it belongs to the pager service. A row
// marked bothAtOnce pages both rota members at once, as §19.8's rota line asks
// of the notices, rather than the second only when the first does not
// acknowledge.

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
}

// §19.8 gives a set at risk and a cluster refusing writes one row.
const (
	pageRowClusterAtRisk    = "a set at_risk, or a cluster refusing writes"
	pageActionClusterAtRisk = "the cold spare within 1 h of the page (19.10); restored within 4 h"
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
		// pager.NoticePageClass, named here as every class is named, by its literal
		class:          "abuse-notice",
		row:            pager.NoticePageRow,
		firstMilestone: "M5",
		action:         pager.NoticePageAction,
		bothAtOnce:     true,
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

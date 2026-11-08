package monitor

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
// classes, and OPS-M3-02 the burn and primary classes. §19.8's later rows join
// with their tasks: both dns edges failing, the canary signature, a split probe
// and the notices at M5, an interface map-stale or a custom host pending at
// M6, a refused drop job at M10.
//
// A row's text leaves the site in the page's summary, so it names a condition
// and an action and never a target (I-13). The rota's acknowledgment time is
// the same 30 minutes on every row, so it belongs to the pager service.

// One alert class and the §19.8 row it pages under.
type pageClass struct {
	class string
	// the condition, as §19.8's row names it
	row string
	// the milestone the row pages from
	firstMilestone string
	// §19.8's first action, what the person paged does
	action string
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

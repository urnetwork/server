package pager

// §21.8's notices on §19.8's rota (R-OPS-11, D-ABUSE-3, owner decisions 134,
// 143): any registrar or registry notice about <root> or bringyour.com, and a
// law-enforcement emergency notice, EU removal orders included, pages both rota
// members at once, at any hour from M5, to be handled within 1 h.
//
// Taskworker's abuse worker files the notice as a case and then calls
// PageNotice with the case's id (ABUSE-M5-10). A page leaves the site, so it
// carries the notice's kind, the time and the case id and nothing else (I-13):
// no sender, body, URL, network or host is an argument here, so none can reach
// a page by mistake, and the person paged reads the rest through the staff
// route by case id. Which notices page at all is the abuse worker's to decide
// (plan-ABUSE-notice-intake): this file pages what it is given.
//
// Who acts is §21.8's: removal orders and registrar or registry notices by the
// rota, production orders and other data requests routed to counsel's
// out-of-hours contact. Both people are paged for every kind, since the hour
// starts at the page, and the summary names where the kind goes.
//
// Delivery is the sink's. A lost page is the worst failure here, so PageNotice
// returns the sink's error unchanged: a caller that can retry hands it a
// RetrySink and waits on its Flush.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// One kind of §21.8 notice that pages.
type NoticeKind string

const (
	NoticeKindRegistrar NoticeKind = "registrar"
	NoticeKindRegistry  NoticeKind = "registry"
	// a law-enforcement emergency notice that is neither of the two below
	NoticeKindLawEnforcementEmergency NoticeKind = "law_enforcement_emergency"
	NoticeKindEuRemovalOrder          NoticeKind = "eu_removal_order"
	NoticeKindProductionOrder         NoticeKind = "production_order"
)

// §19.8's notice row. Monitor's page table carries the same row
// (monitor/page_classes.go), so the two can be compared, and a page's text is
// only ever these words.
const (
	// the class every notice pages under
	NoticePageClass = "abuse-notice"
	// the condition, as §19.8's row names it
	NoticePageRow = "registrar, registry, law-enforcement notices"
	// §19.8's first action
	NoticePageAction = "§21.8: removal orders and registrar or registry notices handled within 1 h by the rota; " +
		"production orders go to counsel's out-of-hours contact"
)

// Who handles each kind within the hour (§21.8), for the summary.
var noticeKindHandlers = map[NoticeKind]string{
	NoticeKindRegistrar:               "the rota handles it",
	NoticeKindRegistry:                "the rota handles it",
	NoticeKindEuRemovalOrder:          "the rota handles it",
	NoticeKindLawEnforcementEmergency: "the rota routes it to counsel's out-of-hours contact unless it is a removal order",
	NoticeKindProductionOrder:         "the rota routes it to counsel's out-of-hours contact",
}

// One notice to page.
type Notice struct {
	Kind NoticeKind
	// the abuse case the notice was filed under: the page's key holds only this
	CaseId server.Id
	// when the notice was filed
	Time time.Time
	// the deployment, shown first in the summary as monitor's pages show it
	Environment string
}

// The refusal says which field is wrong and never repeats a value.
func (self Notice) Validate() error {
	if _, ok := noticeKindHandlers[self.Kind]; !ok {
		return errors.New("pager: a notice of a kind that does not page")
	}
	if self.CaseId == (server.Id{}) {
		return errors.New("pager: a notice has no case id")
	}
	if self.Time.IsZero() {
		return errors.New("pager: a notice has no time")
	}
	return nil
}

// The key a notice's page opens under: its case's, so a retry or a second
// notice filed under the same case opens no second page while the first is
// open.
func NoticeDedupKey(caseId server.Id) string {
	return "notice-" + caseId.String()
}

// The page for notice, both people at once.
func noticePage(notice Notice) (Page, error) {
	if err := notice.Validate(); err != nil {
		return Page{}, err
	}
	environment := strings.TrimSpace(notice.Environment)
	if environment == "" {
		environment = "unknown"
	}
	return Page{
		DedupKey: NoticeDedupKey(notice.CaseId),
		Class:    NoticePageClass,
		Summary: fmt.Sprintf(
			"[%s] %s (%s): %s. Then: %s. This one: %s within 1 h. Case %s.",
			environment, NoticePageClass, notice.Kind, NoticePageRow, NoticePageAction, noticeKindHandlers[notice.Kind], notice.CaseId,
		),
		Time:       notice.Time.UTC(),
		BothAtOnce: true,
	}, nil
}

// Pages both rota members at once for notice (see the file comment). The error
// is the refusal of a notice that cannot page, or the sink's own.
func PageNotice(ctx context.Context, sink Sink, notice Notice) error {
	if sink == nil {
		return errors.New("pager: no sink to page a notice through")
	}
	page, err := noticePage(notice)
	if err != nil {
		return err
	}
	return sink.Trigger(ctx, page)
}

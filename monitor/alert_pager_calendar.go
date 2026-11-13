package monitor

// The business calendar §19.8's business-hours pages wait on (QD-754, owner
// decisions 95 and 134): one rota reviews in business hours, so a page of
// §19.8's business row reaches it only while those hours are open.
//
// The hours are abuse.yml's business_hours block, the review calendar §21's
// clocks already read (model.AbuseCalendar), reread as the file is. A
// pager-service schedule takes its place through PagerBusinessCalendar once
// plan-OPS-pager-service names a service.

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// When the rota takes §19.8's business-hours pages. IsOpen is asked under the
// notifier's lock, so it must not wait on the network, and it is safe for
// concurrent use.
type PagerBusinessCalendar interface {
	// whether a business-hours page may reach the rota at at
	IsOpen(at time.Time) bool
}

// abuse.yml's review hours as the pager's calendar (see the file comment).
//
// The block's mode is not read: always is M6's 24/7 review of machine
// actions, and it must not turn §19.8's "business hours only" into a page at
// 03:00, so the pager keeps the block's zone, days and hours under either
// mode. A block that does not build a calendar keeps the default hours,
// Monday to Friday 09:00 to 18:00 UTC, and says so once per block: a page
// held on a calendar nobody can read would never go. Holidays are not in
// model's calendar yet, so a page can still go on one.
type abuseReviewPagerCalendar struct {
	// the current abuse.yml
	config func() *server.AbuseConfig
	logf   func(format string, args ...any)
	// the default hours' calendar, which a block that does not read keeps
	defaultCalendar *model.AbuseCalendar

	stateLock sync.Mutex
	// the block calendar was built from; nil before the first read
	hours    *model.AbuseReviewHours
	calendar *model.AbuseCalendar
}

// The production calendar, on server.GetAbuseConfig's reads. logf takes one
// line per call, without a trailing newline.
func NewAbuseReviewPagerCalendar(logf func(format string, args ...any)) PagerBusinessCalendar {
	return newAbuseReviewPagerCalendar(server.GetAbuseConfig, logf)
}

func newAbuseReviewPagerCalendar(config func() *server.AbuseConfig, logf func(format string, args ...any)) *abuseReviewPagerCalendar {
	defaultCalendar, err := model.NewAbuseCalendar(model.DefaultAbuseReviewHours(), model.DefaultAbusePolicySettings())
	if err != nil {
		// the default block is fixed by the build
		panic(fmt.Errorf("monitor: the default review hours do not build a calendar: %w", err))
	}
	return &abuseReviewPagerCalendar{config: config, logf: logf, defaultCalendar: defaultCalendar}
}

func (self *abuseReviewPagerCalendar) IsOpen(at time.Time) bool {
	return self.current().IsOpen(at)
}

// The calendar of the file's current block, rebuilt only when the block
// changes.
func (self *abuseReviewPagerCalendar) current() *model.AbuseCalendar {
	hours := model.DefaultAbuseReviewHours()
	if config := self.config(); config != nil {
		hours = model.AbuseReviewHoursOf(config)
	}
	hours.Mode = model.AbuseReviewModeBusiness

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.hours != nil && abuseReviewHoursEqual(*self.hours, hours) {
		return self.calendar
	}
	calendar, err := model.NewAbuseCalendar(hours, model.DefaultAbusePolicySettings())
	if err != nil {
		self.logf("monitor: abuse.yml's business hours do not read (%v): business-hours pages keep the default hours", err)
		calendar = self.defaultCalendar
	}
	self.hours = &hours
	self.calendar = calendar
	return calendar
}

func abuseReviewHoursEqual(a model.AbuseReviewHours, b model.AbuseReviewHours) bool {
	return a.Mode == b.Mode && a.Zone == b.Zone && a.Open == b.Open && a.Close == b.Close && slices.Equal(a.Days, b.Days)
}

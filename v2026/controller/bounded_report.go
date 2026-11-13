// A default-level report that logs at most one line per interval and counts the
// lines it held back, for failures a caller or an outage can repeat at will.
package controller

import (
	"sync"
	"time"
)

// At most one report per interval. Safe for concurrent use.
type boundedReport struct {
	interval time.Duration

	stateLock       sync.Mutex
	nextReportTime  time.Time
	suppressedCount int
}

// A report that logs at most once per `interval`.
func newBoundedReport(interval time.Duration) *boundedReport {
	return &boundedReport{
		interval: interval,
	}
}

// Whether to log a report at now, and how many reports were held back since the
// last one logged.
func (self *boundedReport) Allow(now time.Time) (suppressedCount int, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if now.Before(self.nextReportTime) {
		self.suppressedCount += 1
		return 0, false
	}
	self.nextReportTime = now.Add(self.interval)
	suppressedCount = self.suppressedCount
	self.suppressedCount = 0
	return suppressedCount, true
}

package pager

// The rota's pager (§19.8, D-OPS-9, owner decisions 134, 143): two people on
// one pager service, the second paged when the first has not acknowledged in
// 30 minutes, and a notice paging both at once.
//
// A caller raises a Page through a Sink and later resolves it. Acknowledgment
// and escalation belong to the service, so nothing here keeps a timer: which
// service, and whether it or monitor owns the 30 minutes, waits on
// plan-OPS-pager-service. FakeSink models that policy on a fake clock, so
// FS-50 runs either way.
//
// Monitor raises §19.8's infrastructure pages and taskworker's abuse worker
// raises §21.8's notices, which is why this is its own package.
//
// A page leaves the site for the pager service, so its fields hold a class, a
// time and opaque ids, never a network name, address or token (I-13).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// An opaque id. The shape keeps a raw identity, a path, a mail address or a
// sentence out of a key that leaves the site.
var dedupKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// One page for the rota.
type Page struct {
	// stable across retries and restarts: the sink opens one page per key
	DedupKey string
	// the page class
	Class string
	// what the person paged reads
	Summary string
	// when the condition was observed
	Time time.Time
	// page both rota members at once, as a notice does, instead of the second
	// only when the first does not acknowledge
	BothAtOnce bool
}

// The sink refuses a page without these: an empty key would fold unrelated
// pages into one.
func (self Page) Validate() error {
	if err := ValidateDedupKey(self.DedupKey); err != nil {
		return err
	}
	if strings.TrimSpace(self.Class) == "" {
		return errors.New("pager: a page has no class")
	}
	if strings.TrimSpace(self.Summary) == "" {
		return errors.New("pager: a page has no summary")
	}
	if self.Time.IsZero() {
		return errors.New("pager: a page has no time")
	}
	return nil
}

// The key itself is left out of the error, since a key that failed may hold
// what must not be logged.
func ValidateDedupKey(dedupKey string) error {
	if !dedupKeyPattern.MatchString(dedupKey) {
		return fmt.Errorf("pager: a dedup key of %d bytes is not an opaque id", len(dedupKey))
	}
	return nil
}

// The pager service as a caller sees it. A nil return means the service took
// the event. Both calls are safe to repeat, which is what lets a retry and a
// restarted caller send the same event twice. A Sink honors ctx and is safe
// for concurrent use.
type Sink interface {
	// Opens the page. A key already open changes nothing, its acknowledgment
	// clock included.
	Trigger(ctx context.Context, page Page) error
	// Closes the key's page. A key that is not open changes nothing.
	Resolve(ctx context.Context, dedupKey string) error
}

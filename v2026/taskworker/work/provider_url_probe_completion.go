// Completed URL-turn identity survives buffering, readiness, and metric adapters.
package work

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026/qualityprobe"
)

type providerUrlProbeCompletion = qualityprobe.UrlProbeCompletion

// One completed turn is retained by value until the guarded batch is released.
func (self *providerEgressFullBatch) ReportUrlProbeCompletion(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		entry := self.entryWithLock(completion.ClientId)
		entry.completion = &completion
		failure := completion.ProbeFailure
		entry.attempt = &failure
	}()
	return nil
}

// Publication can refine a local failure class, never the finished-turn clock
// or claim identity. Legacy/manual probes keep their existing wire behavior.
func (self *providerEgressFullBatch) publishAttempt(ctx context.Context, providerClientId, failure string, entry *providerEgressFullBatchEntry) error {
	if entry.completion == nil {
		return self.sink.ReportAttempt(ctx, providerClientId, failure)
	}
	completion := *entry.completion
	completion.ProbeFailure = failure
	return self.sink.ReportUrlProbeCompletion(ctx, completion)
}

// A completed run remains a completed run during platform failure. Withhold
// provider-verdict permission; the server may release this finished claim into
// bounded local retry without granting measured evidence or quota credit.
func (self *providerEgressProbeReadinessReporter) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	var readinessErr error
	if completion.ProbeFailure != "" {
		readinessErr = self.readiness.check(ctx)
		if readinessErr != nil {
			completion.AllowPacing = false
		}
	}
	reporter, ok := self.egressProbeIngest.(qualityprobe.UrlProbeCompletionReporter)
	if !ok {
		return errors.Join(readinessErr, qualityprobe.ErrUrlProbeCompletionUnsupported)
	}
	return errors.Join(readinessErr, reporter.ReportUrlProbeCompletion(ctx, completion))
}

// Counts one completed producer turn, not each HTTP acknowledgement retry.
func (self *egressProbeMetricsReporter) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	egressProbeAttemptsTotal.WithLabelValues(
		egressProbeResultLabel(completion.ProbeFailure),
		self.country(ctx, completion.ClientId),
	).Inc()
	reporter, ok := self.inner.(qualityprobe.UrlProbeCompletionReporter)
	var err error
	if ok {
		err = reporter.ReportUrlProbeCompletion(ctx, completion)
	} else {
		err = qualityprobe.ErrUrlProbeCompletionUnsupported
	}
	egressProbeSubmissionOutcomesTotal.WithLabelValues("attempt",
		egressProbeSubmissionOutcome(err, qualityprobe.ErrUrlProbeCompletionUnsupported)).Inc()
	return err
}

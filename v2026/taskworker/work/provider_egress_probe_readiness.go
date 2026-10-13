// Shared prober admission failures invalidate negative provider measurements.
package work

import (
	"context"
	"errors"
	"sync"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

var errProviderEgressProbeUnfunded = errors.New("provider egress prober cannot fund its initial transfer contract")
var errProviderEgressProbeFundingUnknown = errors.New("provider egress prober transfer credit could not be observed")

// One pass latches a shared observation failure so later recovery cannot make
// earlier negative measurements valid. Concurrent lanes share the same latch;
// database and Redis reads never hold its lock.
type providerEgressProbeReadiness struct {
	available func(context.Context) (model.ByteCount, error)
	minimum   model.ByteCount

	stateLock sync.Mutex
	failure   error
}

// Uses the same available-credit model and minimum as contract admission.
// The model raises datastore failures; turn those into unknown readiness
// without publishing private query values or letting a probe worker panic.
func newProviderEgressProbeReadiness(networkId server.Id) *providerEgressProbeReadiness {
	return &providerEgressProbeReadiness{
		minimum: controller.MinContractTransferByteCount,
		available: func(ctx context.Context) (available model.ByteCount, returnErr error) {
			defer func() {
				if recover() != nil {
					returnErr = errProviderEgressProbeFundingUnknown
				}
			}()
			if model.HasActiveTransferBalance(ctx, networkId, controller.MinContractTransferByteCount) {
				available = controller.MinContractTransferByteCount
			}
			return
		},
	}
}

// Nil is reserved for focused pass tests that do not own a production identity.
// A failed read is unknown, never positive credit; cancellation stays cancellation.
func (self *providerEgressProbeReadiness) check(ctx context.Context) error {
	if self == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := self.err(); err != nil {
		return err
	}
	available, readErr := self.available(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	step := ""
	if readErr != nil {
		err = errProviderEgressProbeFundingUnknown
		step = "funding_unknown"
	} else if available < self.minimum {
		err = errProviderEgressProbeUnfunded
		step = "funding_unavailable"
	}
	if err == nil {
		return self.err()
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !errors.Is(self.failure, err) {
			self.failure = errors.Join(self.failure, err)
			return true
		}
		return false
	}()
	if recorded {
		egressProbePassErrorsTotal.WithLabelValues(step).Inc()
	}
	return self.err()
}

// Exposes only the bounded failure already observed by this pass.
func (self *providerEgressProbeReadiness) err() error {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.failure
}

// Only a completely classified funding failure may use the ordinary shard
// idle cadence. Joined transport, unknown-readiness and cancellation failures
// retain task backoff rather than borrowing the funding retry hint.
func providerEgressProbeUnfundedOnly(err error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	for _, node := range causes.Nodes {
		if node.Leaf && node.Err != errProviderEgressProbeUnfunded {
			return false
		}
	}
	return true
}

// Successful observations remain valid evidence. Failed attempts and degraded
// health require observable funding before they can update provider state;
// an actual TLS authentication failure remains an independent integrity fact.
type providerEgressProbeReadinessReporter struct {
	egressProbeIngest
	readiness *providerEgressProbeReadiness
}

// The failure report also controls the provider's durable retry backoff.
func (self *providerEgressProbeReadinessReporter) ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error {
	if probeFailure != "" {
		if err := self.readiness.check(ctx); err != nil {
			return err
		}
	}
	return self.egressProbeIngest.ReportAttempt(ctx, providerClientId, probeFailure)
}

// Health is published inside the full runner, before it returns its summary.
func (self *providerEgressProbeReadinessReporter) SubmitEgressHealth(ctx context.Context, providerClientId string, result *egresshealth.Result) error {
	if result != nil && !result.TlsAuthenticationFailure && (result.Total <= 0 || result.OkCount < result.Total) {
		if err := self.readiness.check(ctx); err != nil {
			if providerUrlProbeHasSecurityEvidence(result) {
				// Preserve authenticated hops without assigning an uncertain
				// quality trial. The original diagnostic remains immutable and
				// the local readiness/publication errors remain observable.
				securityOnly := *result
				securityOnly.OkCount, securityOnly.Total = 0, 0
				securityOnly.NotMeasured = max(1, result.NotMeasured+result.Total)
				securityOnly.ByClass = nil
				securityOnly.Checks = append([]egresshealth.CheckResult(nil), result.Checks...)
				for i := range securityOnly.Checks {
					securityOnly.Checks[i].Ok, securityOnly.Checks[i].NotMeasured = false, true
				}
				return errors.Join(err, self.egressProbeIngest.SubmitEgressHealth(ctx, providerClientId, &securityOnly))
			}
			return err
		}
	}
	return self.egressProbeIngest.SubmitEgressHealth(ctx, providerClientId, result)
}

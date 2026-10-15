// One provider's bounded measurement, in a capability tunnel of its own.
package capprobe

import (
	"context"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// How often a measurement reads its recorder. The recorder offers no
// notification, and a read is one leaf-lock copy.
const pollInterval = time.Second

// What happened to one provider's measurement, one label of
// urnetwork_capability_probe_providers_total. The recorded outcomes are the
// gate's own (model.ProviderModernOutcomes); the rest recorded nothing.
const (
	outcomeRecordedModern             = "recorded_modern"
	outcomeRecordedNotPostQuantum     = "recorded_not_post_quantum"
	outcomeRecordedNoResizableWindows = "recorded_no_resizable_windows"
	// the tunnel could not be opened
	outcomeTunnelFailed = "tunnel_failed"
	// no acknowledgement of the probe's pings arrived
	outcomeNoAck = "no_ack"
	// acknowledged, but the send path accepted no ClientHello
	outcomeE2eNotAttempted = "e2e_not_attempted"
	// acknowledged, and the ClientHello was still in its window at the end
	outcomeE2ePending = "e2e_pending"
	// the pass ended the measurement
	outcomeCanceled = "canceled"
	// the tunnel's path went away, or its window client was replaced, before
	// the reading concluded
	outcomeTunnelLost = "tunnel_lost"
	// the evidence was conclusive and the model did not store it
	outcomeRecordFailed = "record_failed"
	// a negative reading held back from fresh modern evidence until a later
	// sweep confirms it
	outcomeDowngradeHeld = "downgrade_held"
	// a negative reading not recorded because its pass negotiated no
	// end-to-end session with any provider
	outcomeNegativeWithheld = "negative_withheld"
)

// Every outcome, in the order the metrics list them.
var outcomes = []string{
	outcomeRecordedModern,
	outcomeRecordedNotPostQuantum,
	outcomeRecordedNoResizableWindows,
	outcomeTunnelFailed,
	outcomeNoAck,
	outcomeE2eNotAttempted,
	outcomeE2ePending,
	outcomeCanceled,
	outcomeTunnelLost,
	outcomeRecordFailed,
	outcomeDowngradeHeld,
	outcomeNegativeWithheld,
}

// One open capability measurement tunnel to one provider. Methods are safe
// for concurrent use.
type capabilityTunnel interface {
	// what the tunnel's clients have recorded of the provider so far, false
	// for nothing
	observation() (connect.PeerCapabilityObservation, bool)
	// closed once the tunnel's path to the provider is gone for good, or its
	// window client was replaced
	lost() <-chan struct{}
	// tears the tunnel down and joins its owners; it is called once
	close() error
}

// Opens one provider's capability measurement tunnel. The tunnel lives under
// ctx; its teardown is close, never ctx's end.
type openCapabilityTunnel func(ctx context.Context, providerId server.Id) (capabilityTunnel, error)

// One provider's finished measurement.
type measurement struct {
	providerId server.Id
	// false when the tunnel could not be opened: there is no reading
	opened  bool
	reading reading
	// the evidence to record, for a conclusive reading
	evidence   model.ProviderModernEvidence
	recordable bool
	canceled   bool
	// the tunnel was lost by the final read
	lost bool
	// from opening the tunnel to reading the evidence, teardown excluded
	elapsed time.Duration
	openErr error
	// the teardown's error, which costs nothing of the reading
	closeErr error
}

// The outcome label of a measurement that recorded nothing, or whose
// conclusive evidence is about to be recorded.
func (self measurement) outcome() string {
	switch {
	case !self.opened:
		return outcomeTunnelFailed
	case self.recordable:
		switch classified := modernOutcomeOf(self.evidence); classified {
		case model.ProviderModernOutcomeModern:
			return outcomeRecordedModern
		case model.ProviderModernOutcomeNoResizableWindows:
			return outcomeRecordedNoResizableWindows
		default:
			return outcomeRecordedNotPostQuantum
		}
	case self.canceled:
		return outcomeCanceled
	case self.lost:
		return outcomeTunnelLost
	case self.reading.window == windowNotTested:
		return outcomeNoAck
	case self.reading.e2e == e2eNotTested:
		return outcomeE2eNotAttempted
	default:
		return outcomeE2ePending
	}
}

// The outcome the gate gives freshly recorded evidence, as the model stores
// it: post-quantum needs a negotiated session, and the windows come next.
func modernOutcomeOf(evidence model.ProviderModernEvidence) string {
	switch {
	case !evidence.E2eNegotiated || !evidence.PostQuantum:
		return model.ProviderModernOutcomeNotPostQuantum
	case !evidence.WindowAdvertised:
		return model.ProviderModernOutcomeNoResizableWindows
	default:
		return model.ProviderModernOutcomeModern
	}
}

// Measures one provider in a tunnel of its own and closes it.
//
// The tunnel's window pings the provider as any provider window does, which
// also opens the end-to-end session: its first send starts the ClientHello.
// The measurement reads the recorder every pollInterval and stops as soon as
// the reading is complete: an acknowledgement read the window, and the
// session negotiated (and sealed, or had sealGrace to), or its ClientHello
// went unanswered through a whole establishment window. It stops at
// measureTimeout whatever the reading, at the tunnel's loss, and at ctx's
// end, and then reads once more. So a provider that cannot open the probe's
// sealed frames, which stalls the probe's pings until the provider's own
// window expires, holds its slot for at most measureTimeout and the
// teardown. The tunnel is opened under the pass's ctx, not one bounded by
// measureTimeout: its teardown must still retire the derived client after
// the measurement ends.
//
// A tunnel lost by the final read, its path gone or its window client
// replaced, keeps a negotiated session and the window, which the provider
// showed whichever client saw them, but concludes no unanswered ClientHello:
// a window that ran out at or after the loss may belong to a client that was
// leaving or gone, or be read against a replacement's ClientHello sent after
// it began. A window that had run out by an earlier read ended the
// measurement there.
func (self *passRunner) measure(ctx context.Context, providerId server.Id) (result measurement) {
	result.providerId = providerId
	startedAt := self.now()
	deadline := self.after(self.settings.measureTimeout())
	tunnel, err := self.open(ctx, providerId)
	if err != nil {
		result.openErr = err
		result.elapsed = self.now().Sub(startedAt)
		return result
	}
	result.opened = true
	defer func() {
		result.closeErr = tunnel.close()
	}()

	var ackSeenAt time.Time
	var negotiatedSeenAt time.Time
	// one read at now, carrying what earlier reads saw
	read := func() (reading, connect.PeerCapabilityObservation, time.Time) {
		observation, found := tunnel.observation()
		now := self.now()
		if found && (observation.AckObserved || observation.WindowAdvertised) && ackSeenAt.IsZero() {
			ackSeenAt = now
		}
		current := readObservation(observation, found, now, ackSeenAt, self.tlsTimeout)
		if current.negotiated() && negotiatedSeenAt.IsZero() {
			negotiatedSeenAt = now
		}
		return current, observation, now
	}
	current, observation, now := read()
	for !current.complete(now, negotiatedSeenAt, self.settings.sealGrace()) {
		stop := false
		select {
		case <-ctx.Done():
			result.canceled = true
			stop = true
		case <-deadline:
			stop = true
		case <-tunnel.lost():
			stop = true
		case <-self.after(pollInterval):
		}
		current, observation, now = read()
		if stop {
			break
		}
	}
	select {
	case <-tunnel.lost():
		result.lost = true
	default:
	}
	if result.lost && current.e2e == e2eNoResponder {
		current.e2e = e2ePending
	}
	result.reading = current
	result.elapsed = now.Sub(startedAt)
	if evidence, ok := current.evidence(observation, now); ok {
		result.evidence = evidence
		result.recordable = true
		// a conclusive reading stands even when the pass is ending
		result.canceled = false
	}
	return result
}

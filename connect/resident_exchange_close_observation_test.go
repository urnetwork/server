// Exercises the fixture's exact event boundaries without service setup or timing.
package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Real pipe Close records the first event; the shared observer is then drained
// after teardown to prove that late processing cannot erase live-close evidence.
func TestExchangeQueueRefusalCloseObservationPreservesLiveEvidence(t *testing.T) {
	unexpected := errors.New("synthetic non-done native failure")
	cases := []struct {
		name               string
		phase              uint32
		cancelTransport    bool
		noTransport        bool
		keepRootLive       bool
		firstCloser        int64
		refused            int64
		socketTimeout      bool
		admission          pooledMessageSendResult
		raised             error
		wantAdmissionError bool
		wantWorkerError    bool
		wantCloseText      string
	}{
		{name: "setup live", phase: exchangeQueueRefusalPhaseSetup, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled, wantAdmissionError: true, wantWorkerError: true, wantCloseText: "invalid setup:"},
		{name: "live oracle", phase: exchangeQueueRefusalPhaseLiveOracle, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled, wantAdmissionError: true, wantWorkerError: true, wantCloseText: "invalid setup:"},
		{name: "child canceled without teardown", phase: exchangeQueueRefusalPhaseLiveOracle, cancelTransport: true, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled, wantAdmissionError: true, wantWorkerError: true, wantCloseText: "invalid setup:"},
		{name: "teardown marker with live child", phase: exchangeQueueRefusalPhaseTeardown, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled, wantAdmissionError: true, wantWorkerError: true, wantCloseText: "invalid setup:"},
		{name: "teardown marker without child", phase: exchangeQueueRefusalPhaseTeardown, noTransport: true, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled, wantAdmissionError: true, wantWorkerError: true, wantCloseText: "invalid setup:"},
		{name: "explicit child teardown before root", phase: exchangeQueueRefusalPhaseTeardown, cancelTransport: true, keepRootLive: true, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled},
		{name: "explicit child teardown after root", phase: exchangeQueueRefusalPhaseTeardown, cancelTransport: true, firstCloser: 1, admission: pooledMessageSendDone, raised: context.Canceled},
		{name: "live old refusal proof", phase: exchangeQueueRefusalPhaseLiveOracle, firstCloser: 1, refused: 1, admission: pooledMessageSendDelivered, wantCloseText: "OLD discriminator:"},
		{name: "setup old refusal proof", phase: exchangeQueueRefusalPhaseSetup, firstCloser: 1, refused: 1, admission: pooledMessageSendDelivered, wantCloseText: "OLD discriminator:"},
		{name: "live peer closed first", phase: exchangeQueueRefusalPhaseLiveOracle, firstCloser: 2, refused: 1, admission: pooledMessageSendDelivered, wantCloseText: "invalid setup:"},
		{name: "live wrong refusal count", phase: exchangeQueueRefusalPhaseLiveOracle, firstCloser: 1, refused: 2, admission: pooledMessageSendDelivered, wantCloseText: "invalid setup:"},
		{name: "live socket timeout", phase: exchangeQueueRefusalPhaseLiveOracle, firstCloser: 1, refused: 1, socketTimeout: true, admission: pooledMessageSendDelivered, wantCloseText: "invalid setup:"},
		{name: "teardown does not hide refusal or non-done raise", phase: exchangeQueueRefusalPhaseTeardown, cancelTransport: true, keepRootLive: true, firstCloser: 1, admission: pooledMessageSendDropped, raised: unexpected, wantAdmissionError: true, wantWorkerError: true},
	}
	for _, c := range cases {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &p2OriginFixture{ctx: ctx, state: p2OriginState{changed: make(chan struct{})}}
			q := &exchangeQueueRefusalFixture{origin: f, localClosed: make(chan struct{})}
			if !c.noTransport {
				settings := DefaultExchangeSettings()
				q.transport = NewResidentTransportWithProperties(
					ctx, &Exchange{ctx: ctx, settings: settings}, server.NewId(), server.NewId(),
					clientconnect.TransferCarrierProperties{ReceiveReliability: clientconnect.CarrierReliabilityReliable},
				)
				defer q.transport.Close()
				if c.cancelTransport {
					q.transport.Cancel()
				}
			}
			q.lifecyclePhase.Store(c.phase)
			q.refused.Store(c.refused)
			q.socketTimeout.Store(c.socketTimeout)

			q.observeTransportAdmission(c.admission)
			if (f.state.err != nil) != c.wantAdmissionError {
				t.Errorf("%s: admission error = %v, want error = %t", c.name, f.state.err, c.wantAdmissionError)
			}
			f.state.err = nil
			f.workers.Add(1)
			q.runNativeWorker(func() {
				if c.raised != nil {
					panic(c.raised)
				}
			})
			f.workers.Wait()
			if (f.state.err != nil) != c.wantWorkerError {
				t.Errorf("%s: worker error = %v, want error = %t", c.name, f.state.err, c.wantWorkerError)
			}
			if c.wantWorkerError && !errors.Is(f.state.err, c.raised) {
				t.Errorf("%s: worker lost its exact cause: %v", c.name, f.state.err)
			}
			f.state.err = nil

			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			localOwner := &exchangeQueueRefusalConn{Conn: local, fixture: q, endpoint: 1}
			remoteOwner := &exchangeQueueRefusalConn{Conn: remote, fixture: q, endpoint: 2}
			if c.firstCloser == 2 {
				_ = remoteOwner.Close()
				_ = localOwner.Close()
			} else {
				_ = localOwner.Close()
				_ = remoteOwner.Close()
			}
			proof := q.firstCloseProof.Load()
			wantProof := c.firstCloser | int64(c.phase)<<2 | c.refused<<5
			if c.cancelTransport {
				wantProof |= 16
			}
			if proof != wantProof {
				t.Errorf("%s: first-close snapshot = %d, want %d", c.name, proof, wantProof)
			}
			// Both select cases are ready for late observations. Regardless of
			// which wins, the recorded close phase must decide the result.
			q.lifecyclePhase.Store(exchangeQueueRefusalPhaseTeardown)
			if !c.keepRootLive {
				cancel()
			}
			q.observeConnectionClose()
			if c.wantCloseText == "" {
				if f.state.err != nil {
					t.Errorf("%s: expected cleanup was classified as refusal failure: %v", c.name, f.state.err)
				}
			} else if f.state.err == nil || !strings.Contains(f.state.err.Error(), c.wantCloseText) {
				t.Errorf("%s: captured live-close evidence lost: got %v, want %q", c.name, f.state.err, c.wantCloseText)
			}
		}()
	}
}

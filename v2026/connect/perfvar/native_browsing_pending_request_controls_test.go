// These controls distinguish an actual HTTP Transport offer still waiting for
// TCP/TLS/headers from a manufactured queued-progress flag. No TLS-ready/server
// receipts are seeded for the pending case; original99 image proofs stay separate.
package perfvar

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
)

type nativeBrowsingPendingWitness interface {
	beforeNavigation(context.Context, int, func()) error
	close()
}

func nativeBrowsingPendingWitnessForTest(warm bool) (nativeBrowsingPendingWitness, *nativeBrowsingPressureObservation) {
	pressure := &nativeBrowsingPressureObservation{}
	pressure.assets[1][1].expectedBytes = int64(nativeBrowsingAssetBytes(1, 1))
	if warm {
		return newNativeBrowsingHttp2Witness(pressure), pressure
	}
	return newNativeBrowsingColdLinkWitness(nil, pressure, false), pressure
}

func nativeBrowsingPendingCountsForTest(witness nativeBrowsingPendingWitness) (int, int) {
	switch typed := witness.(type) {
	case *nativeBrowsingColdLinkWitness:
		return typed.requests[1][1].clientAcquisitions, typed.requests[1][1].serverAttempts
	case *nativeBrowsingHttp2Witness:
		return typed.requests[1][1].clientAcquisitions, typed.requests[1][1].serverAttempts
	default:
		panic("unowned pending witness")
	}
}

func nativeBrowsingPendingControllerForTest(t *testing.T, warm bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		oldCtx, cancelOld := context.WithCancel(ctx)
		witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
		progress := &pressure.assets[1][1]
		entered, oldDone := make(chan struct{}), make(chan struct{})
		oldResult := make(chan error, 1)
		defer func() {
			cancelOld()
			witness.close()
			<-oldDone
			if err := <-oldResult; !errors.Is(err, context.Canceled) {
				t.Errorf("pending HTTP request did not join its original cancellation: %v", err)
			}
		}()
		go func() {
			defer close(oldDone)
			progress.started.Store(true)
			defer progress.finished.Store(true)
			pressure.beginFetch(nativeBrowsingAssetBytes(1, 1))
			defer pressure.activeRequests.Add(-1)
			request, err := http.NewRequestWithContext(oldCtx, http.MethodGet, "https://origin2.test/asset/1/1", nil)
			if err != nil {
				oldResult <- err
				close(entered)
				return
			}
			trace := nativeBrowsingContextTraceForTest(witness, request.Context(), request.URL.String())
			request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
			client := &http.Client{Transport: nativeBrowsingControllerRoundTrip(func(request *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(request.Context())
				if trace != nil && trace.GetConn != nil {
					trace.GetConn(request.URL.Hostname() + ":443")
				}
				close(entered)
				<-request.Context().Done()
				return nil, request.Context().Err()
			})}
			_, err = client.Do(request)
			oldResult <- err
		}()
		<-entered
		synctest.Wait()
		acquired, server := nativeBrowsingPendingCountsForTest(witness)
		if !progress.started.Load() || progress.finished.Load() || progress.bodyOpened.Load() || progress.receivedBytes.Load() != 0 || acquired != 0 || server != 0 || pressure.activeRequests.Load() != 1 {
			t.Fatal("fixture lacks one actual HTTP Transport offer still waiting before TCP/TLS/response")
		}
		if err := witness.beforeNavigation(ctx, 2, nil); err != nil {
			marker := "COLD_LINK_HTML_REFUSED_PENDING_ORIGINAL_TLS"
			if warm {
				marker = "HTTP2_HTML_REFUSED_PENDING_ORIGINAL_TLS"
			}
			t.Errorf("%s: original HTTP Transport entered; bytes=0 acquired=0 server_attempts=0 body_opened=false: %v", marker, err)
			return
		}
		dispatched := false
		if err := nativeBrowsingControllerDispatchForTest(ctx, nil, func(*http.Request) { dispatched = true }); err != nil {
			t.Fatal(err)
		}
		if !dispatched || progress.finished.Load() || progress.bodyOpened.Load() || progress.receivedBytes.Load() != 0 || oldCtx.Err() != nil || ctx.Err() != nil {
			t.Fatal("new HTML depended on completing/releasing/canceling the original pending request")
		}
	})
}

func TestNativeBrowsingColdLinkPendingTlsDoesNotBlockHtml(t *testing.T) {
	nativeBrowsingPendingControllerForTest(t, false)
}
func TestNativeBrowsingHttp2PendingTlsDoesNotBlockHtml(t *testing.T) {
	nativeBrowsingPendingControllerForTest(t, true)
}

// Repeated GetConn belongs to the same original trace/request, including an
// HTTP/2 pool retry. It is not a second GotConn acquisition/server attempt.
func TestNativeBrowsingPendingGetConnIsIdempotent(t *testing.T) {
	for _, warm := range []bool{false, true} {
		witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
		pressure.assets[1][1].started.Store(true)
		ctx := context.Background()
		trace := nativeBrowsingContextTraceForTest(witness, ctx, "https://origin2.test/asset/1/1")
		if trace == nil || trace.GetConn == nil {
			t.Fatal("pending owner has no real GetConn registration")
		}
		trace.GetConn("origin2.test:443")
		trace.GetConn("origin2.test:443")
		if err := witness.beforeNavigation(ctx, 2, nil); err != nil {
			t.Fatalf("idempotent GetConn rejected: %v", err)
		}
		if acquired, server := nativeBrowsingPendingCountsForTest(witness); acquired != 0 || server != 0 {
			t.Fatal("GetConn fabricated TLS/server-ready evidence")
		}
		witness.close()
	}
}

func TestNativeBrowsingPendingCanceledOriginalContextRejected(t *testing.T) {
	for _, warm := range []bool{false, true} {
		witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
		pressure.assets[1][1].started.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		trace := nativeBrowsingContextTraceForTest(witness, ctx, "https://origin2.test/asset/1/1")
		if trace == nil || trace.GetConn == nil {
			cancel()
			t.Fatal("pending context owner was not registered")
		}
		trace.GetConn("origin2.test:443")
		cancel()
		if err := witness.beforeNavigation(context.Background(), 2, nil); err == nil {
			t.Error("canceled original request was called active under its live parent")
		}
		witness.close()
	}
}

func TestNativeBrowsingPendingDuplicateTraceAndForeignTargetRejected(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, kind := range []string{"duplicate-same-context", "duplicate-new-context", "foreign-dispatch", "foreign-target", "no-dispatch", "body-without-TLS"} {
			witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
			progress := &pressure.assets[1][1]
			progress.started.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			target := "https://origin2.test/asset/1/1"
			if kind == "foreign-target" {
				target += "?unowned=1"
			}
			trace := nativeBrowsingContextTraceForTest(witness, ctx, target)
			if trace != nil && trace.GetConn != nil && kind != "no-dispatch" {
				host := "origin2.test:443"
				if kind == "foreign-dispatch" {
					host = "unowned.example:443"
				}
				trace.GetConn(host)
			}
			switch kind {
			case "duplicate-same-context":
				_ = nativeBrowsingContextTraceForTest(witness, ctx, "https://origin2.test/asset/1/1")
			case "duplicate-new-context":
				_ = nativeBrowsingContextTraceForTest(witness, context.Background(), "https://origin2.test/asset/1/1")
			case "body-without-TLS":
				progress.bodyOpened.Store(true)
			}
			if err := witness.beforeNavigation(context.Background(), 2, nil); err == nil {
				t.Errorf("accepted invalid %s original pending request", kind)
			}
			cancel()
			witness.close()
		}
	}
}

func TestNativeBrowsingPendingDispatchDoesNotHideObservedSessionError(t *testing.T) {
	for _, warm := range []bool{false, true} {
		witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
		pressure.assets[1][1].started.Store(true)
		trace := nativeBrowsingContextTraceForTest(witness, context.Background(), "https://origin2.test/asset/1/1")
		if trace == nil || trace.GetConn == nil {
			t.Fatal("pending owner has no dispatch callback")
		}
		trace.GetConn("origin2.test:443")
		// A non-TLS actual acquisition must still publish the existing error.
		trace.GotConn(httptrace.GotConnInfo{Conn: nil})
		if err := witness.beforeNavigation(context.Background(), 2, nil); err == nil || !strings.Contains(err.Error(), "TLS") && !strings.Contains(err.Error(), "session") {
			t.Errorf("pending dispatch hid an invalid observed acquisition: %v", err)
		}
		witness.close()
	}
}

// Scalar receipt transitions are a model. Real TLS/session provenance remains
// covered by the selected live integration controls and final500-request census.
func TestNativeBrowsingPendingRequestObservationPhases(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, phase := range []struct {
			acquired int
			server   int
			valid    bool
		}{
			{acquired: 0, server: 0, valid: true},
			{acquired: 1, server: 0, valid: true},
			{acquired: 1, server: 1, valid: true},
			{acquired: 0, server: 1, valid: false},
		} {
			witness, pressure := nativeBrowsingPendingWitnessForTest(warm)
			pressure.assets[1][1].started.Store(true)
			ctx := context.Background()
			trace := nativeBrowsingContextTraceForTest(witness, ctx, "https://origin2.test/asset/1/1")
			if trace == nil || trace.GetConn == nil {
				t.Fatal("transition model lacks original Transport dispatch")
			}
			trace.GetConn("origin2.test:443")
			switch typed := witness.(type) {
			case *nativeBrowsingColdLinkWitness:
				request := &typed.requests[1][1]
				request.clientAcquisitions, request.serverAttempts = phase.acquired, phase.server
				if phase.acquired == 1 {
					request.clientSession, request.verifiedClientTls = &tls.Conn{}, true
				}
				if phase.server == 1 {
					request.serverOwned, request.serverTuple, request.serverProvider = true, "192.0.2.1:40001|192.0.2.2:443", 0
				}
			case *nativeBrowsingHttp2Witness:
				typed.requests[1][1].clientAcquisitions, typed.requests[1][1].serverAttempts = phase.acquired, phase.server
				if phase.acquired == 1 {
					typed.clientSessions[2] = &tls.Conn{}
				}
				if phase.server == 1 {
					typed.serverSessions[2] = "192.0.2.1:40001|192.0.2.2:443"
				}
			}
			err := witness.beforeNavigation(ctx, 2, nil)
			if (err == nil) != phase.valid {
				t.Errorf("warm=%t acquisition=%d server=%d valid=%t: %v", warm, phase.acquired, phase.server, phase.valid, err)
			}
			witness.close()
		}
	}
}

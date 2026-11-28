// These component controls use the existing real verified TLS/tuple fixture.
package perfvar

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
)

// Publish actual trace/server ownership and an opened body at the precise old
// unlocked observation boundary; no sleep or probability decides the ordering.
func TestColdLinkHoldSnapshotKeepsOnePublicationBoundary(t *testing.T) {
	control := newNativeBrowsingRosterControl(t)
	witness := control.witness
	hold := &witness.js[3]
	target := "https://origin0.test/asset/3/1"
	witness.requests[3][1] = nativeBrowsingColdRequest{bodyGate: hold.gate}
	progress := &witness.pressure.assets[3][1]
	progress.expectedBytes = int64(nativeBrowsingAssetBytes(3, 1))
	progress.started.Store(true)
	trace := witness.requestTraceWithContext(context.Background(), target)
	trace.GetConn("origin0.test:443")
	published := false
	witness.afterHoldSnapshotForTest = func() {
		witness.afterHoldSnapshotForTest = nil
		trace.GotConn(httptrace.GotConnInfo{Conn: control.client, Reused: true})
		writer := httptest.NewRecorder()
		witness.wrapHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(nativeBrowsingAsset(3, 1))
		})).ServeHTTP(writer, control.request(target))
		progress.bodyOpened.Store(true)
		reader := witness.wrapReader(context.Background(), target, bytes.NewReader(writer.Body.Bytes()))
		buffer := make([]byte, 137)
		count, err := reader.Read(buffer)
		if err != nil || count != len(buffer) {
			t.Fatalf("actual owned reader: count=%d err=%v", count, err)
		}
		progress.receivedBytes.Store(int64(count))
		published = true
	}
	if err := witness.checkHold(hold, nativeBrowsingColdJs); err != nil {
		t.Fatalf("HOLD_SNAPSHOT_PUBLICATION_RACE: %v receipt=%+v", err, witness.row.HoldPremiseFailure)
	}
	if !published || witness.row.HoldPremiseFailure != nil {
		t.Fatal("control did not cross exactly one actual ownership publication")
	}
	if err := witness.checkHold(hold, nativeBrowsingColdJs); err != nil {
		t.Fatal("published exact request ownership rejected: ", err)
	}
}

// CSS still requires one real established owner; dispatch alone is insufficient.
func TestColdLinkHoldSnapshotReportsMissingCssWithoutWeakening(t *testing.T) {
	control := newNativeBrowsingRosterControl(t)
	witness := control.witness
	witness.requests[0][0] = nativeBrowsingColdRequest{bodyGate: witness.css.gate}
	progress := &witness.pressure.assets[0][0]
	progress.expectedBytes = int64(nativeBrowsingAssetBytes(0, 0))
	progress.started.Store(true)
	trace := witness.requestTraceWithContext(context.Background(), "https://origin0.test/asset/0/0")
	trace.GetConn("origin0.test:443")
	if witness.checkHold(&witness.css, nativeBrowsingColdCss) == nil {
		t.Fatal("unestablished CSS passed the exact ownership premise")
	}
	receipt := witness.row.HoldPremiseFailure
	if receipt == nil || receipt.Kind != nativeBrowsingColdCss || receipt.Page != 0 || receipt.Slot != 0 ||
		receipt.ClientAcquisitions != 0 || receipt.ServerAttempts != 0 || receipt.LatestClientAcquisitions != 0 || receipt.LatestServerAttempts != 0 ||
		!receipt.RequestOwnerUnchanged || !receipt.ExactGate || !receipt.RequestDispatched || !receipt.RequestContextLive {
		t.Fatalf("missing CSS failure did not retain exact bounded premise: %+v", receipt)
	}
	positive := newNativeBrowsingRosterControl(t)
	positive.css(t)
	if err := positive.witness.checkHold(&positive.witness.css, nativeBrowsingColdCss); err != nil {
		t.Fatal("real established CSS rejected: ", err)
	}
}

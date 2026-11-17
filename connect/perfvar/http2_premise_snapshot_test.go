// These component controls use the existing real verified TLS/tuple fixture.
package perfvar

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
)

// Actual trace/server publication crosses the old unlocked observation boundary.
func TestHttp2HoldSnapshotKeepsOnePublicationBoundary(t *testing.T) {
	control := newNativeBrowsingRosterControl(t, "h2")
	pressure := &nativeBrowsingPressureObservation{}
	witness := newNativeBrowsingHttp2Witness(pressure)
	t.Cleanup(witness.close)
	target := "https://origin0.test/asset/3/1"
	progress := &pressure.assets[3][1]
	progress.expectedBytes = int64(nativeBrowsingAssetBytes(3, 1))
	progress.started.Store(true)
	trace := witness.requestTraceWithContext(context.Background(), target)
	trace.GetConn("origin0.test:443")
	owner := witness.requests[3][1].requestOwner
	published := false
	witness.afterUnreadSnapshotForTest = func() {
		witness.afterUnreadSnapshotForTest = nil
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
			t.Fatalf("actual owned HTTP/2 reader: count=%d err=%v", count, err)
		}
		progress.receivedBytes.Store(int64(count))
		published = true
	}
	if err := witness.checkUnreadBody(3); err != nil {
		t.Fatalf("WARM_HOLD_SNAPSHOT_PUBLICATION_RACE: %v", err)
	}
	request := witness.requests[3][1]
	if !published || request.requestOwner != owner || request.bodyGate != witness.gates[3] ||
		request.clientAcquisitions != 1 || request.serverAttempts != 1 ||
		witness.clientSessions[0] != control.client || witness.serverSessions[0] == "" {
		t.Fatal("control did not preserve one exact actual request/session publication")
	}
	if err := witness.checkUnreadBody(3); err != nil {
		t.Fatal("published exact HTTP/2 request ownership rejected: ", err)
	}
}

// A second client acquisition or server attempt remains a hard ownership failure.
func TestHttp2HoldSnapshotRejectsAmbiguousOriginalOwnership(t *testing.T) {
	control := newNativeBrowsingRosterControl(t, "h2")
	for _, duplicate := range []string{"client", "server"} {
		t.Run(duplicate, func(t *testing.T) {
			pressure := &nativeBrowsingPressureObservation{}
			witness := newNativeBrowsingHttp2Witness(pressure)
			t.Cleanup(witness.close)
			target := "https://origin0.test/asset/3/1"
			progress := &pressure.assets[3][1]
			progress.expectedBytes = int64(nativeBrowsingAssetBytes(3, 1))
			progress.started.Store(true)
			trace := witness.requestTraceWithContext(context.Background(), target)
			trace.GetConn("origin0.test:443")
			trace.GotConn(httptrace.GotConnInfo{Conn: control.client, Reused: true})
			handler := witness.wrapHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write(nativeBrowsingAsset(3, 1))
			}))
			handler.ServeHTTP(httptest.NewRecorder(), control.request(target))
			progress.bodyOpened.Store(true)
			if err := witness.checkUnreadBody(3); err != nil {
				t.Fatal("real established HTTP/2 owner prerequisite: ", err)
			}
			if duplicate == "client" {
				trace.GotConn(httptrace.GotConnInfo{Conn: control.client, Reused: true})
			} else {
				handler.ServeHTTP(httptest.NewRecorder(), control.request(target))
			}
			err := witness.checkUnreadBody(3)
			if err == nil || !strings.Contains(err.Error(), map[string]string{
				"client": "changed or reacquired", "server": "replaced its upstream session or replayed",
			}[duplicate]) {
				t.Fatalf("ambiguous %s original ownership was not rejected: %v", duplicate, err)
			}
			request := witness.requests[3][1]
			if duplicate == "client" && request.clientAcquisitions != 2 ||
				duplicate == "server" && request.serverAttempts != 2 {
				t.Fatal("duplicate attempt was merged into an exact-one receipt")
			}
		})
	}
}

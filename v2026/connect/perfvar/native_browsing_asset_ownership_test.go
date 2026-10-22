// Fixed logical-asset counters retain every HTTP replay without holding a
// socket or sampling endpoint internals. Server writes are local acceptance.
package perfvar

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Successful HTTP writes are local acceptance, not delivered or acknowledged
// bytes. Preserve exact partial counts and errors without changing write policy.
func (self *nativeBrowsingRequestProgress) recordServerWrite(expected, count int, err error) {
	if self == nil {
		return
	}
	self.serverWritten.Add(int64(count))
	if err != nil {
		self.serverWriteError.Store(true)
	}
	if count != expected {
		self.serverShortWrite.Store(true)
	}
}

// Cumulative operation-owned receipts distinguish canceled bodies still live
// from disposed handlers. Body.Close does not imply an observed wire FIN/RST.
type nativeBrowsingAssetOwnership struct {
	ClientStarted            int   `json:"client_started"`
	ClientFinished           int   `json:"client_finished"`
	Canceled                 int   `json:"canceled"`
	CanceledClientPending    int   `json:"canceled_client_pending"`
	BodiesClosed             int   `json:"bodies_closed"`
	BodyCloseErrors          int   `json:"body_close_errors"`
	ServerStarted            int   `json:"server_started"`
	ServerFinished           int   `json:"server_finished"`
	CanceledServerPending    int   `json:"canceled_server_pending"`
	ServerWriteBytes         int64 `json:"server_write_bytes"`
	ServerWriteErrors        int   `json:"server_write_errors"`
	ServerShortWrites        int   `json:"server_short_writes"`
	ServerAttempts           int64 `json:"server_attempts"`
	ServerFinishedAttempts   int64 `json:"server_finished_attempts"`
	ServerCanceledAttempts   int64 `json:"server_canceled_attempts"`
	ServerActiveAttempts     int64 `json:"server_active_attempts"`
	ClientAcquisitions       int64 `json:"client_acquisitions"`
	ClientReacquisitions     int64 `json:"client_reacquisitions"`
	ClientRequestWrites      int64 `json:"client_request_writes"`
	ClientRequestWriteErrors int64 `json:"client_request_write_errors"`
	RetriedAssets            int   `json:"retried_assets"`
	InvalidResponseStatuses  int   `json:"invalid_response_statuses"`
}

// The 400 slots are the existing fixed plan; this does not walk live owners.
func (self *nativeBrowsingPressureObservation) assetOwnershipSnapshot() nativeBrowsingAssetOwnership {
	result := nativeBrowsingAssetOwnership{}
	for page := range self.assets {
		for asset := range self.assets[page] {
			progress := &self.assets[page][asset]
			started, finished := progress.started.Load(), progress.finished.Load()
			serverStarted, serverFinished := progress.serverStarted.Load(), progress.serverFinished.Load()
			if receipt, known := progress.serverOwnership(); known {
				serverStarted = receipt.started != 0
				serverFinished = serverStarted && receipt.finished == receipt.started
				result.ServerAttempts += receipt.started
				result.ServerFinishedAttempts += receipt.finished
				result.ServerCanceledAttempts += receipt.canceled
				result.ServerActiveAttempts += receipt.started - receipt.finished
			}
			acquisitions := progress.connectionAcquisitions.Load()
			result.ClientAcquisitions += acquisitions
			if 1 < acquisitions {
				result.ClientReacquisitions += acquisitions - 1
				result.RetriedAssets++
			}
			result.ClientRequestWrites += progress.requestWrites.Load()
			result.ClientRequestWriteErrors += progress.requestWriteErrors.Load()
			if status := progress.responseStatus.Load(); status != 0 && status != http.StatusOK {
				result.InvalidResponseStatuses++
			}
			if started {
				result.ClientStarted++
			}
			if finished {
				result.ClientFinished++
			}
			if serverStarted {
				result.ServerStarted++
			}
			if serverFinished {
				result.ServerFinished++
			}
			if progress.cancelRequested.Load() {
				result.Canceled++
				if started && !finished {
					result.CanceledClientPending++
				}
				if serverStarted && !serverFinished {
					result.CanceledServerPending++
				}
			}
			if progress.bodyClosed.Load() {
				result.BodiesClosed++
			}
			if progress.bodyCloseError.Load() {
				result.BodyCloseErrors++
			}
			result.ServerWriteBytes += progress.serverWritten.Load()
			if progress.serverWriteError.Load() {
				result.ServerWriteErrors++
			}
			if progress.serverShortWrite.Load() {
				result.ServerShortWrites++
			}
		}
	}
	return result
}

// A controlled second Write returns an exact short prefix with or without an
// error; its first write and Flush use the ordinary response recorder.
type nativeBrowsingShortSuffixWriter struct {
	*httptest.ResponseRecorder
	writes int
	err    error
}

// Preserve the first prefix while exposing the suffix's actual count.
func (self *nativeBrowsingShortSuffixWriter) Write(body []byte) (int, error) {
	self.writes++
	if self.writes == 2 {
		return 3, self.err
	}
	return self.ResponseRecorder.Write(body)
}

// The real handler helper records partial writes, even a short nil-error write,
// without converting them to a fabricated successful full body or a cancel.
func TestNativeBrowsingTracePreservesWriterAndBodyDisposal(t *testing.T) {
	for _, writeErr := range []error{nil, io.ErrClosedPipe} {
		pressure := &nativeBrowsingPressureObservation{}
		progress := &pressure.assets[0][2]
		progress.started.Store(true)
		progress.serverStarted.Store(true)
		progress.cancelRequested.Store(true)
		writer := &nativeBrowsingShortSuffixWriter{ResponseRecorder: httptest.NewRecorder(), err: writeErr}
		state := newNativeBrowsingPageState()
		canceled := nativeBrowsingWriteAssetSuffix(writer, context.Background(), context.Background(), state, nativeBrowsingAsset(0, 2), 2, true, progress)
		before := pressure.assetOwnershipSnapshot()
		if canceled || before.ServerWriteBytes != nativeBrowsingHeldPrefixBytes+3 || before.ServerShortWrites != 1 || before.CanceledClientPending != 1 || before.CanceledServerPending != 1 || (before.ServerWriteErrors == 1) != (writeErr != nil) {
			t.Fatalf("writer receipt lost exact partial/error custody: %+v", before)
		}
		progress.bodyClosed.Store(true)
		progress.finished.Store(true)
		progress.serverFinished.Store(true)
		after := pressure.assetOwnershipSnapshot()
		if after.BodiesClosed != 1 || after.ClientFinished != 1 || after.ServerFinished != 1 || after.CanceledClientPending != 0 || after.CanceledServerPending != 0 || before.CanceledClientPending != 1 {
			t.Fatalf("disposal did not preserve the earlier frozen snapshot: %+v", after)
		}
	}
}

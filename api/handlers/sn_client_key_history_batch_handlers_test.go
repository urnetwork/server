// Real authentication and Redis counters prove plural requests keep the
// singleton's logical quota charge before any controller authority work.
package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Direct framing/quota tests retain explicit native deadline capabilities.
type snClientKeyBatchTestRecorder struct {
	*snAttemptUploadRecorder
	writeDeadline time.Time
}

func (self *snClientKeyBatchTestRecorder) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadline = deadline
	return nil
}

// Deadline observation forwards to the real Http connection, never a success
// callback standing in for deadline support.
type snClientKeyBatchDeadlineWriter struct {
	http.ResponseWriter
	observe     func(time.Time)
	observeRead func(time.Time)
}

func (self *snClientKeyBatchDeadlineWriter) Unwrap() http.ResponseWriter { return self.ResponseWriter }
func (self *snClientKeyBatchDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	err := http.NewResponseController(self.ResponseWriter).SetWriteDeadline(deadline)
	if err == nil {
		self.observe(deadline)
	}
	return err
}

func (self *snClientKeyBatchDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(self.ResponseWriter).SetReadDeadline(deadline)
	if err == nil && self.observeRead != nil {
		self.observeRead(deadline)
	}
	return err
}

// Every request describes the same actual decision and a distinct client.
func snClientKeyBatchTestBody(t testing.TB, count int) []byte {
	t.Helper()
	batch := protocol.ClientKeyObservationBatchRequest{MaximumResponseBytes: protocol.MaxClientKeyObservationBatchResponseBytes}
	for index := 0; index < count; index++ {
		item := protocol.ClientKeyObservationRequest{ValidatorHotkey: [32]byte{1}, NativeBlock: 101, NativeHash: [32]byte{2}, NativeEpoch: 3, DecisionBoundary: protocol.ClientKeyEffectiveBoundary{Epoch: 4, Block: 100, Hash: [32]byte{5}}, Nonce: [32]byte{6}}
		binary.BigEndian.PutUint64(item.ClientID[8:], uint64(index+1))
		batch.Requests = append(batch.Requests, item)
	}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// One transport containing two logical clients cannot consume one reservation.
func TestSnClientKeyHistoryBatchChargesEveryMemberBeforeController(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		request := snAttemptUploadTestRequest(tb, identity.Sign(), "metadata", snClientKeyBatchTestBody(tb, 2))
		request.URL.Path = "/sn/client-key/observations"
		response := &snClientKeyBatchTestRecorder{snAttemptUploadRecorder: snAttemptUploadTestRecorder()}
		SnClientKeyObservations(response, request)
		if response.Code != http.StatusTooManyRequests {
			tb.Fatalf("two logical clients escaped a one-item quota: status=%d body=%s", response.Code, response.Body.String())
		}
		if err := controller.StReserveAttemptUpload(tb.Context(), identity.UserId, protocol.MaxClientKeyHistoryResponseBytes); err == nil {
			tb.Fatal("failed batch refunded its first admitted logical item")
		}
	})
}

// Invalid credentials, including account-only credentials, cannot consume a
// plural body or create a signing/Sql/Rpc work owner.
func TestSnClientKeyHistoryBatchAuthenticatesBeforeBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, _ := snAttemptUploadTestIdentity(tb)
		for _, token := range []string{"", "invalid", account.Sign()} {
			request := snAttemptUploadTestRequest(tb, token, "metadata", snClientKeyBatchTestBody(tb, 404))
			request.URL.Path = "/sn/client-key/observations"
			reads := 0
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			response := &snClientKeyBatchTestRecorder{snAttemptUploadRecorder: snAttemptUploadTestRecorder()}
			SnClientKeyObservations(response, request)
			if response.Code != http.StatusUnauthorized || reads != 0 {
				tb.Fatalf("unauthenticated batch reached body: status=%d reads=%d", response.Code, reads)
			}
		}
	})
}

// Malformed framing cannot become a new batch-level quota escape hatch.
func TestSnClientKeyHistoryBatchRejectsExcessiveFramingBeforeBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		request := snAttemptUploadTestRequest(tb, identity.Sign(), "metadata", snClientKeyBatchTestBody(tb, 1))
		request.URL.Path = "/sn/client-key/observations"
		request.ContentLength = protocol.MaxClientKeyObservationBatchRequestBytes + 1
		reads := 0
		request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
		response := &snClientKeyBatchTestRecorder{snAttemptUploadRecorder: snAttemptUploadTestRecorder()}
		SnClientKeyObservations(response, request)
		if response.Code != http.StatusBadRequest || reads != 0 {
			tb.Fatalf("excessive batch consumed body: status=%d reads=%d", response.Code, reads)
		}
	})
}

// The actual handler extends only its authenticated plural route on a server
// whose ordinary native write timeout remains 30s. Earlier parent deadlines win.
func TestSnClientKeyHistoryBatchRealHttpPropagatesFiniteWriteDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		var stateLock sync.Mutex
		var deadlines []time.Time
		var remaining []time.Duration
		var readRemaining []time.Duration
		parentDeadline := time.Now().Add(time.Minute)
		endpoint := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("parent") == "short" {
				ctx, cancel := context.WithDeadline(r.Context(), parentDeadline)
				defer cancel()
				r = r.WithContext(ctx)
			}
			owned := &snClientKeyBatchDeadlineWriter{ResponseWriter: w, observe: func(deadline time.Time) {
				stateLock.Lock()
				defer stateLock.Unlock()
				deadlines = append(deadlines, deadline)
				remaining = append(remaining, time.Until(deadline))
			}, observeRead: func(deadline time.Time) {
				stateLock.Lock()
				defer stateLock.Unlock()
				readRemaining = append(readRemaining, time.Until(deadline))
			}}
			SnClientKeyObservations(owned, r)
		}))
		endpoint.Config.WriteTimeout = 30 * time.Second
		endpoint.Start()
		defer endpoint.Close()
		for _, suffix := range []string{"", "?parent=short"} {
			request, err := http.NewRequestWithContext(tb.Context(), http.MethodPost, endpoint.URL+"/sn/client-key/observations"+suffix, bytes.NewReader(snClientKeyBatchTestBody(tb, 2)))
			if err != nil {
				tb.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+identity.Sign())
			request.Header.Set("Content-Type", "application/json")
			response, err := endpoint.Client().Do(request)
			if err != nil {
				tb.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusTooManyRequests {
				tb.Fatalf("real handler did not reach charged quota: %d", response.StatusCode)
			}
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if len(deadlines) != 2 || remaining[0] <= 30*time.Second || remaining[0] > time.Duration(protocol.ClientKeyObservationBatchOperationSeconds)*time.Second || !deadlines[1].Equal(parentDeadline) || endpoint.Config.WriteTimeout != 30*time.Second || cap(snClientKeyObservationSlots) != protocol.MaxClientKeyObservationActiveOperations {
			tb.Fatalf("plural deadline/parent/ordinary ownership differs: deadlines=%v remaining=%v", deadlines, remaining)
		}
		if len(readRemaining) != 2 {
			tb.Fatal("actual body deadline census differs", readRemaining)
		}
		for _, duration := range readRemaining {
			if duration <= 0 || duration > 25*time.Second {
				tb.Fatal("plural route changed its body read ceiling", duration)
			}
		}
	})
}

// The same real gate refuses either route's fifth active owner without body
// reads or a logical reservation. The protocol constant owns the actual cap.
func TestSnClientKeyHistoryBatchSharesExactActiveAdmissionWithSingleton(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		if cap(snClientKeyObservationSlots) != protocol.MaxClientKeyObservationActiveOperations || len(snClientKeyObservationSlots) != 0 {
			tb.Fatal("shared active owner is not empty and exactly bounded")
		}
		for range protocol.MaxClientKeyObservationActiveOperations {
			snClientKeyObservationSlots <- struct{}{}
		}
		defer func() {
			for range protocol.MaxClientKeyObservationActiveOperations {
				<-snClientKeyObservationSlots
			}
		}()
		for _, plural := range []bool{false, true} {
			request := snAttemptUploadTestRequest(tb, identity.Sign(), "metadata", snClientKeyBatchTestBody(tb, 1))
			reads := 0
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			response := &snClientKeyBatchTestRecorder{snAttemptUploadRecorder: snAttemptUploadTestRecorder()}
			if plural {
				SnClientKeyObservations(response, request)
			} else {
				SnClientKeyObservation(response, request)
			}
			if response.Code != http.StatusTooManyRequests || reads != 0 {
				tb.Fatalf("fifth owner escaped shared gate: plural=%t status=%d reads=%d", plural, response.Code, reads)
			}
		}
		if err := controller.StReserveAttemptUpload(tb.Context(), identity.UserId, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
			tb.Fatal("busy observation consumed quota", err)
		}
	})
}

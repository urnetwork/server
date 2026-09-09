// The real route catalog must preserve singleton admission while adding only
// the explicit authenticated plural publication route.
package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Both exact production routes reach authentication, not a missing route.
func TestSnClientKeyHistoryBatchProductionRoutesRequireAuthentication(t *testing.T) {
	routes := Routes()
	countKVs := map[string]int{}
	for _, route := range routes {
		countKVs[route.String()]++
	}
	handler := router.NewRouter(t.Context(), routes)
	for _, path := range []string{"/sn/client-key/observation", "/sn/client-key/observations"} {
		if countKVs["POST ^"+path+"$"] != 1 || countKVs["GET ^"+path+"$"] != 0 {
			t.Fatal("observation route census differs", path, countKVs)
		}
		request := httptest.NewRequest(http.MethodPost, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("actual route %s status=%d, want401", path, response.Code)
		}
	}
}

// These controls expose transport capability only; they never stand in for
// a successful controller, chain source or signature.
type snClientKeyBatchRouteWriter struct{ *httptest.ResponseRecorder }

func (self *snClientKeyBatchRouteWriter) SetReadDeadline(time.Time) error  { return nil }
func (self *snClientKeyBatchRouteWriter) SetWriteDeadline(time.Time) error { return nil }

type snClientKeyBatchRouteBody struct {
	*bytes.Reader
	reads int
}

func (self *snClientKeyBatchRouteBody) Read(data []byte) (int, error) {
	self.reads++
	return self.Reader.Read(data)
}
func (self *snClientKeyBatchRouteBody) Close() error { return nil }

// Actual authenticated dispatch refuses missing capability, an already-ended
// parent and one-over framing/member limits before any quota/chain owner.
func TestSnClientKeyHistoryBatchProductionRouteRefusesBeforeObservationWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		defer controller.SetStConfig(nil)
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(tb.Context(), networkId, "client-key-route-"+networkId.String(), userId)
		model.Testing_CreateDevice(tb.Context(), networkId, deviceId, clientId, "client-key-route", "test")
		token := jwt.NewByJwt(networkId, userId, "client-key-route", false, false).Client(deviceId, clientId).Sign()
		handler := router.NewRouter(tb.Context(), Routes())
		for index, mode := range []string{"unsupported-write", "cancelled", "excessive-framing", "excessive-members"} {
			cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2, byte(index + 1)}, AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}}
			controller.SetStConfig(cfg)
			count := 1
			if mode == "excessive-members" {
				count = protocol.MaxClientKeyObservationBatchClients + 1
			}
			batch := protocol.ClientKeyObservationBatchRequest{MaximumResponseBytes: protocol.MaxClientKeyObservationBatchResponseBytes}
			for itemIndex := 0; itemIndex < count; itemIndex++ {
				item := protocol.ClientKeyObservationRequest{ValidatorHotkey: [32]byte{1}, NativeBlock: 101, NativeHash: [32]byte{2}, NativeEpoch: 3, DecisionBoundary: protocol.ClientKeyEffectiveBoundary{Epoch: 4, Block: 100, Hash: [32]byte{5}}, Nonce: [32]byte{6}}
				binary.BigEndian.PutUint64(item.ClientID[8:], uint64(itemIndex+1))
				batch.Requests = append(batch.Requests, item)
			}
			encoded, err := json.Marshal(batch)
			if err != nil {
				tb.Fatal(err)
			}
			body := &snClientKeyBatchRouteBody{Reader: bytes.NewReader(encoded)}
			request := httptest.NewRequest(http.MethodPost, "/sn/client-key/observations", bytes.NewReader(encoded))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Body = body
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = &snClientKeyBatchRouteWriter{ResponseRecorder: response}
			want := http.StatusBadRequest
			switch mode {
			case "unsupported-write":
				writer, want = response, http.StatusServiceUnavailable
			case "cancelled":
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request, want = request.WithContext(ctx), http.StatusRequestTimeout
			case "excessive-framing":
				request.ContentLength = protocol.MaxClientKeyObservationBatchRequestBytes + 1
			}
			handler.ServeHTTP(writer, request)
			if response.Code != want || mode != "excessive-members" && body.reads != 0 {
				tb.Fatalf("%s reached observation work: status=%d want=%d reads=%d body=%s", mode, response.Code, want, body.reads, response.Body.String())
			}
			if err := controller.StReserveAttemptUpload(tb.Context(), userId, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
				tb.Fatalf("%s consumed a logical observation reservation: %v", mode, err)
			}
			if err := controller.StReserveAttemptUpload(tb.Context(), userId, protocol.MaxClientKeyHistoryResponseBytes); err == nil {
				tb.Fatal("actual quota control was not live")
			}
		}
	})
}

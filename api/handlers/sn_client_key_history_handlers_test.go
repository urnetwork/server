// The production handler authenticates before reading untrusted request bytes
// and shares the actual finite account/deployment staging admission budget.
package handlers

import (
	"io"
	"net/http"
	"testing"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Missing, invalid and account-only credentials cannot reach body/signing work.
func TestSnClientKeyObservationAuthenticatesBeforeRequestBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, client := snAttemptUploadTestIdentity(tb)
		clientToken := client.Sign()
		for _, token := range []string{"", "invalid", account.Sign(), clientToken} {
			reads := 0
			request := snAttemptUploadTestRequest(tb, token, "metadata", []byte("{}"))
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
			if token == clientToken {
				request.Header.Add("Authorization", "Bearer duplicate")
			}
			response := snAttemptUploadTestRecorder()
			SnClientKeyObservation(response, request)
			if response.Code != http.StatusUnauthorized || reads != 0 {
				tb.Fatalf("unauthenticated observation consumed body: status=%d reads=%d", response.Code, reads)
			}
		}
	})
}

// The actual configured shared quota is consumed before body/RPC/storage.
// A different client cannot bypass the same authenticated account counters.
func TestSnClientKeyObservationQuotaRefusesBeforeRequestBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		if err := controller.StReserveAttemptUpload(tb.Context(), client.UserId, protocol.MaxClientKeyHistoryResponseBytes); err != nil {
			tb.Fatal(err)
		}
		reads := 0
		request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", []byte("{}"))
		request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF })}
		response := snAttemptUploadTestRecorder()
		SnClientKeyObservation(response, request)
		if response.Code != http.StatusTooManyRequests || reads != 0 {
			tb.Fatalf("exhausted observation quota reached body: status=%d reads=%d", response.Code, reads)
		}
	})
}

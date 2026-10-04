// Public wire tests exercise actual HTTP body ownership and immutable database
// receipts without supplying a JWT, signing key or caller-selected authority.
package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Synthetic public configuration is independent of both submitted signatures.
func providerWorkHandlerFixture(t testing.TB) (protocol.OriginalWorkCutSubmission, protocol.OriginalWorkRequest) {
	t.Helper()
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{91}, 32))
	sdk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{92}, 32))
	now := time.Now().Unix()
	request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte(server.NewId()), DomainHash: [32]byte{93}, ClientId: [16]byte(server.NewId()), Generation: [16]byte(server.NewId()), PublicKey: [32]byte(sdk.Public().(ed25519.PublicKey)), Epoch: 7, Kind: "start", Block: 20, BlockHash: [32]byte{94}, IssuedAtUnix: now - 1, ExpiresAtUnix: now + 600}, approver)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := protocol.SignOriginalWorkCut(t.Context(), protocol.OriginalWorkCut{DomainHash: request.DomainHash, ClientId: request.ClientId, Generation: request.Generation, Epoch: request.Epoch, Block: request.Block, BlockHash: request.BlockHash, Complete: true, Contracts: []protocol.OriginalWorkContract{}}, sdk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := request.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	cutRaw, err := cut.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\n", controller.ProviderWorkPolicySchema, request.DomainHash, request.Signer))))
	return protocol.OriginalWorkCutSubmission{Request: raw, Cut: cutRaw}, request
}

// Real server writers provide native deadline and body-close capabilities.
func TestProviderWorkHttpReceiptSurvivesHandlerRestart(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		submission, request := providerWorkHandlerFixture(t)
		post := func(base, path string, raw []byte) (int, []byte) {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
			if err != nil {
				t.Fatal(err)
			}
			return response.StatusCode, body
		}
		first := httptest.NewServer(NewProviderWorkHandlers())
		defer first.Close()
		if status, _ := post(first.URL, "/provider-work/v1/requests", submission.Request); status != http.StatusOK {
			t.Fatal("signed request route refused", status)
		}
		query := fmt.Sprintf("/provider-work/v1/requests?domain=%x&client=%x&generation=%x&key=%x", request.DomainHash, request.ClientId, request.Generation, request.PublicKey)
		response, err := http.Get(first.URL + query)
		if err != nil {
			t.Fatal(err)
		}
		var list protocol.OriginalWorkRequests
		err = json.NewDecoder(response.Body).Decode(&list)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(list.Requests) != 1 || !bytes.Equal(list.Requests[0], submission.Request) {
			t.Fatal("SDK poll did not receive original", err)
		}
		encoded, err := json.Marshal(submission)
		if err != nil {
			t.Fatal(err)
		}
		status, raw := post(first.URL, "/provider-work/v1/cuts", encoded)
		var receipt protocol.OriginalWorkCutReceipt
		if status != http.StatusOK || json.Unmarshal(raw, &receipt) != nil || receipt.Schema != protocol.OriginalWorkReceiptSchema {
			t.Fatal("cut acknowledgment missing", status, string(raw))
		}
		first.Close()
		second := httptest.NewServer(NewProviderWorkHandlers())
		defer second.Close()
		status, retry := post(second.URL, "/provider-work/v1/cuts", encoded)
		if status != http.StatusOK || !bytes.Equal(raw, retry) {
			t.Fatal("restart changed exact receipt", status)
		}
		response, err = http.Get(second.URL + "/provider-work/v1/cuts/" + hex.EncodeToString(receipt.CutHash[:]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(io.LimitReader(response.Body, protocol.MaximumOriginalWorkCutBytes+1))
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, submission.Cut) {
			t.Fatal("cut content route changed original", err)
		}
	})
}

// A recorder exposing the same deadline interface observes exact admission;
// it performs no network operation or artificial wall-clock wait.
type providerWorkDeadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadline  time.Time
	writeDeadline time.Time
}

// Only the current response owner sets these bounded native capabilities.
func (self *providerWorkDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	self.readDeadline = deadline
	return nil
}

// The request's 300-second context also owns a bodyless response write.
func (self *providerWorkDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadline = deadline
	return nil
}

// Invalid selectors and encoding never consume a database slot or select a
// neighboring identity. Each handler instance owns its own finite admission.
func TestProviderWorkHttpAdmissionBoundsAndExactSelectors(t *testing.T) {
	_, request := providerWorkHandlerFixture(t)
	handler := NewProviderWorkHandlers()
	for _, target := range []string{"/provider-work/v1/requests?domain=00", fmt.Sprintf("/provider-work/v1/requests?domain=%x&domain=%x&client=%x&generation=%x&key=%x", request.DomainHash, request.DomainHash, request.ClientId, request.Generation, request.PublicKey), "/provider-work/v1/cuts/" + fmt.Sprintf("%X", [32]byte{0xab}), "/provider-work/v1/windows?domain=00&epoch=01&artifact=00"} {
		writer := &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, target, nil))
		if writer.Code != http.StatusBadRequest || writer.writeDeadline.IsZero() {
			t.Fatal("invalid public selector admitted", target, writer.Code)
		}
	}
	for range cap(handler.slots) {
		handler.slots <- struct{}{}
	}
	writer := &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/provider-work/v1/requests", nil))
	if writer.Code != http.StatusTooManyRequests {
		t.Fatal("busy transport queued work", writer.Code)
	}
	other := &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	NewProviderWorkHandlers().ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/provider-work/v1/requests", nil))
	if other.Code != http.StatusBadRequest {
		t.Fatal("independent API inherited another budget", other.Code)
	}
	for range cap(handler.slots) {
		<-handler.slots
	}
	oversized := httptest.NewRequest(http.MethodPost, "/provider-work/v1/cuts", bytes.NewReader([]byte("{}")))
	oversized.ContentLength = protocol.MaximumOriginalWorkSubmissionBytes + 1
	oversized.Header.Set("Content-Type", "application/json")
	writer = &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, oversized)
	if writer.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("cut byte bound was not enforced", writer.Code)
	}
}

// A barrier proves cancellation joins the actual blocked reader and close.
type providerWorkBlockedBody struct {
	reader  *io.PipeReader
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

// This is the original in-flight read; no replacement request is issued.
func (self *providerWorkBlockedBody) Read(raw []byte) (int, error) {
	self.once.Do(func() { close(self.entered) })
	return self.reader.Read(raw)
}

// Reader interruption and original close error are both preserved.
func (self *providerWorkBlockedBody) Close() error {
	_ = self.reader.CloseWithError(syscall.EIO)
	close(self.closed)
	return syscall.EIO
}

// I/O loss cannot become a returned framing contradiction or discard the
// caller's cancellation merely because fewer bytes reached the application.
func TestProviderWorkBodyCancellationJoinsAndPreservesCause(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &providerWorkBlockedBody{reader: reader, entered: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		raw, err := readProviderWorkBody(ctx, body, 9)
		if raw != nil {
			done <- errors.New("canceled body returned bytes")
			return
		}
		done <- err
	}()
	<-body.entered
	cancel()
	err := <-done
	select {
	case <-body.closed:
	default:
		t.Fatal("body close was not joined")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || errors.Is(err, model.ErrProviderWorkInvalid) {
		t.Fatal("canceled I/O became hard framing refusal", err)
	}
}

// The retry protocol distinguishes unavailable observations from actual
// contradictory originals, including joined cancellation and integrity causes.
func TestProviderWorkHttpErrorClassificationPreservesRetry(t *testing.T) {
	for _, entry := range []struct {
		err    error
		status int
	}{{err: syscall.EIO, status: 503}, {err: context.DeadlineExceeded, status: 503}, {err: errors.Join(syscall.EIO, context.Canceled), status: 503}, {err: model.ErrProviderWorkConflict, status: 409}, {err: errors.Join(model.ErrProviderWorkInvalid, context.DeadlineExceeded), status: 400}, {err: model.ErrProviderWorkMissing, status: 404}, {err: model.ErrProviderWorkCapacity, status: 429}} {
		writer := httptest.NewRecorder()
		providerWorkHttpError(writer, entry.err)
		if writer.Code != entry.status {
			t.Fatal("public retry classification differs", entry.err, writer.Code)
		}
	}
}

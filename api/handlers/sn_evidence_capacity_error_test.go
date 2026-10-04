package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// Real local immutable writes fail at each replica boundary, then complete
// with the original signed bytes once only capacity changes.
func TestSnEvidenceCapacityRecoversExactPartialPublication(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	envelope := signedSnEvidence(t, config, artifactKey)
	wire, err := startifact.EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, copies := range []int64{0, 1} {
		root := t.TempDir()
		limit := copies*int64(len(wire)) + int64(len(wire)) - 1
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf("authority: local\npath: %s\nprefix: blob\nmax_bytes: %d\n", root, limit)))
		refused := serveSnEvidence(http.MethodPost, "/sn/evidence", wire)
		pop()
		if refused.Code != http.StatusInsufficientStorage || !strings.Contains(refused.Body.String(), "bucket quota and disk headroom") {
			t.Fatalf("replica boundary %d hid actionable capacity refusal: %d %s", copies, refused.Code, refused.Body.String())
		}
		store := server.NewLocalBlobStoreWithMaxBytes(root, "blob", 4*int64(len(wire)))
		objects, err := store.List(t.Context(), "blob/")
		if err != nil || len(objects) != int(copies) {
			t.Fatalf("refusal changed partial immutable census: copies=%d objects=%d err=%v", copies, len(objects), err)
		}
		pop = server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf("authority: local\npath: %s\nprefix: blob\nmax_bytes: %d\n", root, 4*len(wire))))
		for retry := 0; retry < 2; retry++ {
			response := serveSnEvidence(http.MethodPost, "/sn/evidence", wire)
			if response.Code != http.StatusOK {
				pop()
				t.Fatalf("original signed retry failed: %d %s", response.Code, response.Body.String())
			}
			var receipt startifact.Published
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
				pop()
				t.Fatal(err)
			}
			for _, key := range []string{receipt.ContentKey, receipt.HistoryKey} {
				reader, err := store.Get(t.Context(), key)
				if err != nil {
					pop()
					t.Fatal(err)
				}
				raw, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(raw, wire) {
					pop()
					t.Fatalf("capacity recovery changed immutable bytes: %v %v", readErr, closeErr)
				}
			}
		}
		pop()
		objects, err = store.List(t.Context(), "blob/")
		if err != nil || len(objects) != 2 {
			t.Fatalf("idempotent recovery added duplicate objects: %d %v", len(objects), err)
		}
	}
}

func TestSnEvidenceCapacityPreservesTypedMinioAndRejectsMixedErrors(t *testing.T) {
	prior := publishSnEvidence
	t.Cleanup(func() { publishSnEvidence = prior })
	quota := minio.ErrorResponse{Code: "XMinioAdminBucketQuotaExceeded", StatusCode: http.StatusBadRequest, Message: "synthetic private backend details"}
	for _, sample := range []struct {
		err    error
		status int
	}{
		{err: fmt.Errorf("create immutable artifact: %w", quota), status: http.StatusInsufficientStorage},
		{err: errors.Join(quota, errors.New("immutable bytes differ")), status: http.StatusBadRequest},
		{err: errors.New("Bucket quota exceeded"), status: http.StatusBadRequest},
		{err: errors.New("evidence signer mismatch"), status: http.StatusBadRequest},
	} {
		publishSnEvidence = func(_ context.Context, _ []byte) (*startifact.Published, error) { return nil, sample.err }
		response := serveSnEvidence(http.MethodPost, "/sn/evidence", []byte(`{"synthetic":true}`))
		if response.Code != sample.status || strings.Contains(response.Body.String(), "private backend") {
			t.Fatalf("wrong public refusal or leaked backend detail: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestSnEvidenceCapacityDoesNotAdmitTamperedEnvelope(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	envelope := signedSnEvidence(t, config, artifactKey)
	envelope.Payload = json.RawMessage(`{"result":"changed"}`)
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	response := serveSnEvidence(http.MethodPost, "/sn/evidence", raw)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("tampered signed payload changed handling: %d %s", response.Code, response.Body.String())
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("test store missing")
	}
	objects, err := store.List(t.Context(), "blob/")
	if err != nil || len(objects) != 0 {
		t.Fatalf("tampered input touched immutable storage: %d %v", len(objects), err)
	}
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestBlobCapacityErrorRecognizesOnlyCompleteTypedRefusals(t *testing.T) {
	quota := minio.ErrorResponse{Code: "XMinioAdminBucketQuotaExceeded", StatusCode: http.StatusBadRequest}
	full := minio.ErrorResponse{Code: "XMinioStorageFull", StatusCode: http.StatusInsufficientStorage}
	for _, err := range []error{quota, &quota, full, ErrBlobCapacityExceeded, fmt.Errorf("create immutable object: %w", quota), errors.Join(quota, fmt.Errorf("local write: %w", ErrBlobCapacityExceeded))} {
		if !IsBlobCapacityError(err) {
			t.Fatalf("typed capacity refusal lost its classification: %v", err)
		}
	}
	for _, err := range []error{nil, errors.New("Bucket quota exceeded"), errors.New("local blob capacity exceeded"), minio.ErrorResponse{Code: quota.Code, StatusCode: http.StatusForbidden}, minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}, errors.Join(quota, errors.New("immutable object differs")), errors.Join(quota, context.Canceled)} {
		if IsBlobCapacityError(err) {
			t.Fatalf("unknown or mixed refusal was masked as capacity only: %v", err)
		}
	}
}

func TestBlobCapacityErrorLocalAccountingKeepsExactTypedRefusal(t *testing.T) {
	remaining, err := localBlobAvailableBytes(4, 0, 5, 8)
	if remaining != 0 || !errors.Is(err, ErrBlobCapacityExceeded) || !IsBlobCapacityError(err) {
		t.Fatalf("real accounting refusal lost capacity type: remaining=%d error=%v", remaining, err)
	}
	if _, err := localBlobAvailableBytes(-1, 0, 1, 8); IsBlobCapacityError(err) {
		t.Fatal("invalid accounting was treated as exhausted capacity")
	}
}

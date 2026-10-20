// Capacity refusals retain their type through immutable publication without
// turning an unrelated joined error into a capacity-only diagnosis.
package server

import (
	"errors"
	"net/http"
	"syscall"

	"github.com/minio/minio-go/v7"
)

var ErrBlobCapacityExceeded = errors.New("local blob capacity exceeded")

// Only complete error chains consisting of known capacity refusals qualify.
// Unknown storage, signature and integrity errors keep their existing handling.
func IsBlobCapacityError(err error) bool {
	switch err := err.(type) {
	case minio.ErrorResponse:
		return err.Code == "XMinioAdminBucketQuotaExceeded" && err.StatusCode == http.StatusBadRequest || err.Code == "XMinioStorageFull" && err.StatusCode == http.StatusInsufficientStorage
	case *minio.ErrorResponse:
		return err != nil && IsBlobCapacityError(*err)
	case interface{ Unwrap() []error }:
		causes := err.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsBlobCapacityError(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsBlobCapacityError(err.Unwrap())
	default:
		return err == ErrBlobCapacityExceeded || err == syscall.ENOSPC || err == syscall.EDQUOT
	}
}

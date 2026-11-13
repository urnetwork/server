// Artifact archive failures keep private, bounded cause metadata without
// changing scoring, retention requirements, retry decisions or public errors.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/minio/minio-go/v7"
)

// Only fixed classifications, an HTTP status and an opaque fingerprint may
// reach the private job ledger. Backend messages, URLs and keys stay absent.
type competitionArtifactArchiveFailure struct {
	Phase        string `json:"phase"`
	Category     string `json:"category"`
	S3Code       string `json:"s3_code,omitempty"`
	HttpStatus   int    `json:"http_status,omitempty"`
	DetailSha256 string `json:"detail_sha256"`
}

// The cause remains available through errors.Is/As, but formatting is safe
// even at adjacent archive call sites that already log returned errors.
type competitionArtifactArchiveError struct {
	phase string
	cause error
}

// Formats only the same explicitly selected fields as the private ledger.
func (self *competitionArtifactArchiveError) Error() string {
	detail := competitionArtifactArchiveFailureDetails(self.phase, self.cause)
	return fmt.Sprintf("artifact archive phase=%s category=%s s3_code=%s http_status=%d detail_sha256=%s",
		detail.Phase, detail.Category, detail.S3Code, detail.HttpStatus, detail.DetailSha256)
}

// Wrapping never removes cancellation, transport or backend error identity.
func (self *competitionArtifactArchiveError) Unwrap() error { return self.cause }

// Preserves only a fixed phase alongside the original programmatic cause.
func competitionArtifactArchivePhaseError(phase string, cause error) error {
	return &competitionArtifactArchiveError{phase: phase, cause: cause}
}

// Unknown phase text cannot become a diagnostic channel, including through a
// custom archive implementation or a malformed wrapped error.
func competitionArtifactArchivePhase(phase string) string {
	switch phase {
	case "archive_attempt", "authenticate_failed_evidence", "authenticate_stage_failure",
		"write_failure_diagnostic", "read_worker_stderr", "authenticate_failure_artifacts",
		"archive_unavailable", "retain_object", "retention_proof", "open_retained_version",
		"read_retained_version", "close_retained_version", "authenticate_retained_version":
		return phase
	default:
		return "unknown"
	}
}

// A syntactically valid arbitrary code can still contain private data. Only
// known backend identifiers are retained; every other value becomes unknown.
func competitionArtifactArchiveS3Code(code string) string {
	switch code {
	case "AccessDenied", "AllAccessDisabled", "InternalError", "InvalidAccessKeyId",
		"InvalidArgument", "InvalidBucketName", "InvalidObjectState", "InvalidRequest",
		"NoSuchBucket", "NoSuchKey", "NoSuchVersion", "ObjectLockConfigurationNotFoundError",
		"RequestTimeout", "ServiceUnavailable", "SignatureDoesNotMatch", "SlowDown", "TooManyRequests",
		"XMinioReadQuorum", "XMinioServerNotInitialized", "XMinioStorageFull", "XMinioWriteQuorum":
		return code
	default:
		return "unknown"
	}
}

// Classification is diagnostic only. In particular, a successful PUT followed
// by a failed read still fails closed rather than qualifying an incomplete proof.
func competitionArtifactArchiveFailureDetails(phase string, cause error) *competitionArtifactArchiveFailure {
	if cause == nil {
		return nil
	}
	var archiveError *competitionArtifactArchiveError
	if errors.As(cause, &archiveError) {
		phase, cause = archiveError.phase, archiveError.cause
	}
	digest := sha256.Sum256([]byte(cause.Error()))
	detail := &competitionArtifactArchiveFailure{
		Phase: competitionArtifactArchivePhase(phase), Category: "other",
		DetailSha256: hex.EncodeToString(digest[:]),
	}
	var response minio.ErrorResponse
	var responsePointer *minio.ErrorResponse
	var networkOperationError *net.OpError
	var networkError net.Error
	var pathError *os.PathError
	var linkError *os.LinkError
	var syscallError *os.SyscallError
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		detail.Category = "deadline_exceeded"
	case errors.Is(cause, context.Canceled):
		detail.Category = "canceled"
	case errors.As(cause, &response), errors.As(cause, &responsePointer):
		if responsePointer != nil {
			response = *responsePointer
		}
		detail.Category = "s3"
		detail.S3Code = competitionArtifactArchiveS3Code(response.Code)
		if 100 <= response.StatusCode && response.StatusCode <= 599 {
			detail.HttpStatus = response.StatusCode
		}
	case errors.As(cause, &networkOperationError):
		detail.Category = "network"
	case errors.Is(cause, os.ErrPermission):
		detail.Category = "filesystem_permission"
	case errors.Is(cause, os.ErrNotExist):
		detail.Category = "filesystem_not_found"
	case errors.As(cause, &pathError), errors.As(cause, &linkError), errors.As(cause, &syscallError):
		detail.Category = "filesystem"
	case errors.As(cause, &networkError):
		// Every syscall.Errno implements net.Error, including ENOENT and EIO.
		// Only actual transport provenance may turn those into network faults;
		// local typed wrappers above retain their own bounded classifications.
		switch networkError.(type) {
		case syscall.Errno, *syscall.Errno:
		default:
			detail.Category = "network"
		}
	}
	return detail
}

// Existing retry and terminal events gain safe metadata only for an actual
// archive failure; ordinary completion payloads remain byte-for-byte unchanged.
func competitionArtifactArchiveEventData(outcome EvaluationOutcome, data map[string]any) map[string]any {
	if outcome.archiveFailure != nil {
		data["artifact_archive_failure"] = outcome.archiveFailure
	}
	return data
}

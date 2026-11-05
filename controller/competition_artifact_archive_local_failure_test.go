// Local retained-file failures must not masquerade as object-store network
// outages merely because an underlying syscall number implements net.Error.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// This is a real failed local read, not the os.ErrNotExist sentinel which lacks
// the underlying syscall.Errno responsible for the original misclassification.
func competitionArchiveMissingFileError(t *testing.T) error {
	t.Helper()
	_, err := os.ReadFile(filepath.Join(t.TempDir(), "synthetic-private-missing-file"))
	var pathError *os.PathError
	var networkError net.Error
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathError) || !errors.As(err, &networkError) {
		t.Fatal("missing-file control did not expose the actual typed syscall boundary")
	}
	if _, errnoOnly := networkError.(syscall.Errno); !errnoOnly {
		t.Fatal("missing-file control no longer has the underlying errno network-interface overlap")
	}
	return err
}

// Only selected category/hash fields reach logs; original programmatic cause
// identity, and therefore ordinary errors.Is/As, must remain unchanged.
func requireCompetitionArchiveDiagnosticClass(t *testing.T, cause error, category string) {
	t.Helper()
	detail := competitionArtifactArchiveFailureDetails("retain_object", cause)
	digest := sha256.Sum256([]byte(cause.Error()))
	if detail == nil || detail.Phase != "retain_object" || detail.Category != category || detail.DetailSha256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("retained archive classified a typed cause incorrectly: got=%+v want=%s", detail, category)
	}
	safe := competitionArtifactArchivePhaseError("retain_object", cause)
	content, err := json.Marshal(detail)
	if err != nil || !errors.Is(safe, cause) || strings.Contains(safe.Error(), "synthetic-private") || strings.Contains(string(content), "synthetic-private") {
		t.Fatal("diagnostic classification changed cause identity or leaked private cause text")
	}
}

// Before the fix errors.As found the nested errno as net.Error before the
// enclosing file error, so this actual failed read was reported as network.
func TestCompetitionArtifactArchiveActualMissingFileRetainsFilesystemClass(t *testing.T) {
	requireCompetitionArchiveDiagnosticClass(t, competitionArchiveMissingFileError(t), "filesystem_not_found")
}

// Wrapping and joining cannot erase bounded filesystem provenance. Unknown
// bare errno values are not affirmative proof of a network operation either.
func TestCompetitionArtifactArchiveLocalWrappersAndJoinsRetainProvenance(t *testing.T) {
	missing := competitionArchiveMissingFileError(t)
	for _, cause := range []error{
		missing,
		fmt.Errorf("synthetic-private-upload: %w", missing),
		errors.Join(errors.New("synthetic-private-publication"), missing),
		&os.PathError{Op: "open", Path: "synthetic-private-file", Err: syscall.ENOENT},
	} {
		requireCompetitionArchiveDiagnosticClass(t, cause, "filesystem_not_found")
	}
	for _, cause := range []error{
		&os.PathError{Op: "open", Path: "synthetic-private-file", Err: os.ErrPermission},
		&os.PathError{Op: "open", Path: "synthetic-private-file", Err: syscall.EACCES},
		errors.Join(errors.New("synthetic-private-context"), &os.PathError{Op: "open", Path: "synthetic-private-file", Err: syscall.EACCES}),
	} {
		requireCompetitionArchiveDiagnosticClass(t, cause, "filesystem_permission")
	}
	for _, cause := range []error{
		&os.PathError{Op: "read", Path: "synthetic-private-file", Err: syscall.EIO},
		&os.LinkError{Op: "link", Old: "synthetic-private-old", New: "synthetic-private-new", Err: syscall.EIO},
		&os.SyscallError{Syscall: "synthetic-private-read", Err: syscall.EIO},
	} {
		requireCompetitionArchiveDiagnosticClass(t, cause, "filesystem")
	}
	requireCompetitionArchiveDiagnosticClass(t, syscall.EIO, "other")
	requireCompetitionArchiveDiagnosticClass(t, fmt.Errorf("synthetic-private-unknown: %w", syscall.EIO), "other")
}

// An actual net.Conn deadline and concrete network operations keep their class,
// even when the transport's native cause is itself a syscall wrapper or errno.
func TestCompetitionArtifactArchiveNetworkDeadlineKeepsTransportClass(t *testing.T) {
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	if err := connection.SetReadDeadline(time.Unix(0, 1)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	_, cause := connection.Read(buffer[:])
	var networkError net.Error
	if !errors.As(cause, &networkError) || !networkError.Timeout() || !errors.Is(cause, os.ErrDeadlineExceeded) {
		t.Fatal("native net.Conn control did not return its already-expired deadline")
	}
	requireCompetitionArchiveDiagnosticClass(t, cause, "network")
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.ENOENT, syscall.ETIMEDOUT} {
		requireCompetitionArchiveDiagnosticClass(t,
			&net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: errno}}, "network")
	}
}

// These higher-level classifications retain their original priority even with
// joined filesystem/transport causes. S3 metadata stays on its safe allowlist.
func TestCompetitionArtifactArchiveContextAndS3PriorityRemainUnchanged(t *testing.T) {
	missing := competitionArchiveMissingFileError(t)
	transport := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	requireCompetitionArchiveDiagnosticClass(t, errors.Join(missing, transport, context.DeadlineExceeded), "deadline_exceeded")
	requireCompetitionArchiveDiagnosticClass(t, errors.Join(missing, transport, context.Canceled), "canceled")
	cause := errors.Join(missing, transport, minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403, Message: "synthetic-private-backend"})
	requireCompetitionArchiveDiagnosticClass(t, cause, "s3")
	detail := competitionArtifactArchiveFailureDetails("retain_object", cause)
	if detail.S3Code != "AccessDenied" || detail.HttpStatus != 403 {
		t.Fatal("local diagnostic ordering changed the retained S3 allowlist", detail)
	}
}

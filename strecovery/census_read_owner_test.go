// Deterministic admission barriers exercise the actual public collector owner
// without opening a connection or disclosing original credential material.
package strecovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The synthetic literal endpoint is never contacted: every test supplies a
// private failure barrier before connection admission.
func censusReadTestSource(t *testing.T) (DatabaseSource, Limits) {
	t.Helper()
	config, _ := censusTestFixture(t)
	source := config.Databases[0]
	raw := []byte("postgres://synthetic_reader:synthetic_password@192.0.2.1:5432/synthetic?sslmode=require&application_name=synthetic_override\n")
	if err := os.WriteFile(source.Connection.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	source.Connection.Sha256 = digest(raw)
	return source, config.Limits
}

// The old connection cap was 30 seconds despite a longer outer read owner.
func TestCensusPublicReadHasOneDefaultOwner(t *testing.T) {
	source, limits := censusReadTestSource(t)
	stopped := errors.New("synthetic bounded admission stop")
	observed := false
	reader := PostgresReader{beforeConnect: func(ctx context.Context, config *pgx.ConnConfig) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Minute || time.Until(deadline) > 300*time.Second || config.ConnectTimeout < time.Minute || config.ConnectTimeout > 300*time.Second || len(config.Fallbacks) != 0 || config.RuntimeParams["application_name"] != "urnetwork-operator-census" {
			t.Fatal("actual public reader has a short, inherited or unbounded owner")
		}
		observed = true
		return stopped
	}}
	image, err := reader.Snapshot(t.Context(), source, limits)
	if !observed || image != nil || !errors.Is(err, stopped) || CensusReadUnavailable(err) {
		t.Fatal("bounded reader lost original admission result", observed, image, err)
	}
}

// Cancellation and a simultaneous I/O cause survive the same public boundary.
func TestCensusPublicReadPreservesCancellationAndIoCause(t *testing.T) {
	source, limits := censusReadTestSource(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := PostgresReader{beforeConnect: func(context.Context, *pgx.ConnConfig) error {
		cancel()
		return syscall.EIO
	}}
	image, err := reader.Snapshot(ctx, source, limits)
	if image != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || CensusReadUnavailable(err) {
		t.Fatal("canceled read lost actual I/O cause or became retry authority", image, err)
	}
}

// The diagnostic remains private while typed cause inspection remains useful.
func TestCensusPublicReadHidesCredentialDetailButRetainsCause(t *testing.T) {
	source, limits := censusReadTestSource(t)
	secret := errors.New("synthetic_password credential detail")
	reader := PostgresReader{beforeConnect: func(context.Context, *pgx.ConnConfig) error { return secret }}
	image, err := reader.Snapshot(t.Context(), source, limits)
	if image != nil || !errors.Is(err, secret) || strings.Contains(err.Error(), "synthetic_password") {
		t.Fatal("read boundary exposed credentials or discarded the original cause")
	}
}

// Configured durations cannot bypass the minimum or create an unlimited owner.
func TestCensusPublicReadRejectsShortAndExcessiveOwners(t *testing.T) {
	source, limits := censusReadTestSource(t)
	for _, duration := range []time.Duration{-time.Second, time.Second, 59 * time.Second, 901 * time.Second} {
		called := false
		reader := PostgresReader{ReadTimeout: duration, beforeConnect: func(context.Context, *pgx.ConnConfig) error { called = true; return io.EOF }}
		if image, err := reader.Snapshot(t.Context(), source, limits); err == nil || image != nil || called {
			t.Fatal("invalid read duration reached connection admission", duration, called, err)
		}
	}
}

// Only a complete tree of repeatable read causes receives soft classification.
func TestCensusReadUnavailableRequiresEveryLeaf(t *testing.T) {
	soft := &CensusReadError{Source: "synthetic", Stage: "read", Cause: errors.Join(syscall.EIO, context.DeadlineExceeded)}
	for _, err := range []error{soft, fmt.Errorf("synthetic wrapper: %w", syscall.ECONNRESET), &pgconn.PgError{Code: "57P03"}, io.ErrUnexpectedEOF} {
		if !CensusReadUnavailable(err) {
			t.Fatal("repeatable read unavailable cause was lost", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, errors.Join(soft, context.Canceled), errors.Join(soft, errors.New("synthetic contradiction")), &Refusal{Source: "synthetic", Cause: "changed pin"}, &pgconn.PgError{Code: "28P01"}, &pgconn.PgError{Code: "08P01"}, &pgconn.PgError{Code: "42P01"}} {
		if CensusReadUnavailable(err) {
			t.Fatal("hard or canceled read became retryable", err)
		}
	}
}

// Cyclic third-party wrappers cannot defeat bounded error classification.
type censusReadCycleError struct{}

// The diagnostic has no recursive rendering.
func (self *censusReadCycleError) Error() string { return "synthetic cause cycle" }

// A deliberately invalid wrapper exercises the classifier's traversal bound.
func (self *censusReadCycleError) Unwrap() error { return self }

// Both deep and cyclic causes remain unknown rather than a successful retry.
func TestCensusReadUnavailableBoundsCauseTraversal(t *testing.T) {
	err := error(syscall.EIO)
	for range 32 {
		err = fmt.Errorf("synthetic wrapper: %w", err)
	}
	if CensusReadUnavailable(err) || CensusReadUnavailable(&censusReadCycleError{}) {
		t.Fatal("unbounded cause tree was admitted")
	}
}

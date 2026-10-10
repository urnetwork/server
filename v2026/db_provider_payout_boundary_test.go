package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func boundaryTestPolicy(t testing.TB) ([]byte, *ProviderPayoutTransition) {
	t.Helper()
	data, _ := payoutTransitionFixture(t)
	policy, err := ParseProviderPayoutTransition(data)
	if err != nil {
		t.Fatal(err)
	}
	return data, policy
}

func TestProviderBoundaryExplicitPreparationAndReadinessIndependence(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		data, policy := boundaryTestPolicy(t)
		t.Cleanup(Config.PushSimpleResource("sn.yml", data))
		if _, err := LoadProviderPayoutEarningPolicy(ctx); !errors.Is(err, ErrProviderEarningBoundaryUnprepared) {
			t.Fatal("worker enrolled missing boundary", err)
		}
		if _, err := PrepareProviderPayoutBoundary(ctx, strings.Repeat("a1", 32)); err == nil {
			t.Fatal("unreviewed configuration digest prepared authority")
		}
		if _, err := RequireProviderPayoutBoundary(ctx, policy); !errors.Is(err, ErrProviderEarningBoundaryUnprepared) {
			t.Fatal("failed preparation left authority", err)
		}
		first, err := PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256)
		if err != nil || first == nil {
			t.Fatal(err)
		}
		// An independently reviewed readiness update is not a new earning asset.
		updatedData := bytes.Replace(data, []byte(`"activation":"reviewed"`), []byte(`"activation":"blocked"`), 1)
		updated, err := ParseProviderPayoutTransition(updatedData)
		if err != nil || updated == nil || updated.ConfigSha256 == policy.ConfigSha256 {
			t.Fatal("readiness fixture did not change", err)
		}
		release := Config.PushSimpleResource("sn.yml", updatedData)
		second, err := PrepareProviderPayoutBoundary(ctx, updated.ConfigSha256)
		release()
		if err != nil || second == nil || second.IdentitySha256 != first.IdentitySha256 || second.InitialConfigSha256 != policy.ConfigSha256 || !second.PreparedAt.Equal(first.PreparedAt) {
			t.Fatal("same earning boundary was rewritten", second, err)
		}
		if _, err := RequireProviderPayoutBoundary(ctx, nil); !errors.Is(err, ErrProviderEarningBoundaryMismatch) {
			t.Fatal("removed schedule fell back to legacy", err)
		}
		for _, cutoff := range []string{"2026-10-05T00:00:00Z", "2026-10-07T00:00:00Z"} {
			changedData := bytes.Replace(data, []byte(policy.CutoffUtc), []byte(cutoff), 1)
			changed, err := ParseProviderPayoutTransition(changedData)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepareProviderPayoutBoundary(ctx, changed, changed.ConfigSha256); !errors.Is(err, ErrProviderEarningBoundaryMismatch) {
				t.Fatal("configuration drift replaced boundary", err)
			}
		}
		if _, err := RequireProviderPayoutBoundary(ctx, policy); err != nil {
			t.Fatal("original authority no longer usable", err)
		}
	})
}

func TestProviderBoundaryConcurrentSamePreparation(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		_, policy := boundaryTestPolicy(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		type result struct {
			binding *ProviderEarningBoundary
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, 8)
		for i := 0; i < cap(results); i++ {
			go func() {
				<-start
				binding, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
				results <- result{binding: binding, err: err}
			}()
		}
		close(start)
		var first *ProviderEarningBoundary
		for i := 0; i < cap(results); i++ {
			got := <-results
			if got.err != nil || got.binding == nil {
				t.Error("concurrent identical prepare failed", got.err)
				continue
			}
			if first == nil {
				first = got.binding
			} else if got.binding.IdentitySha256 != first.IdentitySha256 || !got.binding.PreparedAt.Equal(first.PreparedAt) {
				t.Error("concurrent prepare generated different authority")
			}
		}
		Db(ctx, func(conn PgConn) {
			var count int
			Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_payout_boundary`).Scan(&count))
			if count != 1 {
				t.Fatal("concurrent initialization did not leave exactly one binding", count)
			}
		})
	})
}

func TestProviderBoundaryConcurrentConflictingPreparation(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		data, first := boundaryTestPolicy(t)
		second, err := ParseProviderPayoutTransition(bytes.Replace(data, []byte(first.CutoffUtc), []byte("2026-10-07T00:00:00Z"), 1))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, policy := range []*ProviderPayoutTransition{first, second} {
			go func(policy *ProviderPayoutTransition) {
				<-start
				_, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
				results <- err
			}(policy)
		}
		close(start)
		admitted, refused := 0, 0
		for i := 0; i < 2; i++ {
			err := <-results
			if err == nil {
				admitted++
			} else if errors.Is(err, ErrProviderEarningBoundaryMismatch) {
				refused++
			} else {
				t.Error("unexpected concurrent result", err)
			}
		}
		if admitted != 1 || refused != 1 {
			t.Fatal("conflicting initialization did not retain exactly one authority", admitted, refused)
		}
	})
}

func TestProviderBoundaryRequiredSchemaAndImmutableRows(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, policy := boundaryTestPolicy(t)
		if _, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE provider_payout_boundary SET identity_sha256=repeat('a',64)`,
			`DELETE FROM provider_payout_boundary`, `TRUNCATE provider_payout_boundary`,
		} {
			Db(ctx, func(conn PgConn) {
				if _, err := conn.Exec(ctx, statement); err == nil {
					t.Fatal("immutable authority mutation succeeded", statement)
				}
			}, OptReadWrite())
		}
		if _, err := RequireProviderPayoutBoundary(ctx, policy); err != nil {
			t.Fatal("refused mutation damaged original authority", err)
		}
		for _, mutation := range []struct{ change, restore string }{
			{change: `ALTER TABLE provider_payout_boundary DISABLE TRIGGER provider_payout_boundary_guard`, restore: `ALTER TABLE provider_payout_boundary ENABLE TRIGGER provider_payout_boundary_guard`},
			{change: `DROP TRIGGER provider_payout_boundary_truncate_guard ON provider_payout_boundary`, restore: `CREATE TRIGGER provider_payout_boundary_truncate_guard BEFORE TRUNCATE ON provider_payout_boundary FOR EACH STATEMENT EXECUTE FUNCTION guard_provider_payout_boundary()`},
			{change: `ALTER TABLE provider_payout_boundary RENAME COLUMN earning_identity TO missing_identity`, restore: `ALTER TABLE provider_payout_boundary RENAME COLUMN missing_identity TO earning_identity`},
			{change: `ALTER TABLE migration_catalog RENAME TO retained_migration_catalog`, restore: `ALTER TABLE retained_migration_catalog RENAME TO migration_catalog`},
		} {
			Tx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, mutation.change)) })
			_, err := RequireProviderPayoutBoundary(ctx, policy)
			Tx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, mutation.restore)) })
			if !errors.Is(err, ErrProviderEarningBoundarySchema) || ProviderEarningBoundaryRetryable(ctx, err) {
				t.Fatal("observed schema damage became retryable outage", mutation.change, err)
			}
			if _, err := RequireProviderPayoutBoundary(ctx, policy); err != nil {
				t.Fatal("restored exact schema did not read original binding", err)
			}
		}
	})
}

type providerBoundaryErrorQuery struct{ err error }

func (self providerBoundaryErrorQuery) QueryRow(context.Context, string, ...any) pgx.Row {
	return providerBoundaryErrorRow{err: self.err}
}

type providerBoundaryErrorRow struct{ err error }

func (self providerBoundaryErrorRow) Scan(...any) error { return self.err }

func TestProviderBoundaryObservationClassificationAndOwnerCancellation(t *testing.T) {
	ctx := context.Background()
	for _, cause := range []error{context.DeadlineExceeded, io.ErrUnexpectedEOF, &pgconn.PgError{Code: "57014", Message: "synthetic statement timeout"}} {
		_, err := requireProviderPayoutBoundarySchema(ctx, providerBoundaryErrorQuery{err: cause}, true)
		if !errors.Is(err, cause) || !ProviderEarningBoundaryRetryable(ctx, err) || errors.Is(err, ErrProviderEarningBoundarySchema) {
			t.Fatal("unavailable physical observation poisoned active owner", err)
		}
		for _, hard := range []error{ErrProviderEarningBoundaryMismatch, ErrProviderEarningBoundarySchema, ErrProviderEarningBoundaryUnprepared, context.Canceled} {
			if ProviderEarningBoundaryRetryable(ctx, errors.Join(err, hard)) {
				t.Fatal("timeout overrode hard/canceled cause", hard)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		expired, stop := context.WithDeadline(ctx, time.Unix(1, 0))
		stop()
		if ProviderEarningBoundaryRetryable(canceled, err) || ProviderEarningBoundaryRetryable(expired, err) || ProviderEarningBoundaryRetryable(nil, err) {
			t.Fatal("expired owner acquired continuation")
		}
	}
	for _, code := range []string{"42P01", "42703", "42883", "42501"} {
		cause := &pgconn.PgError{Code: code, Message: "synthetic incompatible schema"}
		joined := errors.Join(context.DeadlineExceeded, cause)
		_, err := requireProviderPayoutBoundarySchema(ctx, providerBoundaryErrorQuery{err: joined}, true)
		if !errors.Is(err, ErrProviderEarningBoundarySchema) || !errors.Is(err, cause) || ProviderEarningBoundaryRetryable(ctx, err) {
			t.Fatal("SQL schema error hidden by timeout", code, err)
		}
		if _, err := readProviderPayoutBoundary(ctx, providerBoundaryErrorQuery{err: joined}); !errors.Is(err, ErrProviderEarningBoundarySchema) {
			t.Fatal("binding read schema error became observation outage", err)
		}
	}
}

func TestProviderBoundaryPanicAndCancellationPreserveCause(t *testing.T) {
	capture := func(ctx context.Context, action func()) (err error) {
		defer recoverProviderBoundaryObservation(ctx, &err)
		action()
		return nil
	}
	cause := errors.New("synthetic database connection refusal")
	if err := capture(context.Background(), func() { panic(cause) }); !errors.Is(err, cause) || !ProviderEarningBoundaryRetryable(context.Background(), err) {
		t.Fatal("database panic lost retryable cause", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := capture(canceled, func() { panic(DbContextDoneError) }); !errors.Is(err, context.Canceled) || ProviderEarningBoundaryRetryable(canceled, err) {
		t.Fatal("database cancellation lost owner cancellation", err)
	}
	var propagated any
	func() {
		defer func() { propagated = recover() }()
		_ = capture(context.Background(), func() {
			var value *int
			_ = *value
		})
	}()
	if _, ok := propagated.(runtime.Error); !ok {
		t.Fatal("programming panic was mislabeled database outage", propagated)
	}
}

type providerBoundaryRestartInput struct {
	Pg, DbConfig, Policy []byte
	ExpectedDigest       string
	WantMismatch         bool
}

func TestProviderBoundaryRestartRetainsAuthority(t *testing.T) {
	if os.Getenv("URNETWORK_TEST_PROVIDER_BOUNDARY_CHILD") == "1" {
		var input providerBoundaryRestartInput
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1024*1024)).Decode(&input); err != nil {
			t.Fatal("invalid private child input")
		}
		defer Vault.PushSimpleResource(DefaultPgVaultResourceName, input.Pg)()
		defer Config.PushSimpleResource(DefaultPgConfigResourceName, input.DbConfig)()
		defer Config.PushSimpleResource("sn.yml", input.Policy)()
		if !strings.HasPrefix(Vault.RequireSimpleResource(DefaultPgVaultResourceName).RequireString("db"), "test_") {
			t.Fatal("child requires an owned test database")
		}
		PgReset()
		defer PgReset()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		policy, err := LoadProviderPayoutEarningPolicy(ctx)
		if input.WantMismatch {
			if !errors.Is(err, ErrProviderEarningBoundaryMismatch) {
				t.Fatal("new process accepted changed economic boundary", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		binding, err := RequireProviderPayoutBoundary(ctx, policy)
		if err != nil || binding == nil || binding.IdentitySha256 != input.ExpectedDigest {
			t.Fatal("new process did not retain original earning authority", err)
		}
		return
	}
	DefaultTestEnv().Run(t, func(t testing.TB) {
		data, policy := boundaryTestPolicy(t)
		binding, err := prepareProviderPayoutBoundary(context.Background(), policy, policy.ConfigSha256)
		if err != nil || binding == nil {
			t.Fatal(err)
		}
		pg := Vault.RequireSimpleResource(DefaultPgVaultResourceName)
		if !strings.HasPrefix(pg.RequireString("db"), "test_") {
			t.Fatal("restart fixture requires private database")
		}
		input := providerBoundaryRestartInput{Pg: pg.Bytes(), DbConfig: Config.RequireSimpleResource(DefaultPgConfigResourceName).Bytes(), Policy: data, ExpectedDigest: binding.IdentitySha256}
		for _, changed := range []bool{false, true} {
			input.WantMismatch = changed
			if changed {
				input.Policy = bytes.Replace(data, []byte(policy.CutoffUtc), []byte("2026-10-07T00:00:00Z"), 1)
			}
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderBoundaryRestartRetainsAuthority$", "-test.count=1")
			command.Env = append(os.Environ(), "URNETWORK_TEST_PROVIDER_BOUNDARY_CHILD=1")
			command.Stdin = bytes.NewReader(encoded)
			// Credentials travel only through the private pipe. Do not print child
			// diagnostics or input if an environmental connection failure occurs.
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			err = command.Run()
			cancel()
			if err != nil {
				t.Fatal("bounded private restart control failed", changed, err)
			}
		}
	})
}

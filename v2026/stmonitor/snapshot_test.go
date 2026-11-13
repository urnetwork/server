package stmonitor_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026/stmonitor"
	"github.com/urnetwork/server/v2026/stmonitor/testfixture"
)

func readFixture(t testing.TB, fixture *testfixture.Fixture) *stmonitor.Snapshot {
	t.Helper()
	value, err := stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	if err != nil {
		t.Fatal("actual read-only PostgreSQL census refused", err)
	}
	return value
}

func TestOperatorSnapshotReadsExactAccountScopeAndSupersededAttempt(t *testing.T) {
	fixture := testfixture.New(t)
	old := fixture.Now.Add(-time.Hour)
	fixture.Intent(t, 1, "uncertain", 2, old)
	fixture.Exec(t, `UPDATE st_transaction_attempt SET status='superseded' WHERE attempt=1`)
	fixture.Exec(t, `UPDATE st_transaction_intent SET deployment_id='synthetic-previous',deployment_key='964:0x9999999999999999999999999999999999999999'`)
	fixture.Intent(t, 2, "prepared", 0, fixture.Now)
	fixture.Exec(t, `UPDATE st_transaction_intent SET from_address=$1 WHERE nonce=2`, "0x"+strings.Repeat("8", 40))
	fixture.Intent(t, 3, "finalized", 1, fixture.Now)
	fixture.Intent(t, 4, "prepared", 0, fixture.Now)
	fixture.Exec(t, `UPDATE st_transaction_intent SET genesis_hash=$1 WHERE nonce=4`, "0x"+strings.Repeat("7", 64))
	fixture.Exec(t, `INSERT INTO st_publish VALUES($1,'10000000-0000-0000-0000-000000000001','pending',$2,$2)`, fixture.Source.DeploymentKey(), old)
	value := readFixture(t, fixture)
	if value.PendingIntents != 1 || value.SignedAttempts != 2 || value.UncertainIntents != 1 || value.ForeignDeploymentIntents != 1 || value.OldestIntent.Nonce != 1 || !value.OldestIntent.CreatedAt.Equal(old) || value.PendingPublications != 1 || !value.OldestPublication.CreatedAt.Equal(old) {
		t.Fatal("exact account census lost original or superseded evidence", value)
	}
	if value.Mirror == nil || value.Mirror.NextBlock != 101 || value.Epoch == nil || value.Epoch.CommitDeadline != 120 {
		t.Fatal("actual production projections were not observed", value)
	}
}

func TestOperatorSnapshotRefusesWritableAndElevatedRole(t *testing.T) {
	fixture := testfixture.New(t)
	fixture.Exec(t, "GRANT UPDATE ON st_publish TO "+pgx.Identifier{fixture.Source.User}.Sanitize())
	value, err := stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	var refusal *stmonitor.ReadError
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "identity" {
		t.Fatal("writable observer credential was admitted", value, err)
	}
	fixture.Exec(t, "REVOKE UPDATE ON st_publish FROM "+pgx.Identifier{fixture.Source.User}.Sanitize())
	if _, err := fixture.Admin.Exec(t.Context(), "ALTER ROLE "+pgx.Identifier{fixture.Source.User}.Sanitize()+" CREATEDB"); err != nil {
		t.Fatal(err)
	}
	value, err = stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "identity" {
		t.Fatal("elevated observer credential was admitted", value, err)
	}
}

func TestOperatorSnapshotCapacityPreservesUnknown(t *testing.T) {
	fixture := testfixture.New(t)
	fixture.Intent(t, 1, "signed", 17, fixture.Now)
	value, err := stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	var refusal *stmonitor.ReadError
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "capacity" {
		t.Fatal("attempt budget was treated as invalid or complete evidence", value, err)
	}
	fixture.Exec(t, `DELETE FROM st_transaction_attempt`)
	fixture.Exec(t, `DELETE FROM st_transaction_intent`)
	fixture.Exec(t, `INSERT INTO st_transaction_intent SELECT ('00000000-0000-0000-0000-'||lpad(n::text,12,'0'))::uuid,$1,$2,964,$3,$4,n,'prepared',NULL,0,$5,$5 FROM generate_series(1,257) n`, fixture.Source.DeploymentKey(), fixture.Source.DeploymentId, fixture.Source.GenesisHash, fixture.Source.Accounts[0], fixture.Now)
	value, err = stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "capacity" {
		t.Fatal("row census silently truncated pending work", value, err)
	}
}

func TestOperatorSnapshotRefusesIncompleteAttemptCensus(t *testing.T) {
	fixture := testfixture.New(t)
	fixture.Intent(t, 1, "signed", 2, fixture.Now)
	fixture.Exec(t, `DELETE FROM st_transaction_attempt WHERE attempt=1`)
	value, err := stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	var refusal *stmonitor.ReadError
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "invalid" {
		t.Fatal("missing attempt was accepted as complete signed census", value, err)
	}
	fixture.Exec(t, `UPDATE st_transaction_attempt SET attempt=1`)
	fixture.Exec(t, `UPDATE st_transaction_intent SET attempt_count=1,current_tx_hash=$1`, "0x"+strings.Repeat("a", 64))
	value, err = stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
	if value != nil || !errors.As(err, &refusal) || refusal.Code != "invalid" {
		t.Fatal("foreign current hash was accepted", value, err)
	}
}

// Positive SQL wait evidence forces cancellation inside the production query.
// The final connection census is a PostgreSQL fact, not a goroutine count.
func waitReader(t testing.TB, fixture *testfixture.Fixture, waiting bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		var count int
		query := `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name='sn-operator-monitor'`
		if waiting {
			query += ` AND wait_event_type='Lock'`
		}
		if err := fixture.Admin.QueryRow(ctx, query, fixture.Source.Database).Scan(&count); err != nil {
			t.Fatal("database connection barrier", err)
		}
		if waiting && count == 1 || !waiting && count == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("database connection barrier not reached", waiting, count)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestOperatorSnapshotCancellationJoinsDatabaseOwner(t *testing.T) {
	fixture := testfixture.New(t)
	lock, err := fixture.Database.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(t.Context(), `LOCK TABLE st_epoch IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		value, err := stmonitor.Read(ctx, fixture.Dsn, fixture.Source)
		if value != nil {
			err = errors.New("canceled read fabricated a snapshot")
		}
		done <- err
	}()
	waitReader(t, fixture, true)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("blocked query cancellation was not retained", err)
	}
	waitReader(t, fixture, false)
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if value := readFixture(t, fixture); value.Mirror == nil {
		t.Fatal("canceled owner damaged next independent read")
	}
}

func TestOperatorSnapshotUsesOneReadOnlyDatabaseSnapshot(t *testing.T) {
	fixture := testfixture.New(t)
	fixture.Intent(t, 1, "prepared", 0, fixture.Now)
	lock, err := fixture.Database.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(t.Context(), `LOCK TABLE st_epoch IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		value *stmonitor.Snapshot
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := stmonitor.Read(t.Context(), fixture.Dsn, fixture.Source)
		done <- result{value: value, err: err}
	}()
	waitReader(t, fixture, true)
	if _, err := lock.Exec(t.Context(), `UPDATE st_transaction_intent SET status='uncertain'`); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := <-done
	if read.err != nil || read.value == nil || read.value.UncertainIntents != 0 || read.value.OldestIntent.Status != "prepared" {
		t.Fatal("observer mixed database snapshots", read.value, read.err)
	}
	if value := readFixture(t, fixture); value.UncertainIntents != 1 {
		t.Fatal("next snapshot failed to observe committed change", value)
	}
}

func TestOperatorSnapshotConnectionAdmissionAndMissingDomains(t *testing.T) {
	fixture := testfixture.New(t)
	for _, mutate := range []func(*url.URL){
		func(value *url.URL) { value.Host = "database.example:5432" },
		func(value *url.URL) { value.Path = "/another_database" },
		func(value *url.URL) { value.User = url.User("another_user") },
		func(value *url.URL) {
			q := value.Query()
			q.Set("options", "-c statement_timeout=0")
			value.RawQuery = q.Encode()
		},
	} {
		value, _ := url.Parse(fixture.Dsn)
		mutate(value)
		if err := stmonitor.ValidateConnection(value.String(), fixture.Source); err == nil {
			t.Fatal("unapproved connection route or session options admitted")
		}
	}
	fixture.Exec(t, `DELETE FROM st_chain_sync`)
	fixture.Exec(t, `DELETE FROM st_epoch`)
	value := readFixture(t, fixture)
	if value.Mirror != nil || value.Epoch != nil || value.PendingIntents != 0 {
		t.Fatal("missing domain fabricated mirror authority", value)
	}
	if value, err := stmonitor.Read(nil, fixture.Dsn, fixture.Source); err == nil || value != nil {
		t.Fatal("nil read context admitted")
	}
}

func TestOperatorSnapshotRetainedValidationRejectsAmbiguity(t *testing.T) {
	fixture := testfixture.New(t)
	fixture.Intent(t, 1, "prepared", 0, fixture.Now)
	original := readFixture(t, fixture)
	for _, mutate := range []func(*stmonitor.Snapshot){
		func(value *stmonitor.Snapshot) { value.PendingIntents = 0 },
		func(value *stmonitor.Snapshot) { value.Source.GenesisHash = "" },
		func(value *stmonitor.Snapshot) { value.SignedAttempts = 4097 },
		func(value *stmonitor.Snapshot) { value.CensusHash = fmt.Sprint("sha256:", strings.Repeat("z", 64)) },
	} {
		value := *original
		mutate(&value)
		if err := value.Validate(); err == nil {
			t.Fatal("ambiguous retained snapshot admitted")
		}
	}
}

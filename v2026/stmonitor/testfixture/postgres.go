// Package testfixture owns isolated synthetic PostgreSQL databases for the
// operator producer/consumer qualification. It is imported only by test files.
package testfixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026/stmonitor"
)

type Fixture struct {
	Admin    *pgx.Conn
	Database *pgx.Conn
	Source   stmonitor.Source
	Dsn      string
	Now      time.Time
}

// The caller supplies an explicitly local disposable PostgreSQL administration
// URL. Every root owns a fresh database/role; existing application DBs are unused.
func New(t testing.TB) *Fixture {
	t.Helper()
	value, err := url.Parse(os.Getenv("STMONITOR_TEST_ADMIN_URL"))
	if err != nil || value == nil || value.Hostname() == "" || net.ParseIP(value.Hostname()) == nil || !net.ParseIP(value.Hostname()).IsLoopback() || value.Port() == "" {
		t.Fatal("STMONITOR_TEST_ADMIN_URL must name an explicitly disposable loopback PostgreSQL fixture")
	}
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, value.String())
	if err != nil {
		t.Fatal("connect local fixture", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := admin.Close(cleanup); err != nil {
			t.Errorf("fixture admin close: %v", err)
		}
	})
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random)
	source := stmonitor.Source{Database: "operator_fixture_" + suffix, User: "observer_" + suffix, DeploymentId: "synthetic-deployment", ChainId: 964, GenesisHash: "0x" + strings.Repeat("1", 64), Coordinator: "0x" + strings.Repeat("2", 40), OperatorId: 1, Accounts: []string{"0x" + strings.Repeat("3", 40)}}
	role := pgx.Identifier{source.User}.Sanitize()
	database := pgx.Identifier{source.Database}.Sanitize()

	fixture := &Fixture{Admin: admin, Source: source, Now: time.Now().UTC().Truncate(time.Second)}
	roleCreated, databaseCreated := false, false
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if fixture.Database != nil {
			if err := fixture.Database.Close(cleanup); err != nil {
				t.Errorf("fixture database close: %v", err)
			}
		}
		if databaseCreated {
			if _, err := admin.Exec(cleanup, "DROP DATABASE "+database+" WITH (FORCE)"); err != nil {
				t.Errorf("fixture database cleanup: %v", err)
			}
		}
		if roleCreated {
			if _, err := admin.Exec(cleanup, "DROP ROLE "+role); err != nil {
				t.Errorf("fixture role cleanup: %v", err)
			}
		}
	})
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'synthetic-fixture-only'"); err != nil {
		t.Fatal(err)
	}
	roleCreated = true
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	databaseCreated = true
	value.Path = "/" + source.Database
	connection, err := pgx.Connect(ctx, value.String())
	if err != nil {
		t.Fatal(err)
	}
	fixture.Database = connection
	for _, statement := range []string{
		`CREATE TABLE st_chain_sync(deployment_key text NOT NULL,singleton_id int NOT NULL,high_water_block bigint NOT NULL,block_hash text NOT NULL,update_time timestamp NOT NULL,PRIMARY KEY(deployment_key,singleton_id))`,
		`CREATE TABLE st_epoch(deployment_key text NOT NULL,epoch bigint NOT NULL,status varchar(16) NOT NULL,commit_deadline_block bigint NOT NULL,finalize_block bigint NOT NULL,PRIMARY KEY(deployment_key,epoch))`,
		`CREATE TABLE st_transaction_intent(intent_id uuid PRIMARY KEY,deployment_key text NOT NULL,deployment_id varchar(128) NOT NULL,chain_id bigint NOT NULL,genesis_hash varchar(66) NOT NULL,from_address varchar(42) NOT NULL,nonce bigint NOT NULL,status varchar(16) NOT NULL,current_tx_hash varchar(80),attempt_count int NOT NULL,create_time timestamp NOT NULL,update_time timestamp NOT NULL)`,
		`CREATE TABLE st_transaction_attempt(intent_id uuid NOT NULL,attempt int NOT NULL,tx_hash varchar(80) NOT NULL,status varchar(16) NOT NULL,PRIMARY KEY(intent_id,attempt))`,
		`CREATE TABLE st_publish(deployment_key text NOT NULL,publish_id uuid PRIMARY KEY,status varchar(16) NOT NULL,create_time timestamp NOT NULL,update_time timestamp NOT NULL)`,
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON st_chain_sync,st_epoch,st_transaction_intent,st_transaction_attempt,st_publish TO " + role,
	} {
		if _, err := connection.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Exec(t, `INSERT INTO st_chain_sync VALUES($1,1,101,$2,$3)`, source.DeploymentKey(), "0x"+strings.Repeat("4", 64), fixture.Now)
	fixture.Exec(t, `INSERT INTO st_epoch VALUES($1,7,'closed',120,150)`, source.DeploymentKey())
	value.User = url.UserPassword(source.User, "synthetic-fixture-only")
	fixture.Dsn = value.String()
	return fixture
}

func (self *Fixture) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := self.Database.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal("fixture statement", err)
	}
}

// Identical semantic source IDs recur only inside separately owned databases.
func (self *Fixture) Intent(t testing.TB, number int, status string, attempts int, created time.Time) {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", number)
	var current *string
	if attempts > 0 {
		hash := fmt.Sprintf("0x%064x", number*100+attempts)
		current = &hash
	}
	self.Exec(t, `INSERT INTO st_transaction_intent VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`, id, self.Source.DeploymentKey(), self.Source.DeploymentId, int64(self.Source.ChainId), self.Source.GenesisHash, self.Source.Accounts[0], number, status, current, attempts, created)
	for attempt := 1; attempt <= attempts; attempt++ {
		self.Exec(t, `INSERT INTO st_transaction_attempt VALUES($1,$2,$3,'signed')`, id, attempt, fmt.Sprintf("0x%064x", number*100+attempt))
	}
}

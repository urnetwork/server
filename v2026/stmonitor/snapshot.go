// Package stmonitor reads the existing operator journal without loading the
// operator configuration, acquiring a nonce owner, or receiving signing bytes.
package stmonitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const MaxRows = 256
const ReadTimeout = 300 * time.Second
const ReadAttemptTimeout = 60 * time.Second

// Source comes from the independent operations census. Accounts include every
// approved nonce owner, including owners with pending work from an older deployment.
// OperatorId is a declared role; the legacy tables cannot prove that declaration.
type Source struct {
	Database     string   `json:"database"`
	User         string   `json:"user"`
	DeploymentId string   `json:"deployment_id"`
	ChainId      uint64   `json:"chain_id"`
	GenesisHash  string   `json:"genesis_hash"`
	Coordinator  string   `json:"coordinator"`
	OperatorId   uint64   `json:"operator_id"`
	Accounts     []string `json:"accounts"`
}

func (self Source) Validate() error {
	if self.Database == "" || len(self.Database) > 63 || self.User == "" || len(self.User) > 63 || self.DeploymentId == "" || len(self.DeploymentId) > 128 || self.ChainId == 0 || self.ChainId > 1<<63-1 || self.OperatorId == 0 || !hexValue(self.GenesisHash, 32) || !hexValue(self.Coordinator, 20) || len(self.Accounts) == 0 || len(self.Accounts) > 4 {
		return errors.New("operator monitor source is incomplete")
	}
	for i, account := range self.Accounts {
		if !hexValue(account, 20) || i > 0 && self.Accounts[i-1] >= account {
			return errors.New("operator monitor accounts must be canonical sorted and unique")
		}
	}
	return nil
}

func hexValue(value string, size int) bool {
	if len(value) != 2+2*size || !strings.HasPrefix(value, "0x") || value == "0x"+strings.Repeat("0", 2*size) {
		return false
	}
	for _, c := range value[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (self Source) Equal(other Source) bool {
	return self.Database == other.Database && self.User == other.User && self.DeploymentId == other.DeploymentId && self.ChainId == other.ChainId && self.GenesisHash == other.GenesisHash && self.Coordinator == other.Coordinator && self.OperatorId == other.OperatorId && slices.Equal(self.Accounts, other.Accounts)
}

func (self Source) DeploymentKey() string {
	return strconv.FormatUint(self.ChainId, 10) + ":" + self.Coordinator
}

// Times describe retained rows, not the observer's successful query. A zero
// pending census is known only for this explicit account/deployment scope.
type Pending struct {
	Id              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Status          string    `json:"status"`
	Account         string    `json:"account,omitempty"`
	Nonce           uint64    `json:"nonce,omitempty"`
	TransactionHash string    `json:"transaction_hash,omitempty"`
}

type Mirror struct {
	NextBlock uint64    `json:"next_block"`
	BlockHash string    `json:"block_hash"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Epoch struct {
	Number         uint64 `json:"number"`
	Status         string `json:"status"`
	CommitDeadline uint64 `json:"commit_deadline"`
	FinalizeBlock  uint64 `json:"finalize_block"`
}

// This exact projection is a local database assertion. No field proves native
// finality, the mirror's genesis, account authority, or complete liabilities.
type Snapshot struct {
	Source                   Source    `json:"source"`
	DatabaseAt               time.Time `json:"database_at"`
	Mirror                   *Mirror   `json:"mirror,omitempty"`
	Epoch                    *Epoch    `json:"epoch,omitempty"`
	PendingIntents           uint64    `json:"pending_intents"`
	SignedAttempts           uint64    `json:"signed_attempts"`
	UncertainIntents         uint64    `json:"uncertain_intents"`
	FailedIntents            uint64    `json:"failed_intents"`
	ForeignDeploymentIntents uint64    `json:"foreign_deployment_intents"`
	OldestIntent             *Pending  `json:"oldest_intent,omitempty"`
	PendingPublications      uint64    `json:"pending_publications"`
	OldestPublication        *Pending  `json:"oldest_publication,omitempty"`
	LatestPublication        *Pending  `json:"latest_publication,omitempty"`
	CensusHash               string    `json:"census_hash"`
}

// A closed classification excludes SQL, credentials, and arbitrary remote text.
type ReadError struct {
	Code  string
	cause error
}

func (self *ReadError) Error() string     { return "operator database observation: " + self.Code }
func (self *ReadError) Unwrap() error     { return self.cause }
func refuse(code string, err error) error { return &ReadError{Code: code, cause: err} }

// The explicit URI cannot inherit another host/database/user or select failover.
// TLS is mandatory off loopback; verify-full is the sole remote TLS mode.
func connectionConfig(dsn string, source Source) (*pgx.ConnConfig, error) {
	if err := source.Validate(); err != nil {
		return nil, refuse("identity", err)
	}
	value, err := url.Parse(dsn)
	if err != nil || len(dsn) > 8192 || (value.Scheme != "postgres" && value.Scheme != "postgresql") || value.User == nil || value.User.Username() != source.User || value.Hostname() == "" || strings.Contains(value.Host, ",") || value.Path != "/"+source.Database || value.Fragment != "" || value.Port() == "" {
		return nil, refuse("identity", nil)
	}
	if password, ok := value.User.Password(); !ok || password == "" {
		return nil, refuse("identity", nil)
	}
	for key, values := range value.Query() {
		if len(values) != 1 || key != "sslmode" && key != "sslrootcert" {
			return nil, refuse("identity", nil)
		}
	}
	mode := value.Query().Get("sslmode")
	ip := net.ParseIP(value.Hostname())
	if mode != "verify-full" && (mode != "disable" || ip == nil || !ip.IsLoopback()) {
		return nil, refuse("identity", nil)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, refuse("identity", err)
	}
	// Remove environment-derived session options. All execution settings below
	// are fixed by this owner, never supplied by the database or query input.
	cfg.RuntimeParams = map[string]string{"application_name": "sn-operator-monitor", "timezone": "UTC"}
	cfg.ConnectTimeout = ReadAttemptTimeout
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.Fallbacks = nil
	return cfg, nil
}

// ValidateConnection checks the independently named endpoint without connecting.
func ValidateConnection(dsn string, source Source) error {
	_, err := connectionConfig(dsn, source)
	return err
}

// Each repeatable read has one five-minute operation budget and one-minute
// attempts. A failed attempt is fully joined before another connection starts.
// The original source remains fixed, and completed refusal never inherits a
// prior transport failure's retry classification.
func Read(ctx context.Context, dsn string, source Source) (*Snapshot, error) {
	if ctx == nil {
		return nil, refuse("invalid", nil)
	}
	source.Accounts = append([]string(nil), source.Accounts...)
	cfg, err := connectionConfig(dsn, source)
	if err != nil {
		return nil, err
	}
	return readWithBudget(ctx, cfg, source)
}

// This owner keeps every statement, row iterator, rollback and physical close
// joined. Only a completed read-only snapshot may leave with a nil error.
func readAttempt(ctx context.Context, cfg *pgx.ConnConfig, source Source, hooks readObservationHooks) (value *Snapshot, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, ReadAttemptTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg.Copy())
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	var tx pgx.Tx
	defer func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelCleanup()
		// A fatal backend message already closes pgx. Issuing another command
		// on that closed connection would fabricate a second local failure.
		if tx != nil && !conn.IsClosed() {
			err := tx.Rollback(cleanup)
			if !errors.Is(err, pgx.ErrTxClosed) {
				resultErr = errors.Join(resultErr, err)
			}
		}
		resultErr = errors.Join(resultErr, conn.Close(cleanup))
		// pgx can begin a separately bounded 15s async close after query cancellation.
		// Close may already report closed, so explicitly join its actual cleanup.
		<-conn.PgConn().CleanupDone()
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			value = nil
		}
	}()
	tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	for _, statement := range []string{"SET LOCAL statement_timeout = '60000ms'", "SET LOCAL lock_timeout = '60000ms'", "SET LOCAL idle_in_transaction_session_timeout = '60000ms'"} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return nil, refuse("unavailable", err)
		}
	}
	if hooks.afterBegin != nil {
		if err := hooks.afterBegin(ctx, conn, tx); err != nil {
			return nil, refuse("unavailable", err)
		}
	}
	value, err = readSnapshot(ctx, tx, source)
	if err != nil {
		return nil, err
	}
	if err := tx.Rollback(ctx); err != nil {
		return nil, refuse("unavailable", err)
	}
	return value, nil
}

// The role must be dedicated to observation: no memberships, elevated role
// flags, or write privilege on any application table. Deployment must also
// restrict its column grants and credentials; this is not a hostile-DB proof.
const admissionSql = `SELECT current_database(), current_user,
 current_setting('transaction_read_only'), current_setting('transaction_isolation'),
 transaction_timestamp(),
 EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)),
 EXISTS (SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user)),
 EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
 AND c.relkind IN ('r','p','v','m','f') AND
 (has_table_privilege(c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR
 has_any_column_privilege(c.oid,'INSERT,UPDATE,REFERENCES')))`

// No calldata or raw signed transaction is read. The bounded attempt census
// validates continuity, but does not cryptographically reauthenticate the bytes.
const intentsSql = `SELECT intent_id::text, deployment_key, deployment_id, from_address, nonce, status,
 current_tx_hash, attempt_count, create_time, update_time
 FROM public.st_transaction_intent WHERE chain_id=$1 AND genesis_hash=$2
 AND from_address=ANY($3) AND status NOT IN ('finalized','reverted','canceled','superseded')
 ORDER BY create_time,intent_id LIMIT 257`
const attemptsSql = `SELECT attempt,tx_hash,status FROM public.st_transaction_attempt
 WHERE intent_id=$1 ORDER BY attempt LIMIT 17`
const pendingPublicationsSql = `SELECT publish_id::text,status,create_time,update_time FROM public.st_publish
 WHERE deployment_key=$1 AND status='pending' ORDER BY create_time,publish_id LIMIT 257`

func readSnapshot(ctx context.Context, tx pgx.Tx, source Source) (*Snapshot, error) {
	value := &Snapshot{Source: source}
	var database, user, readOnly, isolation string
	var elevated, member, writable bool
	if err := tx.QueryRow(ctx, admissionSql).Scan(&database, &user, &readOnly, &isolation, &value.DatabaseAt, &elevated, &member, &writable); err != nil {
		return nil, refuse("unavailable", err)
	}
	if database != source.Database || user != source.User || readOnly != "on" || isolation != "repeatable read" || elevated || member || writable {
		return nil, refuse("identity", nil)
	}
	var next int64
	var mirror Mirror
	err := tx.QueryRow(ctx, `SELECT high_water_block,block_hash,update_time FROM public.st_chain_sync WHERE deployment_key=$1 AND singleton_id=1`, source.DeploymentKey()).Scan(&next, &mirror.BlockHash, &mirror.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("unavailable", err)
	}
	if err == nil {
		if next < 0 || next == 0 && mirror.BlockHash != "" || next > 0 && !hexValue(mirror.BlockHash, 32) || mirror.UpdatedAt.IsZero() {
			return nil, refuse("invalid", nil)
		}
		mirror.NextBlock = uint64(next)
		value.Mirror = &mirror
	}
	var epoch, deadline, finalize int64
	var status string
	err = tx.QueryRow(ctx, `SELECT epoch,status,commit_deadline_block,finalize_block FROM public.st_epoch WHERE deployment_key=$1 ORDER BY epoch DESC LIMIT 1`, source.DeploymentKey()).Scan(&epoch, &status, &deadline, &finalize)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("unavailable", err)
	}
	if err == nil {
		if epoch < 0 || deadline < 0 || finalize < 0 || !slices.Contains([]string{"open", "closed", "committed", "finalized"}, status) {
			return nil, refuse("invalid", nil)
		}
		value.Epoch = &Epoch{Number: uint64(epoch), Status: status, CommitDeadline: uint64(deadline), FinalizeBlock: uint64(finalize)}
	}
	type intent struct {
		Pending
		Deployment   string
		DeploymentId string
		Attempts     int64
	}
	var intents []intent
	rows, err := tx.Query(ctx, intentsSql, int64(source.ChainId), source.GenesisHash, source.Accounts)
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	for rows.Next() {
		var row intent
		var nonce int64
		var hash *string
		if err := rows.Scan(&row.Id, &row.Deployment, &row.DeploymentId, &row.Account, &nonce, &row.Status, &hash, &row.Attempts, &row.CreatedAt, &row.UpdatedAt); err != nil {
			rows.Close()
			return nil, refuse("invalid", err)
		}
		if nonce < 0 || row.Attempts < 0 || !slices.Contains(source.Accounts, row.Account) || !slices.Contains([]string{"prepared", "signed", "broadcast", "mined", "uncertain", "failed", "invalid"}, row.Status) || row.CreatedAt.IsZero() || row.UpdatedAt.Before(row.CreatedAt) {
			rows.Close()
			return nil, refuse("invalid", nil)
		}
		if row.Attempts > 16 {
			rows.Close()
			return nil, refuse("capacity", nil)
		}
		row.Nonce = uint64(nonce)
		if hash != nil {
			row.TransactionHash = *hash
			if !hexValue(*hash, 32) {
				rows.Close()
				return nil, refuse("invalid", nil)
			}
		}
		intents = append(intents, row)
		if len(intents) > MaxRows {
			rows.Close()
			return nil, refuse("capacity", nil)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	census := sha256.New()
	encode := json.NewEncoder(census)
	for _, row := range intents {
		if row.Deployment != source.DeploymentKey() || row.DeploymentId != source.DeploymentId {
			value.ForeignDeploymentIntents++
		}
		value.PendingIntents++
		if value.OldestIntent == nil {
			pending := row.Pending
			value.OldestIntent = &pending
		}
		switch row.Status {
		case "uncertain":
			value.UncertainIntents++
		case "failed", "invalid":
			value.FailedIntents++
		}
		_ = encode.Encode(row)
		attempts, err := tx.Query(ctx, attemptsSql, row.Id)
		if err != nil {
			return nil, refuse("unavailable", err)
		}
		count := int64(0)
		current := false
		for attempts.Next() {
			var number int64
			var hash, status string
			if err := attempts.Scan(&number, &hash, &status); err != nil {
				attempts.Close()
				return nil, refuse("invalid", err)
			}
			count++
			if count > 16 {
				attempts.Close()
				return nil, refuse("capacity", nil)
			}
			if number != count || !hexValue(hash, 32) || !slices.Contains([]string{"signed", "broadcast", "mined", "finalized", "failed", "uncertain", "replaced", "reverted", "invalid", "canceled", "superseded"}, status) {
				attempts.Close()
				return nil, refuse("invalid", nil)
			}
			current = current || hash == row.TransactionHash
			_ = encode.Encode(struct {
				Number int64
				Hash   string
				Status string
			}{Number: number, Hash: hash, Status: status})
		}
		err = attempts.Err()
		attempts.Close()
		if err != nil {
			return nil, refuse("unavailable", err)
		}
		if count != row.Attempts || count == 0 && row.TransactionHash != "" || count > 0 && !current {
			return nil, refuse("invalid", nil)
		}
		value.SignedAttempts += uint64(count)
	}
	publications, err := tx.Query(ctx, pendingPublicationsSql, source.DeploymentKey())
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	for publications.Next() {
		var row Pending
		if err := publications.Scan(&row.Id, &row.Status, &row.CreatedAt, &row.UpdatedAt); err != nil {
			publications.Close()
			return nil, refuse("invalid", err)
		}
		if row.Status != "pending" || row.CreatedAt.IsZero() || row.UpdatedAt.Before(row.CreatedAt) {
			publications.Close()
			return nil, refuse("invalid", nil)
		}
		value.PendingPublications++
		if value.PendingPublications > MaxRows {
			publications.Close()
			return nil, refuse("capacity", nil)
		}
		if value.OldestPublication == nil {
			pending := row
			value.OldestPublication = &pending
		}
		_ = encode.Encode(row)
	}
	err = publications.Err()
	publications.Close()
	if err != nil {
		return nil, refuse("unavailable", err)
	}
	var latest Pending
	err = tx.QueryRow(ctx, `SELECT publish_id::text,status,create_time,update_time FROM public.st_publish WHERE deployment_key=$1 ORDER BY create_time DESC,publish_id DESC LIMIT 1`, source.DeploymentKey()).Scan(&latest.Id, &latest.Status, &latest.CreatedAt, &latest.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("unavailable", err)
	}
	if err == nil {
		if !slices.Contains([]string{"pending", "confirmed", "failed", "skipped"}, latest.Status) || latest.CreatedAt.IsZero() || latest.UpdatedAt.Before(latest.CreatedAt) {
			return nil, refuse("invalid", nil)
		}
		value.LatestPublication = &latest
	}
	value.CensusHash = fmt.Sprintf("sha256:%x", census.Sum(nil))
	if err := value.Validate(); err != nil {
		return nil, refuse("invalid", err)
	}
	return value, nil
}

// Retained snapshots are checked again after checkpoint decoding. These bounds
// do not convert a database assertion into authenticated chain evidence.
func (self Snapshot) Validate() error {
	if err := self.Source.Validate(); err != nil {
		return err
	}
	if self.DatabaseAt.IsZero() || self.PendingIntents > MaxRows || self.PendingPublications > MaxRows || self.SignedAttempts > 16*MaxRows || self.UncertainIntents+self.FailedIntents > self.PendingIntents || self.ForeignDeploymentIntents > self.PendingIntents || (self.PendingIntents == 0) != (self.OldestIntent == nil) || (self.PendingPublications == 0) != (self.OldestPublication == nil) || len(self.CensusHash) != 71 || !hexValue("0x"+strings.TrimPrefix(self.CensusHash, "sha256:"), 32) {
		return errors.New("operator snapshot census is incomplete")
	}
	if self.Mirror != nil && (self.Mirror.UpdatedAt.IsZero() || self.Mirror.NextBlock == 0 && self.Mirror.BlockHash != "" || self.Mirror.NextBlock > 0 && !hexValue(self.Mirror.BlockHash, 32)) {
		return errors.New("operator mirror identity is incomplete")
	}

	if self.Epoch != nil && !slices.Contains([]string{"open", "closed", "committed", "finalized"}, self.Epoch.Status) {
		return errors.New("operator epoch status is unknown")
	}
	if self.OldestIntent != nil {
		value := self.OldestIntent
		if !slices.Contains(self.Source.Accounts, value.Account) || !slices.Contains([]string{"prepared", "signed", "broadcast", "mined", "uncertain", "failed", "invalid"}, value.Status) || value.TransactionHash != "" && !hexValue(value.TransactionHash, 32) {
			return errors.New("operator retained intent scope differs")
		}
	}
	if self.OldestPublication != nil && (self.OldestPublication.Status != "pending" || self.OldestPublication.Account != "" || self.OldestPublication.TransactionHash != "" || self.OldestPublication.Nonce != 0) {
		return errors.New("operator pending publication differs")
	}
	if self.LatestPublication != nil && (!slices.Contains([]string{"pending", "confirmed", "failed", "skipped"}, self.LatestPublication.Status) || self.LatestPublication.Account != "" || self.LatestPublication.TransactionHash != "" || self.LatestPublication.Nonce != 0) {
		return errors.New("operator latest publication differs")
	}
	for _, row := range []*Pending{self.OldestIntent, self.OldestPublication, self.LatestPublication} {
		if row != nil && (len(row.Id) != 36 || row.CreatedAt.IsZero() || row.UpdatedAt.Before(row.CreatedAt)) {
			return errors.New("operator pending evidence is incomplete")
		}
	}
	return nil
}

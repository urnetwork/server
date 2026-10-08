// Business ownership and its transaction share one direct PostgreSQL session.
// Admission waits outside a transaction; a disconnected owner cannot leave a
// still-running transaction on another backend. Every conflicting writer must
// use the same canonical resource domain and identity before taking row locks.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const PgOwnershipAdmissionTimeout = 5 * time.Second
const pgOwnershipPollPeriod = 25 * time.Millisecond
const pgOwnershipQueryLimit = 64

// Comparable opaque keys use the two-integer PostgreSQL advisory namespace.
// Hash collisions conservatively serialize additional resources. They never
// allow two writers of the same canonical identity to enter together.
type PgOwnershipKey struct {
	first  int32
	second int32
}

// The domain names the actual shared resource, independently of the calling
// target or service. For example, all account-balance writers use the domain
// "account_balance" and the row's network id, including paid/provided writers.
func NewPgOwnershipKey(domain string, id Id) PgOwnershipKey {
	if domain == "" || strings.ContainsRune(domain, '\x00') || id == (Id{}) {
		panic(errors.New("invalid database ownership identity"))
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("urnetwork:business-owner:v1\x00"))
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(id[:])
	sum := hash.Sum(nil)
	return PgOwnershipKey{first: int32(binary.BigEndian.Uint32(sum[:4])), second: int32(binary.BigEndian.Uint32(sum[4:8]))}
}

type PgOwnershipEventKind uint8

const (
	PgOwnershipWaiting PgOwnershipEventKind = iota
	PgOwnershipAdmitted
	PgOwnershipReleased
	PgOwnershipRefused
	PgOwnershipUncertain
)

// Optional per-call observation carries opaque keys and the actual backend.
// Observers must bound their own work. Released follows actual cleanup; another
// session may have admitted before its callback runs, so event intervals alone
// cannot establish global exclusion. Unknown cleanup emits Uncertain instead.
type PgOwnershipEvent struct {
	Kind              PgOwnershipEventKind
	BackendPid        uint32
	Keys              []PgOwnershipKey
	TransactionScoped bool
}

type PgOwnershipObservation struct {
	Observe func(PgOwnershipEvent)
}

// The test observer follows the real entry point through model/task adapters.
// Production contexts carry no hook. It may hold admission to force a causal
// schedule; the caller remains responsible for a finite context and release.
type pgOwnershipObservationKey struct{}

func Testing_WithPgOwnershipObservation(ctx context.Context, observe func(PgOwnershipEvent)) context.Context {
	return context.WithValue(ctx, pgOwnershipObservationKey{}, &PgOwnershipObservation{Observe: observe})
}

// This option is private: only OwnedTx can donate an admitted connection to
// the ordinary transaction engine. Its sole caller owns all lifecycle fields.
type pgOwnedConnection struct {
	conn        PgConn
	keys        []PgOwnershipKey
	observation *PgOwnershipObservation
	backendPid  uint32
	admitted    bool
	entered     bool
	released    bool
	discard     bool
}

// Admit the complete key set before BEGIN, then reuse the normal transaction
// wrapper, commit observations, detached cleanup and post-commit publications.
// No callback/commit/connection error reruns this transaction. A known busy
// admission releases its partial keys before waiting; it has executed no
// business transaction. Key-count work is proportional to the complete set,
// with bounded SQL chunks and a finite admission context, never a fallback.
func OwnedTx(ctx context.Context, keys []PgOwnershipKey, callback func(PgTx), options ...any) {
	checkPostgresAllowed(ctx)
	ownedTxWithResource(ctx, keys, requirePgOwnershipResource(), AcquireMaintenanceDbConn, callback, options...)
}

// The production caller supplies only the explicit direct resource/acquirer.
// A private transport seam exercises actual pgx replies without global pools.
func ownedTxWithResource(ctx context.Context, keys []PgOwnershipKey, resource pgOwnershipResource,
	acquire func(context.Context) (PgConn, error), callback func(PgTx), options ...any) {
	keys = normalizePgOwnershipKeys(keys)
	if len(keys) == 0 {
		panic(errors.New("database ownership requires at least one key"))
	}
	observation, _ := ctx.Value(pgOwnershipObservationKey{}).(*PgOwnershipObservation)
	var timing *DbTiming
	for _, option := range options {
		switch value := option.(type) {
		case *PgOwnershipObservation:
			observation = value
		case *DbTiming:
			timing = value
		}
	}
	admissionCtx, cancel := context.WithTimeout(ctx, PgOwnershipAdmissionTimeout)
	defer cancel()
	for {
		started := timing.start()
		conn, err := acquire(admissionCtx)
		timing.finish(DbTimingAcquire, started)
		if err != nil {
			if admissionCtx.Err() != nil {
				err = dbContextDoneCause(admissionCtx, err)
			}
			panic(err)
		}
		owner := &pgOwnedConnection{conn: conn, keys: keys, observation: observation, backendPid: conn.Conn().PgConn().PID(), discard: true}
		admitted := false
		func() {
			defer owner.release(ctx)
			Raise(resource.validate(conn))
			var err error
			admitted, err = tryPgOwnershipKeys(admissionCtx, conn, keys, false, owner.backendPid)
			if err != nil {
				if admissionCtx.Err() != nil {
					err = dbContextDoneCause(admissionCtx, err)
				}
				panic(err)
			}
			owner.discard = false
			if !admitted {
				// Return pool capacity before observing or waiting. A crowded
				// resource cannot retain every slot needed by unrelated work.
				Raise(owner.release(ctx))
				owner.observe(PgOwnershipWaiting)
				return
			}
			owner.admitted = true
			owner.observe(PgOwnershipAdmitted)
			ownedOptions := append(append([]any{}, options...), owner, OptNoRetry())
			Tx(ctx, func(tx PgTx) {
				var backendPid uint32
				Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPid))
				if backendPid != owner.backendPid {
					owner.discard = true
					panic(errors.New("database ownership backend changed before transaction"))
				}
				transaction := tx.(*postCommitPgTx)
				transaction.ownershipAllowed = false
				transaction.ownershipKeys = keys
				callback(tx)
			}, ownedOptions...)
		}()
		if admitted {
			return
		}
		select {
		case <-admissionCtx.Done():
			panic(admissionCtx.Err())
		case <-time.After(pgOwnershipPollPeriod):
		}
	}
}

func normalizePgOwnershipKeys(keys []PgOwnershipKey) []PgOwnershipKey {
	keys = slices.Clone(keys)
	slices.SortFunc(keys, comparePgOwnershipKeys)
	return slices.Compact(keys)
}

func comparePgOwnershipKeys(a, b PgOwnershipKey) int {
	if a.first < b.first || a.first == b.first && a.second < b.second {
		return -1
	}
	if a == b {
		return 0
	}
	return 1
}

// A session owner never silently inherits the ordinary transaction-pool route.
// Validate both the explicit authority and the checked-out pool's immutable
// connection configuration, including an already-open pool from an older scope.
type pgOwnershipResource struct {
	host     string
	port     uint16
	database string
	user     string
}

func requirePgOwnershipResource() pgOwnershipResource {
	resource, err := Vault.SimpleResource(MaintenancePgVaultResourceName)
	if err != nil {
		panic(errors.New("database ownership requires an explicit direct maintenance resource"))
	}
	values, err := resource.ParseE()
	if err != nil {
		panic(errors.New("invalid database ownership maintenance resource"))
	}
	authority, _ := values["authority"].(string)
	database, _ := values["db"].(string)
	user, _ := values["user"].(string)
	parsed, err := url.Parse("postgres://" + authority)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || database == "" || user == "" {
		panic(errors.New("invalid database ownership maintenance resource"))
	}
	port := uint64(5432)
	if parsed.Port() != "" {
		port, err = strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil || port == 0 {
			panic(errors.New("invalid database ownership maintenance resource"))
		}
	}
	return pgOwnershipResource{host: parsed.Hostname(), port: uint16(port), database: database, user: user}
}

func (self pgOwnershipResource) validate(conn PgConn) error {
	config := conn.Conn().Config()
	if !strings.EqualFold(config.Host, self.host) || config.Port != self.port || config.Database != self.database || config.User != self.user {
		return errors.New("database ownership checkout does not match its explicit maintenance resource")
	}
	return nil
}

// Both variants acquire the same key space. The transaction-scoped variant is
// for a caller that already owns BEGIN; a false result requires ending that
// attempt before any shared-resource row lock/write. Private intent locks may
// precede admission. Partial keys live until that transaction ends. The helper
// requires a read-committed server transaction with OptNoRetry and one complete
// key-set probe before any savepoint. A repeatable-read snapshot can predate a
// prior owner's commit even when its advisory key is now free. An admitted
// subset returns true without a new query or interval; extra keys fail closed.
func TryTxOwnership(ctx context.Context, tx PgTx, keys []PgOwnershipKey) (bool, error) {
	keys = normalizePgOwnershipKeys(keys)
	if len(keys) == 0 {
		return false, errors.New("database ownership requires at least one key")
	}
	if TxOwnsKeys(tx, keys) {
		return true, nil
	}
	owner, ok := tx.(*postCommitPgTx)
	if !ok || !owner.ownershipAllowed || owner.ownership != nil {
		return false, errors.New("transaction ownership requires a fresh read-committed no-retry server transaction")
	}
	observation, _ := ctx.Value(pgOwnershipObservationKey{}).(*PgOwnershipObservation)
	owner.ownership = &pgTransactionOwnership{keys: keys, observation: observation}
	admitted, err := tryPgOwnershipKeys(ctx, tx, keys, true, 0)
	if err != nil {
		return false, err
	}
	// Query on the same transaction route; a transaction pooler pins that
	// backend until the enclosing commit/rollback. Its startup PID may differ.
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&owner.ownership.backendPid); err != nil {
		return false, err
	}
	owner.ownership.admitted = admitted
	if admitted {
		owner.ownershipKeys = keys
		owner.ownership.observe(PgOwnershipAdmitted)
	} else {
		owner.ownership.observe(PgOwnershipRefused)
	}
	return admitted, nil
}

// Validate a subset of this live server transaction's complete admitted set.
// This is read-only and never acquires locks, expands authority or adds events.
// Raw/savepoint wrappers cannot borrow the outer owner's authority. Callers
// must establish transaction-scoped admission before any SQL savepoint that
// could otherwise roll its locks back before the enclosing transaction ends.
func TxOwnsKeys(tx PgTx, keys []PgOwnershipKey) bool {
	owner, ok := tx.(*postCommitPgTx)
	if !ok || len(keys) == 0 || len(owner.ownershipKeys) == 0 {
		return false
	}
	for _, key := range keys {
		if _, found := slices.BinarySearchFunc(owner.ownershipKeys, key, comparePgOwnershipKeys); !found {
			return false
		}
	}
	return true
}

// Transaction-scoped ownership ends at the real commit/rollback reply. An
// unknown reply emits uncertainty, never an invented successful release.
type pgTransactionOwnership struct {
	keys        []PgOwnershipKey
	observation *PgOwnershipObservation
	backendPid  uint32
	admitted    bool
	finished    bool
}

func (self *postCommitPgTx) Commit(ctx context.Context) (returnErr error) {
	returned := false
	defer func() { self.finishOwnership(returned, returnErr) }()
	returnErr = self.PgTx.Commit(ctx)
	returned = true
	return
}

func (self *postCommitPgTx) Rollback(ctx context.Context) (returnErr error) {
	returned := false
	defer func() { self.finishOwnership(returned, returnErr) }()
	returnErr = self.PgTx.Rollback(ctx)
	returned = true
	return
}

func (self *postCommitPgTx) finishOwnership(returned bool, err error) {
	self.ownershipKeys = nil
	ownership := self.ownership
	if ownership == nil || ownership.finished {
		return
	}
	ownership.finished = true
	kind := PgOwnershipUncertain
	if returned && (err == nil || errors.Is(err, pgx.ErrTxCommitRollback)) {
		kind = PgOwnershipReleased
	} else if returned && !self.PgTx.Conn().IsClosed() && self.PgTx.Conn().PgConn().TxStatus() == 'I' {
		kind = PgOwnershipReleased
	}
	// A refused partial probe has no admitted interval to close. Its partial
	// transaction keys still follow the actual transaction end above.
	if ownership.admitted || kind == PgOwnershipUncertain {
		func() {
			defer func() { _ = recover() }()
			ownership.observe(kind)
		}()
	}
}

func (self *pgTransactionOwnership) observe(kind PgOwnershipEventKind) {
	if self.observation != nil && self.observation.Observe != nil {
		self.observation.Observe(PgOwnershipEvent{Kind: kind, BackendPid: self.backendPid, Keys: slices.Clone(self.keys), TransactionScoped: true})
	}
}

// Nonblocking probes are explicitly bounded per statement. Every returned key
// comes from a supplied array; no advisory function runs on a queue scan or an
// expression whose LIMIT could be evaluated after acquiring additional locks.
func tryPgOwnershipKeys(ctx context.Context, query PgCanQuery, keys []PgOwnershipKey, transactional bool, backendPid uint32) (bool, error) {
	function := "pg_try_advisory_lock"
	if transactional {
		function = "pg_try_advisory_xact_lock"
	}
	for offset := 0; offset < len(keys); offset += pgOwnershipQueryLimit {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		page := keys[offset:min(offset+pgOwnershipQueryLimit, len(keys))]
		firsts, seconds := make([]int32, len(page)), make([]int32, len(page))
		for index, key := range page {
			firsts[index], seconds[index] = key.first, key.second
		}
		rows, err := query.Query(ctx, `SELECT pg_backend_pid(),`+function+`(owner.first,owner.second)
			FROM unnest($1::integer[],$2::integer[]) AS owner(first,second)`, firsts, seconds)
		if err != nil {
			return false, err
		}
		admitted, count := true, 0
		for rows.Next() {
			var acquired bool
			var observedBackendPid uint32
			if err := rows.Scan(&observedBackendPid, &acquired); err != nil {
				rows.Close()
				return false, err
			}
			if backendPid != 0 && observedBackendPid != backendPid {
				rows.Close()
				return false, errors.New("database ownership requires a direct backend session")
			}
			admitted = admitted && acquired
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		if count != len(page) {
			return false, errors.New("database ownership reply count mismatch")
		}
		if !admitted {
			return false, nil
		}
	}
	return true, nil
}

// Called exactly once by dbWithPool instead of acquiring a second connection.
// Cleanup precedes the transaction engine's optional post-commit publications.
func (self *pgOwnedConnection) run(ctx context.Context, callback func(PgConn), readOnly bool) {
	if self.entered || !self.admitted || self.released {
		panic(errors.New("database ownership connection cannot be reused"))
	}
	self.entered = true
	defer self.release(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok && isConnectionError(err) {
				self.discard = true
				if isDoneContextConnectionError(ctx, err) {
					panic(dbContextDoneCause(ctx, err))
				}
			}
			panic(recovered)
		}
	}()
	if !readOnly {
		RaisePgResult(self.conn.Exec(ctx, "SET default_transaction_read_only=off"))
	}
	callback(self.conn)
}

// A bad/uncertain unlock destroys the session instead of returning its locks to
// the pool. Cleanup never replaces the transaction's primary error or invents
// a failed business outcome after an acknowledged commit.
func (self *pgOwnedConnection) release(ctx context.Context) (returnErr error) {
	if self.released {
		return nil
	}
	self.released = true
	if !self.discard && !self.conn.Conn().IsClosed() && self.conn.Conn().PgConn().TxStatus() == 'I' {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PgCloseTimeout)
		_, err := self.conn.Exec(cleanupCtx, `SELECT pg_advisory_unlock_all()`)
		cancel()
		self.discard = err != nil
		returnErr = err
	} else {
		self.discard = true
	}
	if self.discard {
		discardPgConnection(ctx, self.conn)
	} else {
		self.conn.Release()
	}
	if self.admitted {
		// Observation is secondary once the business outcome is decided. A
		// faulty observer must not turn an acknowledged commit into an error.
		func() {
			defer func() { _ = recover() }()
			kind := PgOwnershipReleased
			if self.discard {
				kind = PgOwnershipUncertain
			}
			self.observe(kind)
		}()
	}
	return
}

func (self *pgOwnedConnection) observe(kind PgOwnershipEventKind) {
	if self.observation != nil && self.observation.Observe != nil {
		self.observation.Observe(PgOwnershipEvent{Kind: kind, BackendPid: self.backendPid, Keys: slices.Clone(self.keys)})
	}
}

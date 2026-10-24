// Exempts one maintenance operation's session from the application role's
// transaction_timeout (PARTITION-MAINTENANCE-PLAN.md A6).
//
// A role default of 15 minutes bounds every transaction of the application's
// sessions, explicit or the implicit one around a single statement, and
// PostgreSQL ends the session (FATAL 25P04) once one runs past it. A
// maintenance operation that legitimately runs longer, such as a concurrent
// rebuild or a VACUUM FULL of a large table, a long copy or a partition
// cutover, lifts the bound for its own session only: the maintenance pool also
// carries ordinary application work, so the exemption is per operation, never
// per pool.
//
// PostgreSQL 18 arms the timer when a transaction starts, with the value in
// effect then, and disarms it at commit or abort. Inside a transaction the
// setting's assign hook disarms a running timer when the value drops to zero,
// arms a new one counted from that moment when a positive value is set while
// none runs, and leaves a running timer alone otherwise. So `SET LOCAL
// transaction_timeout = 0` as the first statement of a transaction exempts that
// whole transaction, while no later SET restores its original deadline. A
// statement that runs several transactions of its own (REINDEX CONCURRENTLY,
// CREATE INDEX CONCURRENTLY, a VACUUM) gets a fresh timer for each of them.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urnetwork/glog"
)

// PgCanExec is satisfied by PgConn, PgTx and any other value that can execute
// a statement.
type PgCanExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (PgTag, error)
}

// The SQLSTATE of a session PostgreSQL ended because one of its transactions
// ran past transaction_timeout (PostgreSQL 17+). The retry taxonomy never
// reruns it (see db_retry_taxonomy.go): the bound ended the work, and a rerun
// would run into the same bound.
const pgTransactionTimeoutCode = "25P04"

// The `PgConn.CustomData` key that marks a session of the maintenance pool,
// which connects straight to PostgreSQL, so it may carry a session-level SET.
// The main pool goes through the transaction pooler, where a session SET would
// reach other clients' sessions.
const pgDirectSessionKey = "github.com/urnetwork/server.directSession"

// The `PgConn.CustomData` key that marks a session whose transaction_timeout
// WithoutTransactionTimeout has lifted and will restore.
const pgTransactionTimeoutExemptKey = "github.com/urnetwork/server.transactionTimeoutExempt"

// Bounds restoring a session's transaction_timeout, on a context detached from
// the exempted operation's. RESET does no work in PostgreSQL; its only wait is
// the round trip, the budget a connection's validation Ping is given.
const pgTransactionTimeoutRestoreTimeout = PgPingTimeout

// The id of the code migration that sets the application roles' bound; the
// monitor's migration contract names it by this id.
const ApplicationTransactionTimeoutMigrationId = "20261024_set_application_transaction_timeout"

// Bounds every transaction of the application's sessions at 15 minutes, as a
// default of the role each pool connects as, in this database only. The main
// pool goes through the transaction pooler, so neither a startup parameter nor
// a session SET can carry the bound there; a server-side default can. Each
// pool sets the default of its own role (CURRENT_USER), which an ordinary role
// may do for itself, so no role name is read from the vault resources or
// written anywhere. Scoped to the role and this database, the default leaves
// alone every other role (the operator's superuser, replication, backups),
// every other database, and physical replication, whose walsender reads no
// role settings. A superuser role is skipped: its default would also bound the
// operator's own sessions. Sessions read role defaults when they start, so the
// bound reaches each pool as its connections recycle. Idempotent: ALTER ROLE
// SET replaces the role's earlier value.
func migrationSetApplicationTransactionTimeout(ctx context.Context) {
	setRoleDefault := func(poolName string) func(PgConn) {
		return func(conn PgConn) {
			var superuser bool
			var alterSql string
			Raise(conn.QueryRow(
				ctx,
				`
					SELECT
						rolsuper,
						format(
							'ALTER ROLE CURRENT_USER IN DATABASE %I SET transaction_timeout = %L',
							current_database(),
							'15min'
						)
					FROM pg_roles
					WHERE rolname = current_user
				`,
			).Scan(&superuser, &alterSql))
			if superuser {
				glog.Infof("[migrate]transaction_timeout: the %s pool connects as a superuser role, whose default would also bound operator sessions; left unbounded\n", poolName)
				return
			}
			RaisePgResult(conn.Exec(ctx, alterSql))
			glog.Infof("[migrate]transaction_timeout: set 15min for new sessions of the %s pool's role\n", poolName)
		}
	}
	Db(ctx, setRoleDefault("main"), OptReadWrite(), OptNoRetry())
	MaintenanceDb(ctx, setRoleDefault("maintenance"), OptReadWrite(), OptNoRetry())
}

// Marks every session of a pool that connects straight to PostgreSQL (see
// pgDirectSessionKey) once it is established. Only the maintenance pool does
// (pg_maintenance.yml); every session SET in the maintenance code relies on it.
func configurePgPoolDirectSession(config *pgxpool.Config, direct bool) {
	if !direct {
		return
	}
	afterConnect := config.AfterConnect
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if afterConnect != nil {
			if err := afterConnect(ctx, conn); err != nil {
				return err
			}
		}
		conn.PgConn().CustomData()[pgDirectSessionKey] = true
		return nil
	}
}

var errTransactionTimeoutPooledSession = errors.New(
	"transaction_timeout exemption needs a maintenance pool session: a session SET through the transaction pooler would reach other clients",
)

var errTransactionTimeoutInTransaction = errors.New(
	"transaction_timeout exemption needs a session idle outside a transaction: inside one, a RESET cannot give the rest of the transaction its deadline back",
)

// Runs callback with transaction_timeout off for conn's session, then restores
// the session's default. Every transaction that starts on conn while the
// callback runs, including the implicit one around each autocommit statement,
// runs without the bound; the callback may begin its own transactions there.
//
// conn must be a session of the maintenance pool (`MaintenanceDb` or
// `AcquireMaintenanceDbConn`), and idle outside a transaction. Through the main
// pool's transaction pooler a session SET would reach other clients' sessions
// and a later RESET another server, and inside a transaction a RESET cannot
// give the rest of that transaction its deadline back. Either refusal panics
// before any statement runs. A call from inside the callback on the same
// session runs its own callback under the outer exemption.
//
// The default is restored when the callback returns or panics, on a context
// detached from ctx, so a canceled caller still restores it. A session that
// cannot be restored (closed, busy, left inside a transaction, or failing the
// RESET) is closed instead, so its pool destroys it on release rather than
// handing the exemption to later work. The callback must not release or hijack
// conn.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	withoutTransactionTimeout(ctx, &pgPoolTimeoutSession{conn: conn}, callback)
}

// The session operations the exemption needs, so its ordering and failure
// handling are testable without a database.
type pgTimeoutSession interface {
	// a session of a pool that connects straight to PostgreSQL
	direct() bool
	// open, not busy, and idle outside a transaction
	idle() bool
	// an enclosing exemption holds this session
	exempt() bool
	setExempt(exempt bool)
	exec(ctx context.Context, sql string) error
	// destroys the session, so its pool does not reuse it
	close(ctx context.Context)
}

func withoutTransactionTimeout(ctx context.Context, session pgTimeoutSession, callback func()) {
	if session.exempt() {
		// the enclosing call restores the session
		callback()
		return
	}
	if !session.direct() {
		panic(errTransactionTimeoutPooledSession)
	}
	if !session.idle() {
		panic(errTransactionTimeoutInTransaction)
	}
	session.setExempt(true)
	defer func() {
		session.setExempt(false)
		restoreCtx, restoreCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			pgTransactionTimeoutRestoreTimeout,
		)
		defer restoreCancel()
		// Inside a transaction a RESET would neither re-arm that transaction's
		// timer nor survive its rollback, so only an idle session is restored.
		if session.idle() && session.exec(restoreCtx, `RESET transaction_timeout`) == nil {
			return
		}
		session.close(restoreCtx)
	}()
	Raise(session.exec(ctx, `SET transaction_timeout = 0`))
	callback()
}

// Lifts the role's transaction bound for the rest of a transaction. A
// statement list that lifts its statement_timeout for a long statement lifts
// this too, or the transaction bound would end the statement anyway.
const liftTransactionTimeoutInTxSql = `SET LOCAL transaction_timeout = 0`

// The statement that replaces the transaction's bound: zero lifts it, and a
// positive bound is armed afresh after the running timer is disarmed, since
// PostgreSQL leaves a running timer alone when a positive value is set.
func transactionTimeoutInTxSql(bound time.Duration) string {
	if bound <= 0 {
		return liftTransactionTimeoutInTxSql
	}
	return fmt.Sprintf(`SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = %d`, max(int64(1), bound.Milliseconds()))
}

// Replaces the role's transaction_timeout for the rest of the caller's
// transaction with bound, counted from this call; a non-positive bound lifts
// it. For a long transaction that keeps a bound of its own (its owner's
// deadline, a statement_timeout), called first in the transaction: the role's
// bound counts from BEGIN. SET LOCAL ends with the transaction, so this is safe
// on the main pool's transaction pooler too.
func SetTransactionTimeoutInTx(ctx context.Context, tx PgCanExec, bound time.Duration) {
	RaisePgResult(tx.Exec(ctx, transactionTimeoutInTxSql(bound)))
}

// A pooled connection as a pgTimeoutSession.
type pgPoolTimeoutSession struct {
	conn PgConn
}

func (self *pgPoolTimeoutSession) direct() bool {
	direct, _ := self.conn.Conn().PgConn().CustomData()[pgDirectSessionKey].(bool)
	return direct
}

func (self *pgPoolTimeoutSession) idle() bool {
	physical := self.conn.Conn().PgConn()
	return !physical.IsClosed() && !physical.IsBusy() && physical.TxStatus() == 'I'
}

func (self *pgPoolTimeoutSession) exempt() bool {
	exempt, _ := self.conn.Conn().PgConn().CustomData()[pgTransactionTimeoutExemptKey].(bool)
	return exempt
}

func (self *pgPoolTimeoutSession) setExempt(exempt bool) {
	data := self.conn.Conn().PgConn().CustomData()
	if exempt {
		data[pgTransactionTimeoutExemptKey] = true
	} else {
		delete(data, pgTransactionTimeoutExemptKey)
	}
}

func (self *pgPoolTimeoutSession) exec(ctx context.Context, sql string) error {
	_, err := self.conn.Exec(ctx, sql)
	return err
}

func (self *pgPoolTimeoutSession) close(ctx context.Context) {
	closePgConnection(ctx, self.conn.Conn())
}

// One failed session cleanup must not hold every later pending operation.
package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The earlier operation's completion write fails. The later operation must
// still complete, while the failed one stays pending and its error is returned.
func TestRecoverSessionOperationsContinuesPastFailedCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		first, second := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, first, "synthetic-recovery-first", server.NewId())
		Testing_CreateNetwork(ctx, second, "synthetic-recovery-second", server.NewId())
		firstOperation, secondOperation := server.NewId(), server.NewId()
		cutoff := server.NowUtc().Add(-time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(session.JournalSessionRetirementInTx(ctx, tx, first, firstOperation, "reset", cutoff))
			server.Raise(session.JournalSessionRetirementInTx(ctx, tx, second, secondOperation, "reset", cutoff.Add(time.Second)))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
				CREATE FUNCTION synthetic_session_cleanup_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.network_id='%s'::uuid THEN
						RAISE EXCEPTION USING ERRCODE='53200',MESSAGE='synthetic session cleanup failure';
					END IF;
					RETURN NEW;
				END $$;
				CREATE TRIGGER synthetic_session_cleanup_failure BEFORE UPDATE OF status ON network_session_operation
				FOR EACH ROW WHEN (NEW.status='complete') EXECUTE FUNCTION synthetic_session_cleanup_failure();`, first)))
		})
		err := RecoverSessionOperations(ctx, 32)
		statuses := map[server.Id]string{}
		server.Db(ctx, func(conn server.PgConn) {
			for _, operation := range []struct{ networkId, operationId server.Id }{
				{networkId: first, operationId: firstOperation}, {networkId: second, operationId: secondOperation},
			} {
				var status string
				server.Raise(conn.QueryRow(ctx, `SELECT status FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`,
					operation.networkId, operation.operationId).Scan(&status))
				statuses[operation.operationId] = status
			}
		})
		if err == nil || !strings.Contains(err.Error(), "synthetic session cleanup failure") {
			t.Fatal("the failed cleanup disappeared from recovery's result", err)
		}
		if statuses[firstOperation] != "enforced" || statuses[secondOperation] != "complete" {
			t.Fatal("one failed cleanup held a later operation, or the failed one left pending", statuses)
		}
	})
}

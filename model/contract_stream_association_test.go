// A contract's stream association writes the contract row only when the row
// does not already carry the stream.
package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// Counts the row versions written to each transfer_contract row after its
// insert. The trigger exists only in the test environment's disposable
// database.
func countContractRowWrites(t testing.TB, ctx context.Context) func(contractId server.Id) int64 {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE TABLE test_contract_row_write (contract_id uuid NOT NULL);
			CREATE FUNCTION test_contract_row_write() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO test_contract_row_write (contract_id) VALUES (NEW.contract_id);
				RETURN NULL;
			END;
			$$;
			CREATE TRIGGER test_contract_row_write AFTER UPDATE ON transfer_contract
			FOR EACH ROW EXECUTE FUNCTION test_contract_row_write();
		`))
	})
	return func(contractId server.Id) (writeCount int64) {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_contract_row_write WHERE contract_id = $1`, contractId).Scan(&writeCount))
		})
		return
	}
}

func readContractStreamId(ctx context.Context, contractId server.Id) (streamId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT stream_id FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&streamId))
	})
	return
}

// The stream join after commit wrote the contract row with an UPDATE every
// time, also when the row already had the stream: a repeated association
// rewrote the same value. The first association of a contract created without
// a binding writes the row once, and a repeat writes nothing.
func TestSetContractStreamWritesContractRowOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		networkId := server.NewId()
		origin, egress := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, egress: networkId})

		contractId, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		if streamId := readContractStreamId(ctx, contractId); streamId != nil {
			t.Fatal("a contract created without a binding has a stream id", *streamId)
		}
		streamId := AddToStream(ctx, contractId, origin, egress, nil)
		for range 2 {
			if err := SetContractStream(ctx, contractId, streamId, nil); err != nil {
				t.Fatal(err)
			}
		}
		if written := readContractStreamId(ctx, contractId); written == nil || *written != streamId {
			t.Fatal("late stream association did not write the stream id", written)
		}
		if writeCount := contractRowWriteCount(contractId); writeCount != 1 {
			t.Fatal("late stream association wrote the contract row more than once", writeCount)
		}
	})
}

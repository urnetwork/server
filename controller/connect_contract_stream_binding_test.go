package controller

// A streamed contract's insert writes its stream id
// (model/contract_stream_binding.go). Creating it through the controller
// writes the contract row once, on the escrow, companion and network paths.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Before the binding, every streamed contract was inserted without its
// stream and joined it with a second, never-HOT UPDATE of the fresh row. The
// trigger counts the row versions written after each insert; it exists only
// in the test environment's disposable database.
func TestCreateContractWritesStreamAtInsert(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
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
		requireStreamWrittenAtInsert := func(name string, stored *protocol.StoredContract, wantStreamId *server.Id) server.Id {
			t.Helper()
			if len(stored.StreamId) == 0 {
				t.Fatal(name, "contract carries no stream")
			}
			streamId := server.Id(stored.StreamId)
			if wantStreamId != nil && streamId != *wantStreamId {
				t.Fatal(name, "contract did not join the expected stream", streamId, *wantStreamId)
			}
			contractId := server.Id(stored.ContractId)
			var rowStreamId *server.Id
			var writeCount int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT stream_id FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&rowStreamId))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_contract_row_write WHERE contract_id = $1`, contractId).Scan(&writeCount))
			})
			if rowStreamId == nil || *rowStreamId != streamId {
				t.Fatal(name, "contract row does not carry its stream", rowStreamId, streamId)
			}
			if writeCount != 0 {
				t.Fatal(name, "contract row was written again after its insert", writeCount)
			}
			return streamId
		}

		streamVersion1 := uint32(1)
		createContract := func(source server.Id, destination server.Id, companion bool, forceStream bool, intermediaryIds []server.Id) *protocol.StoredContract {
			t.Helper()
			intermediaryIdBytes := make([][]byte, len(intermediaryIds))
			for intermediaryIndex, intermediaryId := range intermediaryIds {
				intermediaryIdBytes[intermediaryIndex] = intermediaryId.Bytes()
			}
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{
				DestinationId:     destination.Bytes(),
				IntermediaryIds:   intermediaryIdBytes,
				TransferByteCount: uint64(1024 * 1024),
				Companion:         companion,
				ForceStream:       &forceStream,
				StreamVersion:     &streamVersion1,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			stored := &protocol.StoredContract{}
			connect.AssertEqual(t, proto.Unmarshal(result.Contract.StoredContractBytes, stored), nil)
			return stored
		}
		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
			return clientId
		}
		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), server.NewId())
			err := model.AddBasicTransferBalance(
				ctx,
				networkId,
				model.ByteCount(1024*1024*1024*1024),
				server.NowUtc(),
				server.NowUtc().Add(365*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		// escrow: a funded consumer's forced stream to a public provider starts
		// the stream; a second forced contract joins it; the provider's
		// companion reply rides the origin's stream
		consumer := newClient(newFundedNetwork())
		provider := newClient(newFundedNetwork())
		model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{
			model.ProvideModePublic: []byte("test-provide-secret-key-public00"),
		})
		model.SetProvide(ctx, consumer, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("test-provide-secret-key-stream00"),
		})
		escrowStreamId := requireStreamWrittenAtInsert("escrow start:", createContract(consumer, provider, false, true, nil), nil)
		requireStreamWrittenAtInsert("escrow join:", createContract(consumer, provider, false, true, nil), &escrowStreamId)
		requireStreamWrittenAtInsert("companion:", createContract(provider, consumer, true, false, nil), &escrowStreamId)

		// network: a multi-hop forward starts the stream and the reply without
		// an intermediary list joins the pair's stream
		networkId := newFundedNetwork()
		client := newClient(networkId)
		peer := newClient(networkId)
		intermediary := newClient(networkId)
		for _, clientId := range []server.Id{client, peer} {
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: []byte("test-provide-secret-key-network0"),
				model.ProvideModeStream:  []byte("test-provide-secret-key-stream00"),
			})
		}
		networkStreamId := requireStreamWrittenAtInsert("network multi-hop:", createContract(client, peer, false, true, []server.Id{intermediary}), nil)
		requireStreamWrittenAtInsert("network pair reply:", createContract(peer, client, false, false, nil), &networkStreamId)
	})
}

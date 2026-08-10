package task

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type addressHashWorkArgs struct{}

type addressHashWorkResult struct{}

func addressHashWork(
	args *addressHashWorkArgs,
	clientSession *session.ClientSession,
) (*addressHashWorkResult, error) {
	return &addressHashWorkResult{}, nil
}

// The task tables persist only the peppered address hash + port of the
// scheduling session (server.ClientIpHash), never the raw ip:port. This
// covers the write path, the read path, the session reconstruction, and the
// legacy fallback for rows written before the migration.
func TestTaskClientAddressHash(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientAddress := "1.2.3.4:5678"
		expectedHash, err := server.ClientIpHash("1.2.3.4")
		assert.Equal(t, err, nil)

		clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			addressHashWork,
			&addressHashWorkArgs{},
			clientSession,
		)

		// the row itself: hash + port stored, raw address NOT stored
		var storedAddress string
		var storedHash []byte
		var storedPort int
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
					SELECT client_address, client_address_hash, client_address_port
					FROM pending_task
					WHERE task_id = $1
				`,
				taskId,
			)
			server.WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				server.Raise(result.Scan(&storedAddress, &storedHash, &storedPort))
			})
		})
		assert.Equal(t, storedAddress, "")
		assert.Equal(t, storedHash, expectedHash[:])
		assert.Equal(t, storedPort, 5678)

		// the read path and the reconstructed session: the hash round-trips,
		// and the session carries no raw address
		task, ok := GetTasks(ctx, taskId)[taskId]
		assert.Equal(t, ok, true)
		assert.Equal(t, task.ClientAddress, "")
		assert.Equal(t, task.ClientAddressHash, expectedHash[:])
		assert.Equal(t, task.ClientAddressPort, 5678)

		taskSession, err := task.ClientSession(ctx)
		assert.Equal(t, err, nil)
		defer taskSession.Cancel()
		assert.Equal(t, taskSession.ClientAddress, "")
		sessionHash, sessionPort, err := taskSession.ClientAddressHashPort()
		assert.Equal(t, err, nil)
		assert.Equal(t, sessionHash, expectedHash)
		assert.Equal(t, sessionPort, 5678)
	})
}

// A row written before the migration (raw client_address, NULL hash) must
// still reconstruct a working session from the raw address until it drains.
func TestTaskClientAddressLegacyRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientAddress := "5.6.7.8:1234"
		expectedHash, err := server.ClientIpHash("5.6.7.8")
		assert.Equal(t, err, nil)

		clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			addressHashWork,
			&addressHashWorkArgs{},
			clientSession,
		)

		// rewrite the row into its pre-migration shape
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE pending_task
					SET client_address = $2,
						client_address_hash = NULL,
						client_address_port = 0
					WHERE task_id = $1
				`,
				taskId,
				clientAddress,
			))
		})

		task, ok := GetTasks(ctx, taskId)[taskId]
		assert.Equal(t, ok, true)
		assert.Equal(t, task.ClientAddress, clientAddress)

		taskSession, err := task.ClientSession(ctx)
		assert.Equal(t, err, nil)
		defer taskSession.Cancel()
		assert.Equal(t, taskSession.ClientAddress, clientAddress)
		sessionHash, sessionPort, err := taskSession.ClientAddressHashPort()
		assert.Equal(t, err, nil)
		assert.Equal(t, sessionHash, expectedHash)
		assert.Equal(t, sessionPort, 1234)
	})
}

// A scheduling session with no parseable address (internal/local schedulers)
// stores NULL, and the reconstructed session reports no hash rather than a
// zero hash.
func TestTaskClientAddressAbsent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			addressHashWork,
			&addressHashWorkArgs{},
			clientSession,
		)

		task, ok := GetTasks(ctx, taskId)[taskId]
		assert.Equal(t, ok, true)
		assert.Equal(t, task.ClientAddress, "")
		assert.Equal(t, len(task.ClientAddressHash), 0)

		taskSession, err := task.ClientSession(ctx)
		assert.Equal(t, err, nil)
		defer taskSession.Cancel()
		_, _, err = taskSession.ClientAddressHashPort()
		if err == nil {
			t.Fatal("an address-less task session must not report an address hash")
		}
	})
}

package controller

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A provide frame that declares post-quantum encryption sessions, as connect's
// provider sends it, is recorded for discovery through the control dispatch;
// the provide of a release before the declaration records none (support inbox
// 7918, https://github.com/urnetwork/connect/issues/220).
func TestProvideFrameRecordsPostQuantumEncryptionDeclaration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

		declared := func() bool {
			var postQuantumEncryption bool
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT post_quantum_encryption FROM provide_capability WHERE client_id = $1`,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&postQuantumEncryption))
					}
				})
			})
			return postQuantumEncryption
		}
		provide := func(postQuantumEncryption bool) {
			frame, err := connect.ToFrame(&protocol.Provide{
				Keys: []*protocol.ProvideKey{{
					Mode:             protocol.ProvideMode_Public,
					ProvideSecretKey: bytes.Repeat([]byte{7}, 32),
				}},
				PostQuantumEncryption: postQuantumEncryption,
			}, connect.DefaultProtocolVersion)
			connect.AssertEqual(t, err, nil)
			frames := []*protocol.Frame{frame}
			defer returnConnectControlFrames(frames)
			outFrames, err := ConnectControlFrames(ctx, clientId, frames, connect.DefaultContractManagerSettings())
			defer returnConnectControlFrames(outFrames)
			connect.AssertEqual(t, err, nil)
		}

		provide(true)
		connect.AssertEqual(t, declared(), true)
		provide(false)
		connect.AssertEqual(t, declared(), false)
	})
}

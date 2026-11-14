// The real public challenge and acceptance paths read the actual finalized
// coordinator epoch. No supplied epoch/current-state verdict bypasses that RPC.
package controller

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urnetwork/server/v2026"
)

// Advancing the raw RPC boundary between issue and signature deterministically
// exercises the public stale-consent refusal, without waiting for a real epoch.
func TestWalletMappingPublicFinalizedEpochPreventsRetroactiveConsent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, credential, _ := newStClientKeyHistoryControllerFixture(t)
		fixture.boundary.Epoch = 50
		endpoint := walletMappingControllerHttp(t)
		key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{62})
		if err != nil {
			t.Fatal(err)
		}
		address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		args := &SnWalletMappingChallengeArgs{ClientId: credential.ClientId, ColdkeySs58: address, FromEpoch: 1, ThroughEpoch: 100}
		if status, _ := walletMappingControllerPost(t, endpoint.URL, "/sn/wallet/consent", credential.Testing_Sign(), args); status == http.StatusOK {
			t.Fatal("authenticated caller backdated consent despite current finalized epoch50")
		}
		args.FromEpoch = 51
		status, raw := walletMappingControllerPost(t, endpoint.URL, "/sn/wallet/consent", credential.Testing_Sign(), args)
		var challenge SnWalletMappingChallengeResult
		if status != http.StatusOK || json.Unmarshal(raw, &challenge) != nil {
			t.Fatal("future challenge failed", status, string(raw))
		}
		statement, err := protocol.DecodeWalletMappingStatement(challenge.Message)
		if err != nil || statement.Schema != protocol.WalletMappingProspectiveSchema || statement.Prospective.Boundary != fixture.boundary || statement.Prospective.Signer != fixture.root {
			t.Fatal("challenge did not retain actual original epoch authority", statement, err)
		}
		signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(challenge.Message)))
		if err != nil {
			t.Fatal(err)
		}
		encoded := signature.Encode()
		fixture.boundary = protocol.ClientKeyEffectiveBoundary{Epoch: 51, Block: 101, Hash: [32]byte{6}}
		set := &SnSetWalletArgs{ClientId: credential.ClientId, ColdkeySs58: address, Message: challenge.Message, Signature: "0x" + hex.EncodeToString(encoded[:])}
		status, raw = walletMappingControllerPost(t, endpoint.URL, "/sn/wallet", credential.Testing_Sign(), set)
		var result SnSetWalletResult
		if status == http.StatusOK && json.Unmarshal(raw, &result) == nil && result.Error == nil {
			t.Fatal("late actual signature changed an already-earned mapping", string(raw))
		}
		var retained int
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM wallet_mapping_consent WHERE client_id=$1`, *credential.ClientId).Scan(&retained))
		})
		if retained != 0 {
			t.Fatal("refused historical signature retained a wallet projection", retained)
		}
	})
}

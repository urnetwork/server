// Actual SQL custody tests retain independent coldkey signatures, nonce replay
// identity and wallet projections under rollback and directory changes.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urnetwork/server/v2026"
)

// All identities and the original signing key are test-owned synthetic values.
type walletMappingFixture struct {
	owner   WalletMappingOwner
	key     *schnorrkel.MiniSecretKey
	address string
}

// The actual network directory is required before a challenge can be issued.
func newWalletMappingFixture(t testing.TB) *walletMappingFixture {
	t.Helper()
	key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{37})
	if err != nil {
		t.Fatal(err)
	}
	address, err := ss58.Encode(key.Public().Encode(), ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	owner := WalletMappingOwner{Domain: protocol.ClientKeyHistoryDomain{ChainID: 964, GenesisHash: [32]byte{1}, Netuid: 25, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: [32]byte{4}, PolicyHash: [32]byte{5}, NoID: 6}, UserId: server.NewId(), ClientId: server.NewId(), NetworkId: server.NewId()}
	addContractPayoutTestClients(t.Context(), map[server.Id]server.Id{owner.ClientId: owner.NetworkId})
	return &walletMappingFixture{owner: owner, key: key, address: address}
}

// A real wallet signs the complete server-issued message, not a rebuilt tuple.
func (self *walletMappingFixture) challenge(t testing.TB, from uint64) protocol.WalletMappingConsent {
	t.Helper()
	message, err := CreateWalletMappingChallenge(t.Context(), self.owner, self.key.Public().Encode(), from, from+100)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := self.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	return protocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}
}

// Query original rows rather than trusting the returned admission projection.
func (self *walletMappingFixture) requireCounts(t testing.TB, count int) {
	t.Helper()
	var consents, wallets int
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM wallet_mapping_consent WHERE client_id=$1`, self.owner.ClientId).Scan(&consents))
		server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM st_provider_wallet_history WHERE client_id=$1`, self.owner.ClientId).Scan(&wallets))
	})
	if consents != count || wallets != count {
		t.Fatal("consent and wallet projections lost atomic original census", consents, wallets, count)
	}
}

// Lost acknowledgements reuse one exact receipt; replaying an older accepted
// consent after rotation cannot restore an obsolete wallet or append history.
func TestWalletMappingAcceptanceRetainsOriginalAndExactRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newWalletMappingFixture(t)
		first := f.challenge(t, 0)
		accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, first, f.address)
		if err != nil || accepted == nil || !accepted.Applied || accepted.Generation != 1 {
			t.Fatal("original mapping was not accepted", accepted, err)
		}
		for range 2 {
			retained, err := AcceptWalletMappingConsent(t.Context(), f.owner, first, f.address)
			if err != nil || retained == nil || retained.Applied || retained.OriginalHash != accepted.OriginalHash {
				t.Fatal("lost acknowledgement changed original mapping", retained, err)
			}
		}
		f.requireCounts(t, 1)
		second := f.challenge(t, 10)
		next, err := AcceptWalletMappingConsent(t.Context(), f.owner, second, f.address)
		if err != nil || next == nil || !next.Applied || next.Generation != 2 {
			t.Fatal("original successor was not retained", next, err)
		}
		if old, err := AcceptWalletMappingConsent(t.Context(), f.owner, first, f.address); err != nil || old == nil || old.Applied {
			t.Fatal("earlier exact replay changed a newer head", old, err)
		}
		f.requireCounts(t, 2)
		originals, err := ReadWalletMappingHistory(t.Context(), f.owner.Domain, f.owner.ClientId, 2, next.OriginalHash)
		if err != nil || len(originals) != 2 || originals[0] != first || originals[1] != second {
			t.Fatal("actual public-history model lost original signed bytes", err)
		}
		for _, epoch := range []uint64{0, 10} {
			mapping, err := protocol.VerifyWalletMappingHistory(t.Context(), originals, protocol.WalletMappingHistoryExpectation{Domain: f.owner.Domain, ClientId: [16]byte(f.owner.ClientId), HeadHash: next.OriginalHash, Generation: 2, Epoch: epoch})
			if err != nil || mapping == nil || mapping.Statement.FromEpoch != epoch {
				t.Fatal("consumer changed the original selected epoch", epoch, mapping, err)
			}
		}
	})
}

// A second valid signature cannot change the stored nonce's original statement.
func TestWalletMappingRejectsForeignCallerAndChangedIssuedMessage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newWalletMappingFixture(t)
		original := f.challenge(t, 0)
		for _, mutate := range []func(*WalletMappingOwner){func(o *WalletMappingOwner) { o.UserId = server.NewId() }, func(o *WalletMappingOwner) { o.NetworkId = server.NewId() }, func(o *WalletMappingOwner) { o.ClientId = server.NewId() }, func(o *WalletMappingOwner) { o.Domain.NoID++ }} {
			changed := f.owner
			mutate(&changed)
			if accepted, err := AcceptWalletMappingConsent(t.Context(), changed, original, f.address); accepted != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("foreign authenticated scope borrowed consent", accepted, err)
			}
		}
		statement, err := protocol.DecodeWalletMappingStatement(original.Message)
		if err != nil {
			t.Fatal(err)
		}
		statement.ThroughEpoch++
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		signature, err := f.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
		if err != nil {
			t.Fatal(err)
		}
		changed := protocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, changed, f.address); accepted != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("changed issued statement gained original nonce", accepted, err)
		}
		f.requireCounts(t, 0)
	})
}

// A failed wallet projection rolls back nonce consumption and the signed
// original; the identical request succeeds once after storage recovers.
func TestWalletMappingProjectionRollbackRetainsRetryAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newWalletMappingFixture(t)
		original := f.challenge(t, 0)
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `CREATE FUNCTION wallet_mapping_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic wallet projection refused'; END $$; CREATE TRIGGER wallet_mapping_test_refuse BEFORE INSERT ON st_wallet FOR EACH ROW EXECUTE FUNCTION wallet_mapping_test_refuse()`))
		})
		remove := func() {
			server.Tx(t.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(t.Context(), `DROP TRIGGER IF EXISTS wallet_mapping_test_refuse ON st_wallet; DROP FUNCTION IF EXISTS wallet_mapping_test_refuse()`))
			})
		}
		defer remove()
		if err := server.HandleError(func() {
			_, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address)
			server.Raise(err)
		}); err == nil {
			t.Fatal("failed wallet projection was reported successful")
		}
		f.requireCounts(t, 0)
		remove()
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address); err != nil || accepted == nil || !accepted.Applied {
			t.Fatal("rolled-back original could not resume", accepted, err)
		}
		f.requireCounts(t, 1)
	})
}

// Directory movement after issuance cannot redirect a signed provider mapping.
func TestWalletMappingDirectoryChangeAndCancellationWithholdProjection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newWalletMappingFixture(t)
		original := f.challenge(t, 0)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if accepted, err := AcceptWalletMappingConsent(ctx, f.owner, original, f.address); accepted != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled mapping published a projection", accepted, err)
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, f.owner.ClientId, server.NewId()))
		})
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address); accepted != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("changed directory borrowed prior consent", accepted, err)
		}
		f.requireCounts(t, 0)
	})
}

// An actual original expiry is rejected without sleeps or overriding the clock;
// append-only guards refuse later rewriting of the retained signed statement.
func TestWalletMappingExpiryAndImmutableHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newWalletMappingFixture(t)
		original := f.challenge(t, 0)
		statement, err := protocol.DecodeWalletMappingStatement(original.Message)
		if err != nil {
			t.Fatal(err)
		}
		statement.Nonce[0]++
		now := server.NowUtc()
		statement.IssuedAt, statement.ExpiresAt = now.Add(-10*time.Minute).Unix(), now.Add(-5*time.Minute).Unix()
		message, err := statement.Message()
		if err != nil {
			t.Fatal(err)
		}
		domain, _ := f.owner.Domain.Digest()
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_challenge(nonce,domain_hash,client_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], domain[:], f.owner.ClientId, 1, statement.ExpiresAt, message))
		})
		signature, err := f.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
		if err != nil {
			t.Fatal(err)
		}
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, protocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}, f.address); accepted != nil || err == nil {
			t.Fatal("expired original became a fresh mapping", accepted, err)
		}
		f.requireCounts(t, 0)
		if accepted, err := AcceptWalletMappingConsent(t.Context(), f.owner, original, f.address); err != nil || accepted == nil {
			t.Fatal(err)
		}
		for _, query := range []string{`UPDATE wallet_mapping_consent SET original=$2 WHERE client_id=$1`, `DELETE FROM wallet_mapping_consent WHERE client_id=$1 AND $2::bytea IS NOT NULL`} {
			if err := server.HandleError(func() {
				server.Tx(t.Context(), func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(t.Context(), query, f.owner.ClientId, []byte(`{}`)))
				})
			}); err == nil {
				t.Fatal("original mapping mutation succeeded")
			}
		}
		f.requireCounts(t, 1)
		if raw, err := json.Marshal(original); err != nil || len(raw) > protocol.MaxWalletMappingConsentBytes {
			t.Fatal("original mapping exceeded its retained profile", err)
		}
	})
}

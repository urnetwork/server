// Historical test consent supplies real wallet and prospective-root signatures
// before the independently approved payout roster fixes its immutable head.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Seed only an explicitly historical fixture: the actual current challenge API
// cannot issue an already-expired consent before this test's closed epoch. The
// same production decoders, history reader and prospective verifier admit it.
func providerWorkRetainFixtureWallet(t testing.TB, cfg *StConfig, domain snprotocol.ClientKeyHistoryDomain, epoch, startBlock uint64, startTime time.Time, clientId, networkId [16]byte) payoutartifact.WholeWorkExpectedProvider {
	t.Helper()
	if cfg == nil || cfg.RootKey == nil || epoch == 0 || startBlock == 0 {
		t.Fatal("historical wallet fixture requires an original prospective boundary")
	}
	seed := sha256.Sum256(append([]byte("synthetic-provider-work-wallet/"), clientId[:]...))
	key, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		t.Fatal(err)
	}
	nonce := sha256.Sum256(append([]byte("synthetic-provider-work-wallet-nonce/"), clientId[:]...))
	boundaryHash := sha256.Sum256(append([]byte("synthetic-provider-work-wallet-boundary/"), clientId[:]...))
	statement := snprotocol.WalletMappingStatement{Domain: domain, UserId: [16]byte(server.NewId()), ClientId: clientId, NetworkId: networkId, Coldkey: key.Public().Encode(), Generation: 1, Nonce: nonce, IssuedAt: startTime.Add(-10 * time.Minute).Unix(), ExpiresAt: startTime.Add(-5 * time.Minute).Unix(), FromEpoch: epoch, ThroughEpoch: epoch + 100}
	boundary := snprotocol.ClientKeyEffectiveBoundary{Epoch: epoch - 1, Block: startBlock - 1, Hash: boundaryHash}
	if err := snprotocol.SignProspectiveWalletMapping(&statement, boundary, cfg.RootKey); err != nil {
		t.Fatal(err)
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	original := snprotocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}
	_, head, err := snprotocol.VerifyWalletMappingConsent(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	domainHash, err := domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_challenge(nonce,domain_hash,client_id,generation,expires_at,message) VALUES($1,$2,$3,1,$4,$5)`, nonce[:], domainHash[:], server.Id(clientId), statement.ExpiresAt, message))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_consent(domain_hash,client_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,1,$3,$4,$5,$6)`, domainHash[:], server.Id(clientId), head[:], nonce[:], raw, time.Unix(statement.IssuedAt+1, 0).UTC()))
	})
	originals, err := model.ReadWalletMappingHistory(t.Context(), domain, server.Id(clientId), 1, head)
	if err != nil || len(originals) != 1 || originals[0] != original {
		t.Fatal("historical fixture lost its actual signed consent", err)
	}
	for _, activeEpoch := range []uint64{epoch, epoch + 1} {
		mapping, err := snprotocol.VerifyWalletMappingHistory(t.Context(), originals, snprotocol.WalletMappingHistoryExpectation{Domain: domain, ClientId: clientId, HeadHash: head, Generation: 1, Epoch: activeEpoch})
		if err != nil {
			t.Fatal(err)
		}
		if err := snprotocol.VerifyProspectiveWalletMapping(t.Context(), mapping, crypto.PubkeyToAddress(cfg.RootKey.PublicKey), startBlock, startTime.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	return payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, NetworkId: networkId, WalletHeadHash: hex.EncodeToString(head[:]), WalletGeneration: 1}
}

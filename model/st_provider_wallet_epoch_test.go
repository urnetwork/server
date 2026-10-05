// Actual immutable SQL originals and a newly accepted future rotation reproduce
// the difference between an epoch's earning wallet and the account projection.
package model

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
)

// Test-owned keys separate the wallet, operator and independent roster roles.
type stProviderWalletEpochFixture struct {
	wallet    *walletMappingFixture
	rosterKey *ecdsa.PrivateKey
	authority payoutartifact.WholeWorkAuthority
	expected  payoutartifact.WholeWorkExpectation
	scope     StProviderWalletEpochScope
	originals [2]protocol.WalletMappingConsent
	heads     [2][32]byte
	coldkeys  [2][32]byte
}

// The first original is a retained historical fixture accepted before epoch
// 51. Only that historical seed uses SQL directly; rotation uses the actual
// challenge and acceptance API while its authenticated boundary is epoch 51.
func newStProviderWalletEpochFixture(t testing.TB) *stProviderWalletEpochFixture {
	t.Helper()
	wallet := walletMappingProspectiveFixture(t)
	f := &stProviderWalletEpochFixture{wallet: wallet}
	var err error
	f.rosterKey, err = crypto.HexToECDSA(strings.Repeat("43", 32))
	if err != nil {
		t.Fatal(err)
	}
	startTime := server.NowUtc().Truncate(time.Second).Add(-10 * time.Minute)
	f.scope = StProviderWalletEpochScope{Domain: wallet.owner.Domain, Epoch: 51, Start: payoutartifact.Boundary{Number: 510, Hash: common.Hash{51}.Hex()}, End: payoutartifact.Boundary{Number: 520, Hash: common.Hash{52}.Hex()}, StartTime: startTime}
	f.coldkeys[0] = wallet.key.Public().Encode()
	statement := protocol.WalletMappingStatement{Domain: wallet.owner.Domain, UserId: [16]byte(wallet.owner.UserId), ClientId: [16]byte(wallet.owner.ClientId), NetworkId: [16]byte(wallet.owner.NetworkId), Coldkey: f.coldkeys[0], Generation: 1, Nonce: [32]byte{71}, IssuedAt: startTime.Add(-10 * time.Minute).Unix(), ExpiresAt: startTime.Add(-5 * time.Minute).Unix(), FromEpoch: 51, ThroughEpoch: 151}
	if err := protocol.SignProspectiveWalletMapping(&statement, wallet.owner.Prospective.Boundary, wallet.owner.Prospective.RootKey); err != nil {
		t.Fatal(err)
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := wallet.key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	f.originals[0] = protocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}
	_, f.heads[0], err = protocol.VerifyWalletMappingConsent(t.Context(), f.originals[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(f.originals[0])
	if err != nil {
		t.Fatal(err)
	}
	domain, err := wallet.owner.Domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_challenge(nonce,domain_hash,client_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], domain[:], wallet.owner.ClientId, 1, statement.ExpiresAt, message))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_consent(domain_hash,client_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], wallet.owner.ClientId, 1, f.heads[0][:], statement.Nonce[:], raw, time.Unix(statement.IssuedAt+1, 0).UTC()))
		encodedSignature := "0x" + hex.EncodeToString(f.originals[0].Signature[:])
		setStProviderWalletOriginalInTx(t.Context(), tx, wallet.owner.ClientId, wallet.owner.NetworkId, wallet.address, f.coldkeys[0], &message, &encodedSignature)
	})
	wallet.owner.Prospective.Boundary = protocol.ClientKeyEffectiveBoundary{Epoch: 51, Block: 510, Hash: [32]byte{51}}
	wallet.key, err = schnorrkel.NewMiniSecretKeyFromRaw([32]byte{38})
	if err != nil {
		t.Fatal(err)
	}
	f.coldkeys[1] = wallet.key.Public().Encode()
	wallet.address, err = ss58.Encode(f.coldkeys[1], ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	f.originals[1] = wallet.challenge(t, 52)
	accepted, err := AcceptWalletMappingConsent(t.Context(), wallet.owner, f.originals[1], wallet.address)
	if err != nil || accepted == nil || !accepted.Applied || accepted.Generation != 2 {
		t.Fatal("actual future rotation was not accepted in epoch 51", accepted, err)
	}
	f.heads[1] = accepted.OriginalHash
	f.authority = payoutartifact.WholeWorkAuthority{Domain: f.scope.Domain, Epoch: f.scope.Epoch, Start: f.scope.Start, End: f.scope.End, RequestPublicKey: [32]byte{72}, Owners: []payoutartifact.WholeWorkOwner{{ClientId: [16]byte(wallet.owner.ClientId), NetworkId: [16]byte(wallet.owner.NetworkId), Generation: [16]byte{73}, PublicKey: [32]byte{74}}}, PriorContracts: []payoutartifact.WholeWorkPriorContract{}, ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{{ClientId: [16]byte(wallet.owner.ClientId), NetworkId: [16]byte(wallet.owner.NetworkId), WalletHeadHash: hex.EncodeToString(f.heads[1][:]), WalletGeneration: 2}}}
	f.expected = payoutartifact.WholeWorkExpectation{AuthoritySigner: crypto.PubkeyToAddress(f.rosterKey.PublicKey), ClientKeyRootSigner: crypto.PubkeyToAddress(wallet.owner.Prospective.RootKey.PublicKey)}
	return f
}

// A changed roster must receive a fresh independent signature; tests never
// mutate an allegedly verified projection and call it original authority.
func (self *stProviderWalletEpochFixture) approve(t testing.TB, authority payoutartifact.WholeWorkAuthority) []byte {
	t.Helper()
	authority, err := payoutartifact.SignWholeWorkAuthority(t.Context(), authority, self.rosterKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := authority.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Acceptance changes the actual projection immediately, but the payout model
// keeps the old original in E and selects the new original only in E+1.
func TestStProviderWalletEpochKeepsCurrentWalletAfterProspectiveRotation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		endTime := f.scope.StartTime.Add(30 * time.Minute)
		projection := GetStProviderWalletsAt(t.Context(), endTime)[f.wallet.owner.ClientId]
		if projection == nil || projection.ColdkeyPubkey != f.coldkeys[1] {
			t.Fatal("fixture did not reproduce immediately changed future-wallet projection")
		}
		for index := range 2 {
			scope, authority := f.scope, f.authority
			if index == 1 {
				scope.Epoch, scope.Start, scope.End, scope.StartTime = 52, f.scope.End, payoutartifact.Boundary{Number: 530, Hash: common.Hash{53}.Hex()}, endTime
				authority.Epoch, authority.Start, authority.End = scope.Epoch, scope.Start, scope.End
			}
			wallets, err := GetStProviderWalletsForEpoch(t.Context(), f.approve(t, authority), f.expected, scope)
			if err != nil || len(wallets) != 1 {
				t.Fatal("epoch wallet selection failed", scope.Epoch, wallets, err)
			}
			wallet := wallets[f.wallet.owner.ClientId]
			if wallet == nil || wallet.ColdkeyPubkey != f.coldkeys[index] || wallet.NetworkId != f.wallet.owner.NetworkId || wallet.OriginalMessage == nil || *wallet.OriginalMessage != f.originals[index].Message || wallet.OriginalSignature == nil || *wallet.OriginalSignature != "0x"+hex.EncodeToString(f.originals[index].Signature[:]) || !wallet.SetTime.IsZero() {
				t.Fatal("payout selected another epoch or rebuilt original evidence", scope.Epoch, wallet)
			}
		}
	})
}

// Independently approved domain, epoch, boundary, signer and content pins
// cannot be borrowed from an otherwise valid original roster.
func TestStProviderWalletEpochRefusesChangedScopeAndRoster(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		raw := f.approve(t, f.authority)
		for index, change := range []func(*StProviderWalletEpochScope){func(s *StProviderWalletEpochScope) { s.Domain.NoID++ }, func(s *StProviderWalletEpochScope) { s.Epoch++ }, func(s *StProviderWalletEpochScope) { s.Start.Hash = common.Hash{91}.Hex() }, func(s *StProviderWalletEpochScope) { s.End.Number++ }} {
			scope := f.scope
			change(&scope)
			if wallets, err := GetStProviderWalletsForEpoch(t.Context(), raw, f.expected, scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("changed epoch scope borrowed roster", index, wallets, err)
			}
		}
		for index, change := range []func(*payoutartifact.WholeWorkExpectation){func(e *payoutartifact.WholeWorkExpectation) { e.AuthoritySigner = common.Address{92} }, func(e *payoutartifact.WholeWorkExpectation) { e.ClientKeyRootSigner = common.Address{93} }, func(e *payoutartifact.WholeWorkExpectation) { e.AuthorityHash = "sha256:" + strings.Repeat("94", 32) }} {
			expected := f.expected
			change(&expected)
			if wallets, err := GetStProviderWalletsForEpoch(t.Context(), raw, expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("changed independent approval borrowed roster", index, wallets, err)
			}
		}
		var changed payoutartifact.WholeWorkAuthority
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		changed.ExpectedProviders[0].WalletHeadHash = strings.Repeat("95", 32)
		forged, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if wallets, err := GetStProviderWalletsForEpoch(t.Context(), forged, f.expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("edited unsigned roster head selected a wallet", wallets, err)
		}
	})
}

// A valid roster signature does not excuse a foreign consent network, missing
// original chain or different head. No refusal may return a partial map.
func TestStProviderWalletEpochRefusesChangedConsentIdentityAndHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		for index, change := range []func(*payoutartifact.WholeWorkAuthority){
			func(a *payoutartifact.WholeWorkAuthority) {
				a.ExpectedProviders[0].WalletHeadHash = strings.Repeat("96", 32)
			},
			func(a *payoutartifact.WholeWorkAuthority) { a.ExpectedProviders[0].WalletGeneration = 3 },
			func(a *payoutartifact.WholeWorkAuthority) {
				a.Owners[0].NetworkId = [16]byte{97}
				a.ExpectedProviders[0].NetworkId = a.Owners[0].NetworkId
			},
			func(a *payoutartifact.WholeWorkAuthority) {
				a.Owners[0].ClientId = [16]byte{98}
				a.ExpectedProviders[0].ClientId = a.Owners[0].ClientId
			},
			func(a *payoutartifact.WholeWorkAuthority) {
				a.ExpectedProviders[0].WalletHeadHash = ""
				a.ExpectedProviders[0].WalletGeneration = 0
			},
			func(a *payoutartifact.WholeWorkAuthority) { a.ExpectedProviders = nil },
		} {
			authority := f.authority
			authority.Owners = append([]payoutartifact.WholeWorkOwner(nil), authority.Owners...)
			authority.ExpectedProviders = append([]payoutartifact.WholeWorkExpectedProvider(nil), authority.ExpectedProviders...)
			change(&authority)
			if wallets, err := GetStProviderWalletsForEpoch(t.Context(), f.approve(t, authority), f.expected, f.scope); wallets != nil || err == nil {
				t.Fatal("foreign or unavailable original history gained a payout wallet", index, wallets, err)
			}
		}
	})
}

// A later SQL head is outside an earlier independently approved checkpoint.
// The selected prefix stays authoritative even after its successor is active.
func TestStProviderWalletEpochDoesNotAdoptUnapprovedLaterHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		scope := f.scope
		scope.Epoch, scope.Start, scope.End, scope.StartTime = 52, f.scope.End, payoutartifact.Boundary{Number: 530, Hash: common.Hash{53}.Hex()}, scope.StartTime.Add(30*time.Minute)
		authority := f.authority
		authority.Epoch, authority.Start, authority.End = scope.Epoch, scope.Start, scope.End
		authority.ExpectedProviders[0].WalletHeadHash, authority.ExpectedProviders[0].WalletGeneration = hex.EncodeToString(f.heads[0][:]), 1
		wallets, err := GetStProviderWalletsForEpoch(t.Context(), f.approve(t, authority), f.expected, scope)
		if err != nil || len(wallets) != 1 || wallets[f.wallet.owner.ClientId].ColdkeyPubkey != f.coldkeys[0] {
			t.Fatal("later accepted SQL head replaced independent checkpoint", wallets, err)
		}
	})
}

// A future or expired effective interval cannot fall back to an account row;
// a missing pre-earning clock or contradictory issuance boundary also refuses.
func TestStProviderWalletEpochRefusesFutureExpiredAndUndatedConsent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		for index, change := range []func(*StProviderWalletEpochScope){func(s *StProviderWalletEpochScope) { s.Epoch = 50 }, func(s *StProviderWalletEpochScope) { s.Epoch = 153 }, func(s *StProviderWalletEpochScope) { s.StartTime = time.Unix(1, 0).UTC() }, func(s *StProviderWalletEpochScope) { s.Start.Number = 500 }} {
			scope, authority := f.scope, f.authority
			change(&scope)
			authority.Epoch, authority.Start, authority.End = scope.Epoch, scope.Start, scope.End
			if wallets, err := GetStProviderWalletsForEpoch(t.Context(), f.approve(t, authority), f.expected, scope); wallets != nil || err == nil {
				t.Fatal("unavailable or contradictory earning interval borrowed account projection", index, wallets, err)
			}
		}
	})
}

// Original v1 consent proves possession but supplies no prospective issuance
// authority; the accepted account projection cannot fill that missing proof.
func TestStProviderWalletEpochRefusesLegacyConsentAndMissingAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		legacy := newWalletMappingFixture(t)
		original := legacy.challenge(t, 0)
		accepted, err := AcceptWalletMappingConsent(t.Context(), legacy.owner, original, legacy.address)
		if err != nil || accepted == nil {
			t.Fatal(err)
		}
		authority := f.authority
		authority.Owners = []payoutartifact.WholeWorkOwner{{ClientId: [16]byte(legacy.owner.ClientId), NetworkId: [16]byte(legacy.owner.NetworkId), Generation: [16]byte{75}, PublicKey: [32]byte{76}}}
		authority.ExpectedProviders = []payoutartifact.WholeWorkExpectedProvider{{ClientId: [16]byte(legacy.owner.ClientId), NetworkId: [16]byte(legacy.owner.NetworkId), WalletHeadHash: hex.EncodeToString(accepted.OriginalHash[:]), WalletGeneration: 1}}
		for _, raw := range [][]byte{nil, f.approve(t, authority)} {
			if wallets, err := GetStProviderWalletsForEpoch(t.Context(), raw, f.expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
				t.Fatal("legacy or missing original consent gained epoch authority", wallets, err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if wallets, err := GetStProviderWalletsForEpoch(ctx, f.approve(t, f.authority), f.expected, f.scope); wallets != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled owner returned wallet authority", wallets, err)
		}
	})
}

// A retained hash and original-shaped row cannot excuse a bad coldkey
// signature. This corruption fixture models storage outside trusted intake.
func TestStProviderWalletEpochReverifiesOriginalConsentSignature(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newStProviderWalletEpochFixture(t)
		foreign := newWalletMappingFixture(t)
		original := foreign.challenge(t, 0)
		statement, err := protocol.DecodeWalletMappingStatement(original.Message)
		if err != nil {
			t.Fatal(err)
		}
		original.Signature[0] ^= 1
		raw, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		head := sha256.Sum256(raw)
		domain, err := foreign.owner.Domain.Digest()
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO wallet_mapping_consent(domain_hash,client_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, domain[:], foreign.owner.ClientId, 1, head[:], statement.Nonce[:], raw, server.NowUtc()))
		})
		authority := f.authority
		authority.Owners = []payoutartifact.WholeWorkOwner{{ClientId: [16]byte(foreign.owner.ClientId), NetworkId: [16]byte(foreign.owner.NetworkId), Generation: [16]byte{77}, PublicKey: [32]byte{78}}}
		authority.ExpectedProviders = []payoutartifact.WholeWorkExpectedProvider{{ClientId: [16]byte(foreign.owner.ClientId), NetworkId: [16]byte(foreign.owner.NetworkId), WalletHeadHash: hex.EncodeToString(head[:]), WalletGeneration: 1}}
		if wallets, err := GetStProviderWalletsForEpoch(t.Context(), f.approve(t, authority), f.expected, f.scope); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("unverified original-shaped SQL row gained payout authority", wallets, err)
		}
	})
}

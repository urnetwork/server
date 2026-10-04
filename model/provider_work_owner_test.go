// Real signed client-key history admits startup statements once; retained
// enrollment survives later rotation, cleanup and reopened model readers.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"syscall"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// The authenticated client and independent registration predate SDK enrollment.
func providerWorkOwnerFixture(t testing.TB) (StClientKeyRegistrationInput, protocol.OriginalWorkOwnerEnrollment, ed25519.PrivateKey, []byte) {
	t.Helper()
	input := newStClientKeyHistoryTestInput(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{121}, ed25519.SeedSize))
	input.PublicKey = bytes.Clone(key.Public().(ed25519.PublicKey))
	domain, err := input.Domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := protocol.SignOriginalWorkOwnerEnrollment(t.Context(), protocol.OriginalWorkOwnerEnrollment{DomainHash: domain, ClientId: [16]byte(input.ClientID), Generation: [16]byte(server.NewId())}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := owner.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return input, owner, key, raw
}

// Admission cannot invent a registration. Once committed, the same receipt and
// original admission bytes remain usable after key rotation and client cleanup.
func TestProviderWorkOwnerOriginalSurvivesRotationAndDeletion(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		input, owner, key, raw := providerWorkOwnerFixture(t)
		root := crypto.PubkeyToAddress(input.RootKey.PublicKey)
		if receipt, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); !errors.Is(err, ErrProviderWorkOwnerPending) || receipt.Schema != "" {
			t.Fatal("SDK signature invented original key registration", receipt, err)
		}
		registration, err := StoreStClientKeyRegistration(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		first, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root)
		if err != nil || first.Schema != protocol.OriginalWorkOwnerReceiptSchema || first.OwnerHash != sha256.Sum256(raw) {
			t.Fatal("original enrollment not committed", err)
		}
		input.PublicKey = bytes.Repeat([]byte{122}, ed25519.PublicKeySize)
		input.Boundary.Block++
		input.Boundary.Hash[0]++
		if _, err := StoreStClientKeyRegistration(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		if receipt, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); err != nil || receipt != first {
			t.Fatal("rotation revoked exact retained receipt", err)
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `DELETE FROM network_client WHERE client_id=$1`, input.ClientID))
		})
		if receipt, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); err != nil || receipt != first {
			t.Fatal("cleanup revoked exact retained receipt", err)
		}
		identity := ProviderWorkOwner{DomainHash: owner.DomainHash, ClientId: owner.ClientId, Generation: owner.Generation, PublicKey: owner.PublicKey}
		got, err := GetProviderWorkOwner(t.Context(), identity)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("reopened reader lost original enrollment", err)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var retainedRegistration []byte
			server.Raise(conn.QueryRow(t.Context(), `SELECT key_registration FROM provider_work_owner WHERE owner_hash=$1`, first.OwnerHash[:]).Scan(&retainedRegistration))
			if !bytes.Equal(retainedRegistration, registration.RegistrationBytes) {
				t.Fatal("original admission registration changed")
			}
		})
		owner.Generation = [16]byte(server.NewId())
		owner, err = protocol.SignOriginalWorkOwnerEnrollment(t.Context(), owner, key)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := owner.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkOwner(t.Context(), changed, owner.DomainHash, root); !errors.Is(err, ErrProviderWorkOwnerPending) {
			t.Fatal("retired client allocated a new SDK generation", err)
		}
	})
}

// A valid SDK signature is insufficient for a foreign registration root or
// different registered key, and a generation can never migrate to another key.
func TestProviderWorkOwnerRejectsForeignAuthorityAndGenerationReplacement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		input, owner, _, raw := providerWorkOwnerFixture(t)
		if _, err := StoreStClientKeyRegistration(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		root := crypto.PubkeyToAddress(input.RootKey.PublicKey)
		if _, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, common.Address{99}); !errors.Is(err, ErrProviderWorkConflict) {
			t.Fatal("SDK intake selected its own registration authority", err)
		}
		otherKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{123}, ed25519.SeedSize))
		foreign, err := protocol.SignOriginalWorkOwnerEnrollment(t.Context(), owner, otherKey)
		if err != nil {
			t.Fatal(err)
		}
		foreignRaw, err := foreign.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkOwner(t.Context(), foreignRaw, owner.DomainHash, root); !errors.Is(err, ErrProviderWorkInvalid) {
			t.Fatal("unregistered key consumed permanent enrollment", err)
		}
		if _, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); err != nil {
			t.Fatal(err)
		}
		input.PublicKey = bytes.Clone(otherKey.Public().(ed25519.PublicKey))
		input.Boundary.Block++
		input.Boundary.Hash[0]++
		if _, err := StoreStClientKeyRegistration(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkOwner(t.Context(), foreignRaw, owner.DomainHash, root); !errors.Is(err, ErrProviderWorkConflict) {
			t.Fatal("new registered key replaced original SDK generation", err)
		}
		identity := ProviderWorkOwner{DomainHash: owner.DomainHash, ClientId: owner.ClientId, Generation: owner.Generation, PublicKey: foreign.PublicKey}
		if _, err := GetProviderWorkOwner(t.Context(), identity); !errors.Is(err, ErrProviderWorkMissing) {
			t.Fatal("lookup crossed original public key", err)
		}
	})
}

// A synchronized burst of exact retry callers exercises real transaction
// serialization. Every caller must observe the same one-row durable receipt.
func TestProviderWorkOwnerConcurrentRetriesKeepOneOriginal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		input, owner, _, raw := providerWorkOwnerFixture(t)
		if _, err := StoreStClientKeyRegistration(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		root := crypto.PubkeyToAddress(input.RootKey.PublicKey)
		start := make(chan struct{})
		var wait sync.WaitGroup
		receipts := make([]protocol.OriginalWorkOwnerReceipt, 8)
		failures := make([]error, len(receipts))
		for index := range receipts {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				receipts[index], failures[index] = RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root)
			}()
		}
		close(start)
		wait.Wait()
		for index, receipt := range receipts {
			if failures[index] != nil || receipt.OwnerHash != sha256.Sum256(raw) || receipt.Schema != protocol.OriginalWorkOwnerReceiptSchema {
				t.Fatal("concurrent enrollment changed receipt", failures[index])
			}
		}
		index, err := ListProviderWorkOwners(t.Context(), ProviderWorkOwner{DomainHash: owner.DomainHash, ClientId: owner.ClientId, PublicKey: owner.PublicKey})
		if err != nil || len(index.Owners) != 1 || !bytes.Equal(index.Owners[0], raw) {
			t.Fatal("concurrent enrollment changed retained index", err)
		}
	})
}

// Legitimate retained original rows fill the lifetime allowance, including
// earlier SDK lifecycles. Replays do not consume or renew that allowance.
func TestProviderWorkOwnerLifetimeBoundAndSqlCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		input, owner, key, raw := providerWorkOwnerFixture(t)
		registration, err := StoreStClientKeyRegistration(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		root := crypto.PubkeyToAddress(input.RootKey.PublicKey)
		if _, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); err != nil {
			t.Fatal(err)
		}
		rows := make([][]any, 0, MaximumProviderWorkOwnersPerClient-1)
		for value := 1; value < MaximumProviderWorkOwnersPerClient; value++ {
			generation := [16]byte{0xf1}
			binary.BigEndian.PutUint64(generation[8:], uint64(value))
			other := owner
			other.Generation = generation
			other, err = protocol.SignOriginalWorkOwnerEnrollment(t.Context(), other, key)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := other.Bytes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encoded)
			rows = append(rows, []any{bytes.Clone(owner.DomainHash[:]), bytes.Clone(owner.ClientId[:]), bytes.Clone(generation[:]), bytes.Clone(owner.PublicKey[:]), bytes.Clone(digest[:]), encoded, registration.RegistrationBytes})
		}
		server.Tx(t.Context(), func(tx server.PgTx) {
			count, err := tx.CopyFrom(t.Context(), pgx.Identifier{"provider_work_owner"}, []string{"domain_hash", "client_id", "generation", "public_key", "owner_hash", "original", "key_registration"}, pgx.CopyFromRows(rows))
			server.Raise(err)
			if count != int64(len(rows)) {
				t.Fatal("fixture lost retained original rows")
			}
		})
		if _, err := RetainProviderWorkOwner(t.Context(), raw, owner.DomainHash, root); err != nil {
			t.Fatal("full allowance refused exact original retry", err)
		}
		owner.Generation = [16]byte(server.NewId())
		owner, err = protocol.SignOriginalWorkOwnerEnrollment(t.Context(), owner, key)
		if err != nil {
			t.Fatal(err)
		}
		extra, err := owner.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkOwner(t.Context(), extra, owner.DomainHash, root); !errors.Is(err, ErrProviderWorkCapacity) {
			t.Fatal("new lifecycle renewed lifetime storage allowance", err)
		}
		index, err := ListProviderWorkOwners(t.Context(), ProviderWorkOwner{DomainHash: owner.DomainHash, ClientId: owner.ClientId, PublicKey: owner.PublicKey})
		if err != nil || len(index.Owners) != MaximumProviderWorkOwnersPerClient {
			t.Fatal("full index silently truncated retained originals", err)
		}
		for _, statement := range []string{`UPDATE provider_work_owner SET original=original`, `DELETE FROM provider_work_owner`, `TRUNCATE provider_work_owner`} {
			if recovered := server.HandleError(func() {
				server.Tx(t.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(t.Context(), statement)) })
			}); recovered == nil {
				t.Fatal("SDK original SQL custody mutated", statement)
			}
		}
	})
}

// Cancellation preserves its actual owner cause before any database admission;
// malformed canonical encoding remains a separate hard refusal.
func TestProviderWorkOwnerCancellationAndCanonicalBoundary(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{124}, ed25519.SeedSize))
	owner, err := protocol.SignOriginalWorkOwnerEnrollment(t.Context(), protocol.OriginalWorkOwnerEnrollment{DomainHash: [32]byte{1}, ClientId: [16]byte{2}, Generation: [16]byte{3}}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := owner.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(syscall.EIO)
	if receipt, err := RetainProviderWorkOwner(ctx, raw, owner.DomainHash, common.Address{4}); !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || errors.Is(err, ErrProviderWorkInvalid) || receipt.Schema != "" {
		t.Fatal("SDK enrollment lost cancellation owner", receipt, err)
	}
	if _, err := RetainProviderWorkOwner(t.Context(), append(bytes.Clone(raw), '\n'), owner.DomainHash, common.Address{4}); !errors.Is(err, ErrProviderWorkInvalid) {
		t.Fatal("SDK enrollment accepted alternate original spelling", err)
	}
}

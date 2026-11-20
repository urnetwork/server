// Real SQL generations and immutable publication reproduce rotation, restart
// and deletion behavior; no detached verifier result stands in for storage.
package model

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

// Authenticated client identity exists independently of its key-history rows.
func newStClientKeyHistoryTestInput(t testing.TB) StClientKeyRegistrationInput {
	t.Helper()
	clientID, networkID, userID, deviceID := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	Testing_CreateNetwork(t.Context(), networkID, "key-history-"+networkID.String(), userID)
	Testing_CreateDevice(t.Context(), networkID, deviceID, clientID, "key-history", "test")
	return newStClientKeyHistoryUnitTestInput(t, clientID)
}

// The signed registration inputs alone, for checks that need no rows.
func newStClientKeyHistoryUnitTestInput(t testing.TB, clientID server.Id) StClientKeyRegistrationInput {
	t.Helper()
	root, err := crypto.HexToECDSA(strings.Repeat("12", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := crypto.HexToECDSA(strings.Repeat("13", 32))
	if err != nil {
		t.Fatal(err)
	}
	deployment := "key-history-test"
	return StClientKeyRegistrationInput{Domain: protocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte(deployment)), PolicyHash: [32]byte{4}, NoID: 1}, DeploymentID: deployment, ClientID: clientID, PublicKey: bytes.Repeat([]byte{9}, 32), Boundary: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{5}}, RootKey: root, ArtifactKey: artifact, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
}

// Seals one generation exactly as StoreStClientKeyRegistration does, without
// a database, so stored row shapes can be built and decoded directly.
func sealStClientKeyHistoryTestRecord(t testing.TB, input StClientKeyRegistrationInput, networkID server.Id, prior *protocol.ClientKeyRegistration) (StClientKeyHistoryRecord, [32]byte) {
	t.Helper()
	registration := protocol.ClientKeyRegistration{Domain: input.Domain, ClientID: [16]byte(input.ClientID), NetworkID: [16]byte(networkID), Generation: 1, Present: len(input.PublicKey) != 0, EffectiveBoundary: input.Boundary}
	copy(registration.PublicKey[:], input.PublicKey)
	if prior != nil {
		previousHash, err := prior.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		registration.Generation = prior.Generation + 1
		registration.PreviousHash = previousHash
	}
	if err := protocol.SignClientKeyRegistration(&registration, input.RootKey); err != nil {
		t.Fatal(err)
	}
	if err := registration.Follows(prior); err != nil {
		t.Fatal(err)
	}
	registrationBytes, err := registration.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	evidenceBytes, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(input.DeploymentID, registration, input.ArtifactKey, input.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	return StClientKeyHistoryRecord{Registration: registration, RegistrationBytes: registrationBytes, EvidenceHash: evidenceHash, EvidenceBytes: evidenceBytes}, sha256.Sum256(registrationBytes)
}

// Installs one sealed generation as its current head the way rows look once
// writers stop filling the registration column: only its hash and evidence.
func insertStClientKeyHistoryTestRecordWithoutRegistration(t testing.TB, input StClientKeyRegistrationInput, prior *protocol.ClientKeyRegistration) StClientKeyHistoryRecord {
	t.Helper()
	domainHash, err := input.Domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var networkID server.Id
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT network_id FROM network_client WHERE client_id = $1`, input.ClientID).Scan(&networkID))
	})
	record, registrationHash := sealStClientKeyHistoryTestRecord(t, input, networkID, prior)
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, input.ClientID, record.Registration.Generation, domainHash[:], registrationHash[:], record.EvidenceHash, record.EvidenceBytes))
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation, retired)
			VALUES ($1, $2, $3, $4, false)
			ON CONFLICT (client_id, domain_hash) DO UPDATE SET generation = EXCLUDED.generation
		`, input.ClientID, domainHash[:], networkID, record.Registration.Generation))
	})
	return record
}

// Retry identity includes the original complete wrapper, not fresh timestamps.
func TestStClientKeyHistoryDurableRotationTombstoneAndExactRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		initial, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil {
			tb.Fatal(err)
		}
		original := bytes.Clone(initial.EvidenceBytes)
		input.CreatedAt = input.CreatedAt.Add(time.Hour)
		retry, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil || !bytes.Equal(retry.EvidenceBytes, original) {
			tb.Fatalf("retry changed exact durable bytes: %v", err)
		}
		for index, key := range [][]byte{bytes.Repeat([]byte{10}, 32), nil, bytes.Repeat([]byte{11}, 32)} {
			input.PublicKey = key
			input.Boundary.Block++
			input.Boundary.Hash = [32]byte{byte(20 + index)}
			record, err := StoreStClientKeyRegistration(tb.Context(), input)
			if err != nil || record.Registration.Generation != uint64(index+2) {
				tb.Fatalf("actual transition %d: %v", index, err)
			}
			current, err := GetClientPublicKey(tb.Context(), input.ClientID)
			if err != nil || !bytes.Equal(current, key) {
				tb.Fatalf("unsigned current projection diverged: %v", err)
			}
			projected := map[server.Id][32]byte{input.ClientID: {99}}
			overlayStClientKeyCurrent(tb.Context(), []server.Id{input.ClientID}, projected)
			value, found := projected[input.ClientID]
			if found != (len(key) > 0) || found && value != [32]byte(key) {
				tb.Fatal("epoch-close projection retained a stale Redis key")
			}
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 4, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 4 || !bytes.Equal(history[0].EvidenceBytes, original) {
			tb.Fatalf("fresh SQL reader lost history: %v", err)
		}
		store := server.NewLocalBlobStore(tb.TempDir(), "key-history")
		for _, record := range history {
			published, err := startifact.PublishClientKeyEvidence(tb.Context(), store, record.EvidenceBytes, record.EvidenceHash)
			if err != nil {
				tb.Fatal(err)
			}
			body, err := store.Get(tb.Context(), published.ContentKey)
			if err != nil {
				tb.Fatal(err)
			}
			encoded, readErr := io.ReadAll(body)
			if err := errors.Join(readErr, body.Close()); err != nil || !bytes.Equal(encoded, record.EvidenceBytes) {
				tb.Fatalf("real immutable readback differs: %v", err)
			}
		}
	})
}

// The network-client deletion trigger retires current access atomically and
// never manufactures an operator signature for a cleanup operation.
func TestStClientKeyHistoryClientDeletionRetainsUnsignedAbsence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		initial, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil {
			tb.Fatal(err)
		}
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "DELETE FROM network_client WHERE client_id = $1", input.ClientID))
		})
		current, err := GetClientPublicKey(tb.Context(), input.ClientID)
		if err != nil || len(current) != 0 {
			tb.Fatalf("deleted client retained current key: %v", err)
		}
		if _, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 4, MaxStClientKeyHistoryBytes); err == nil {
			tb.Fatal("reaped client produced a positive current history")
		}
		if _, err := StoreStClientKeyRegistration(tb.Context(), input); err == nil {
			tb.Fatal("deleted client could append a generation")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var count int64
			var encoded []byte
			server.Raise(conn.QueryRow(tb.Context(), "SELECT COUNT(*) FROM st_client_key_history WHERE client_id=$1", input.ClientID).Scan(&count))
			server.Raise(conn.QueryRow(tb.Context(), "SELECT evidence FROM st_client_key_history WHERE client_id=$1 AND generation=1", input.ClientID).Scan(&encoded))
			if count != 1 || !bytes.Equal(encoded, initial.EvidenceBytes) {
				tb.Fatal("client cleanup erased history or invented a signed tombstone")
			}
		})
	})
}

// Identical concurrent authenticated retries serialize through the real
// client-row lock and produce one generation, including the first insertion.
func TestStClientKeyHistoryConcurrentFirstRetriesHaveOneSQLGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		start := make(chan struct{})
		results := make(chan *StClientKeyHistoryRecord, 8)
		failures := make(chan error, 8)
		var wait sync.WaitGroup
		for range 8 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := StoreStClientKeyRegistration(tb.Context(), input)
				results <- result
				failures <- err
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(failures)
		for err := range failures {
			if err != nil {
				tb.Fatal(err)
			}
		}
		var first []byte
		for result := range results {
			if result == nil || result.Registration.Generation != 1 {
				tb.Fatal("concurrent retry allocated another generation")
			}
			if first == nil {
				first = result.EvidenceBytes
			}
			if !bytes.Equal(first, result.EvidenceBytes) {
				tb.Fatal("concurrent retry returned different signed bytes")
			}
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 1, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 1 {
			tb.Fatalf("real SQL generation count differs: %v", err)
		}
	})
}

// Finite bounds and an immutable domain apply before a partial result can
// reach any current/history consumer, even with authentic stored signatures.
func TestStClientKeyHistoryRejectsBoundsDomainAndBoundaryRollback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		if _, err := StoreStClientKeyRegistration(tb.Context(), input); err != nil {
			tb.Fatal(err)
		}
		if history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 1, 1); err == nil || history != nil {
			tb.Fatal("tiny byte allowance returned a partial history")
		}
		foreign := input.Domain
		foreign.NoID++
		if history, err := LoadStClientKeyHistory(tb.Context(), foreign, input.ClientID, 1, MaxStClientKeyHistoryBytes); err == nil || history != nil {
			tb.Fatal("wrong domain reached stored history")
		}
		input.PublicKey = bytes.Repeat([]byte{10}, 32)
		input.Boundary.Block--
		if _, err := StoreStClientKeyRegistration(tb.Context(), input); err == nil {
			tb.Fatal("signed generation rolled back the effective boundary")
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 1, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 1 {
			tb.Fatalf("failed transition partly committed: %v", err)
		}
	})
}

// Re-signs evidence after one field change, so each decoder check sees a
// wrapper that is otherwise exactly what a writer would produce.
func changeStClientKeyHistoryTestEvidence(t testing.TB, evidence []byte, signer *ecdsa.PrivateKey, change func(*startifact.EvidenceEnvelope)) ([]byte, string) {
	t.Helper()
	var envelope startifact.EvidenceEnvelope
	if err := json.Unmarshal(evidence, &envelope); err != nil {
		t.Fatal(err)
	}
	change(&envelope)
	if err := startifact.SignEvidence(&envelope, signer); err != nil {
		t.Fatal(err)
	}
	encoded, err := startifact.EvidenceBytes(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, envelope.ContentHash
}

// The evidence payload is the exact signed registration, so a row's hash and
// evidence alone reproduce the record its writer returned.
func TestStClientKeyHistoryRecordDecodesRegistrationFromEvidencePayload(t *testing.T) {
	input := newStClientKeyHistoryUnitTestInput(t, server.NewId())
	sealed, registrationHash := sealStClientKeyHistoryTestRecord(t, input, server.NewId(), nil)
	record, err := decodeStClientKeyHistoryRecord(registrationHash[:], sealed.EvidenceBytes, sealed.EvidenceHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(record.RegistrationBytes, sealed.RegistrationBytes) || record.Registration != sealed.Registration || !bytes.Equal(record.EvidenceBytes, sealed.EvidenceBytes) || record.EvidenceHash != sealed.EvidenceHash {
		t.Fatal("decoded record differs from the record its writer sealed")
	}
	next := input
	next.PublicKey = bytes.Repeat([]byte{10}, 32)
	next.Boundary.Block++
	next.Boundary.Hash = [32]byte{6}
	rotated, rotatedHash := sealStClientKeyHistoryTestRecord(t, next, server.Id(sealed.Registration.NetworkID), &sealed.Registration)
	decoded, err := decodeStClientKeyHistoryRecord(rotatedHash[:], rotated.EvidenceBytes, rotated.EvidenceHash)
	if err != nil || !bytes.Equal(decoded.RegistrationBytes, rotated.RegistrationBytes) || decoded.Registration.Follows(&record.Registration) != nil {
		t.Fatalf("decoded rotation lost its signed link: %v", err)
	}
}

// The registration hash binds the row to the payload its evidence carries; a
// validly signed wrapper around another registration is refused.
func TestStClientKeyHistoryRecordRejectsPayloadOutsideItsRegistrationHash(t *testing.T) {
	input := newStClientKeyHistoryUnitTestInput(t, server.NewId())
	sealed, registrationHash := sealStClientKeyHistoryTestRecord(t, input, server.NewId(), nil)
	other := input
	other.PublicKey = bytes.Repeat([]byte{11}, 32)
	foreign, foreignHash := sealStClientKeyHistoryTestRecord(t, other, server.Id(sealed.Registration.NetworkID), nil)
	swapped, swappedHash := changeStClientKeyHistoryTestEvidence(t, sealed.EvidenceBytes, input.ArtifactKey, func(envelope *startifact.EvidenceEnvelope) {
		envelope.Payload = bytes.Clone(foreign.RegistrationBytes)
	})
	if record, err := decodeStClientKeyHistoryRecord(foreignHash[:], swapped, swappedHash); err != nil || !bytes.Equal(record.RegistrationBytes, foreign.RegistrationBytes) {
		t.Fatalf("the swapped wrapper is not otherwise valid, so the refusal below proves nothing: %v", err)
	}
	if _, err := decodeStClientKeyHistoryRecord(registrationHash[:], swapped, swappedHash); err == nil {
		t.Fatal("a signed wrapper carrying another registration was admitted under this row's hash")
	}
	if _, err := decodeStClientKeyHistoryRecord(foreignHash[:], sealed.EvidenceBytes, sealed.EvidenceHash); err == nil {
		t.Fatal("a row hash naming another registration admitted this evidence")
	}
}

// Every wrapper identity check that compared the old column copy still holds.
// Each case is re-signed, so only the check it names can refuse it.
func TestStClientKeyHistoryRecordRejectsMismatchedEvidenceWrapper(t *testing.T) {
	input := newStClientKeyHistoryUnitTestInput(t, server.NewId())
	sealed, registrationHash := sealStClientKeyHistoryTestRecord(t, input, server.NewId(), nil)
	type evidenceCase struct {
		evidence     []byte
		evidenceHash string
	}
	resign := func(change func(*startifact.EvidenceEnvelope)) evidenceCase {
		evidence, evidenceHash := changeStClientKeyHistoryTestEvidence(t, sealed.EvidenceBytes, input.ArtifactKey, change)
		return evidenceCase{evidence: evidence, evidenceHash: evidenceHash}
	}
	unchanged := resign(func(*startifact.EvidenceEnvelope) {})
	if _, err := decodeStClientKeyHistoryRecord(registrationHash[:], unchanged.evidence, unchanged.evidenceHash); err != nil {
		t.Fatalf("re-signing alone changed the decoder's verdict: %v", err)
	}
	var forged startifact.EvidenceEnvelope
	if err := json.Unmarshal(sealed.EvidenceBytes, &forged); err != nil {
		t.Fatal(err)
	}
	forged.ContentHash = "sha256:" + strings.Repeat("cd", 32)
	forgedBytes, err := json.Marshal(&forged)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]evidenceCase{
		"empty":        {nil, sealed.EvidenceHash},
		"oversized":    {bytes.Repeat([]byte{' '}, startifact.MaxClientKeyEvidenceBytes+1), sealed.EvidenceHash},
		"malformed":    {[]byte("malformed"), sealed.EvidenceHash},
		"noncanonical": {append(bytes.Clone(sealed.EvidenceBytes), '\n'), sealed.EvidenceHash},
		"content hash": {forgedBytes, forged.ContentHash},
		"row hash":     {sealed.EvidenceBytes, "sha256:" + strings.Repeat("ef", 32)},
		"kind": resign(func(envelope *startifact.EvidenceEnvelope) {
			envelope.Kind = startifact.ClientKeyObservationEvidenceKind
		}),
		"run":        resign(func(envelope *startifact.EvidenceEnvelope) { envelope.RunID = "synthetic-run" }),
		"chain":      resign(func(envelope *startifact.EvidenceEnvelope) { envelope.ChainID++ }),
		"netuid":     resign(func(envelope *startifact.EvidenceEnvelope) { envelope.Netuid++ }),
		"genesis":    resign(func(envelope *startifact.EvidenceEnvelope) { envelope.GenesisHash = "0x" + strings.Repeat("ab", 32) }),
		"deployment": resign(func(envelope *startifact.EvidenceEnvelope) { envelope.DeploymentID = "another-deployment" }),
	}
	for name, value := range cases {
		if _, err := decodeStClientKeyHistoryRecord(registrationHash[:], value.evidence, value.evidenceHash); err == nil {
			t.Errorf("%s evidence wrapper was admitted", name)
		}
	}
}

// The per-generation allowance is the formula the census computed in SQL from
// both stored columns, now taken from the decoded payload length.
func TestStClientKeyHistoryAllowanceMatchesStoredColumnFormula(t *testing.T) {
	input := newStClientKeyHistoryUnitTestInput(t, server.NewId())
	sealed, registrationHash := sealStClientKeyHistoryTestRecord(t, input, server.NewId(), nil)
	record, err := decodeStClientKeyHistoryRecord(registrationHash[:], sealed.EvidenceBytes, sealed.EvidenceHash)
	if err != nil {
		t.Fatal(err)
	}
	control := stClientKeyHistoryControlBytes()
	stored := 6*int64(len(sealed.RegistrationBytes)) + 6*int64(len(sealed.EvidenceBytes)) + 2*int64(len(sealed.EvidenceHash)) + int64(len(registrationHash)) + control
	if got := stClientKeyHistoryRecordAllowanceBytes(&record, registrationHash[:], control); got != stored {
		t.Fatalf("allowance %d differs from the stored-column census %d", got, stored)
	}
}

// Rows written without the registration column serve every current reader and
// the exact historical read from their evidence alone.
func TestStClientKeyHistoryReadersDeriveRegistrationWithoutColumn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		record := insertStClientKeyHistoryTestRecordWithoutRegistration(tb, input, nil)
		key, err := GetClientPublicKey(tb.Context(), input.ClientID)
		if err != nil || !bytes.Equal(key, input.PublicKey) {
			tb.Fatalf("current key lookup did not derive its signed registration: %v", err)
		}
		projected := map[server.Id][32]byte{input.ClientID: {99}}
		overlayStClientKeyCurrent(tb.Context(), []server.Id{input.ClientID}, projected)
		if projected[input.ClientID] != [32]byte(input.PublicKey) {
			tb.Fatal("epoch-close projection did not derive its signed registration")
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 1, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 1 || !bytes.Equal(history[0].RegistrationBytes, record.RegistrationBytes) || !bytes.Equal(history[0].EvidenceBytes, record.EvidenceBytes) {
			tb.Fatalf("historical read did not return the exact signed bytes: %v", err)
		}
	})
}

// The byte allowance admits exactly what the stored-column census admitted: a
// history that uses the whole allowance loads, one byte less refuses it.
func TestStClientKeyHistoryAllowanceBoundaryWithoutRegistrationColumn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		first := insertStClientKeyHistoryTestRecordWithoutRegistration(tb, input, nil)
		next := input
		next.PublicKey = bytes.Repeat([]byte{10}, 32)
		next.Boundary.Block++
		next.Boundary.Hash = [32]byte{6}
		second := insertStClientKeyHistoryTestRecordWithoutRegistration(tb, next, &first.Registration)
		control := stClientKeyHistoryControlBytes()
		var exact int64
		for _, record := range []StClientKeyHistoryRecord{first, second} {
			hash := sha256.Sum256(record.RegistrationBytes)
			exact += 6*int64(len(record.RegistrationBytes)) + 6*int64(len(record.EvidenceBytes)) + 2*int64(len(record.EvidenceHash)) + int64(len(hash)) + control
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 2, uint64(exact))
		if err != nil || len(history) != 2 {
			tb.Fatalf("history using its whole allowance was refused: %v", err)
		}
		if history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 2, uint64(exact-1)); err == nil || history != nil {
			tb.Fatal("history one byte over its allowance was admitted")
		}
	})
}

// A head without the registration column still anchors exact retries and the
// next signed generation's link.
func TestStClientKeyRegistrationRotatesFromHeadWithoutRegistration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		first := insertStClientKeyHistoryTestRecordWithoutRegistration(tb, input, nil)
		retry, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil || !bytes.Equal(retry.RegistrationBytes, first.RegistrationBytes) || !bytes.Equal(retry.EvidenceBytes, first.EvidenceBytes) {
			tb.Fatalf("exact retry did not reuse the derived head: %v", err)
		}
		input.PublicKey = bytes.Repeat([]byte{10}, 32)
		input.Boundary.Block++
		input.Boundary.Hash = [32]byte{6}
		rotated, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil || rotated.Registration.Generation != 2 {
			tb.Fatalf("rotation from a derived head failed: %v", err)
		}
		previousHash, err := first.Registration.ContentHash()
		if err != nil || rotated.Registration.PreviousHash != previousHash {
			tb.Fatalf("rotation did not link the derived head: %v", err)
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 2, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 2 || !bytes.Equal(history[0].RegistrationBytes, first.RegistrationBytes) {
			tb.Fatalf("mixed history did not read back: %v", err)
		}
	})
}

// Writers keep only the registration's hash beside its evidence: the stored
// row has no registration copy, and every reader still recovers the exact
// signed bytes from the evidence payload, for a first generation and a rotation.
func TestStClientKeyRegistrationStoresNoRegistrationCopy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		first, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil {
			tb.Fatal(err)
		}
		input.PublicKey = bytes.Repeat([]byte{10}, 32)
		input.Boundary.Block++
		input.Boundary.Hash = [32]byte{6}
		rotated, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil || rotated.Registration.Generation != 2 {
			tb.Fatalf("rotation from a stored head without a registration copy failed: %v", err)
		}
		for _, record := range []*StClientKeyHistoryRecord{first, rotated} {
			var copied bool
			var registrationHash []byte
			server.Db(tb.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(tb.Context(), `SELECT registration IS NOT NULL, registration_hash FROM st_client_key_history WHERE client_id = $1 AND generation = $2`, input.ClientID, int64(record.Registration.Generation)).Scan(&copied, &registrationHash))
			})
			wantHash := sha256.Sum256(record.RegistrationBytes)
			if copied || !bytes.Equal(registrationHash, wantHash[:]) {
				tb.Fatalf("generation %d stored a registration copy or a different hash", record.Registration.Generation)
			}
		}
		current, err := GetClientPublicKey(tb.Context(), input.ClientID)
		if err != nil || !bytes.Equal(current, input.PublicKey) {
			tb.Fatalf("current key did not derive from the stored evidence: %v", err)
		}
		history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 2, MaxStClientKeyHistoryBytes)
		if err != nil || len(history) != 2 || !bytes.Equal(history[0].RegistrationBytes, first.RegistrationBytes) || !bytes.Equal(history[1].RegistrationBytes, rotated.RegistrationBytes) {
			tb.Fatalf("history did not derive both generations from their evidence: %v", err)
		}
	})
}

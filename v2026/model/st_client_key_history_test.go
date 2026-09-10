// Real SQL generations and immutable publication reproduce rotation, restart
// and deletion behavior; no detached verifier result stands in for storage.
package model

import (
	"bytes"
	"crypto/sha256"
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

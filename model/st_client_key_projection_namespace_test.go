// Two signed policy namespaces can share a client and generation. Current
// projection and exact historical reads must retain the complete routing key.
package model

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// Install authentic immutable records independently of the mutation path so
// an older reader can be checked against the already-migrated live schema.
func newStClientKeyProjectionNamespaceFixture(t testing.TB, currentPresent bool) (StClientKeyRegistrationInput, StClientKeyRegistrationInput, []StClientKeyHistoryRecord) {
	t.Helper()
	old := newStClientKeyHistoryTestInput(t)
	current := old
	current.Domain.PolicyHash = [32]byte{7}
	current.PublicKey = bytes.Repeat([]byte{10}, 32)
	current.Boundary.Block++
	current.Boundary.Hash = [32]byte{8}
	if !currentPresent {
		current.PublicKey = nil
	}
	records := make([]StClientKeyHistoryRecord, 0, 2)
	server.Tx(t.Context(), func(tx server.PgTx) {
		var networkId server.Id
		server.Raise(tx.QueryRow(t.Context(), "SELECT network_id FROM network_client WHERE client_id = $1", old.ClientID).Scan(&networkId))
		for index, input := range []StClientKeyRegistrationInput{old, current} {
			domainHash, err := input.Domain.Digest()
			server.Raise(err)
			registration := protocol.ClientKeyRegistration{Domain: input.Domain, ClientID: [16]byte(input.ClientID), NetworkID: [16]byte(networkId), Generation: 1, Present: len(input.PublicKey) != 0, EffectiveBoundary: input.Boundary}
			copy(registration.PublicKey[:], input.PublicKey)
			server.Raise(protocol.SignClientKeyRegistration(&registration, input.RootKey))
			registrationBytes, err := registration.Bytes()
			server.Raise(err)
			registrationHash := sha256.Sum256(registrationBytes)
			evidenceBytes, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(input.DeploymentID, registration, input.ArtifactKey, input.CreatedAt)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO st_client_key_history (client_id, domain_hash, generation, registration_hash, registration, evidence_hash, evidence) VALUES ($1, $2, 1, $3, $4, $5, $6)`, input.ClientID, domainHash[:], registrationHash[:], registrationBytes, evidenceHash, evidenceBytes))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation, retired, is_current) VALUES ($1, $2, $3, 1, false, $4)`, input.ClientID, domainHash[:], networkId, index == 1))
			records = append(records, StClientKeyHistoryRecord{Registration: registration, RegistrationBytes: registrationBytes, EvidenceHash: evidenceHash, EvidenceBytes: evidenceBytes})
		}
	})
	return old, current, records
}

// Public current lookup and epoch-close batch projection select the current
// domain even when both policies have the same client and generation one.
func TestStClientKeyCurrentProjectionSeparatesPolicyNamespaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, current, _ := newStClientKeyProjectionNamespaceFixture(tb, true)
		key, err := GetClientPublicKey(tb.Context(), current.ClientID)
		if err != nil || !bytes.Equal(key, current.PublicKey) {
			tb.Errorf("current API did not select its exact signed policy head: %v", err)
		}
		projected := map[server.Id][32]byte{current.ClientID: {99}}
		overlayStClientKeyCurrent(tb.Context(), []server.Id{current.ClientID}, projected)
		if len(projected) != 1 || projected[current.ClientID] != [32]byte(current.PublicKey) {
			tb.Fatal("epoch-close projection mixed current and archived namespaces")
		}
	})
}

// Independent history requests retain the exact original bytes from each
// policy; neither another head nor another policy's census is admissible.
func TestStClientKeyHistoricalReadSelectsPolicyNamespace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		old, current, records := newStClientKeyProjectionNamespaceFixture(tb, true)
		for index, input := range []StClientKeyRegistrationInput{old, current} {
			history, err := LoadStClientKeyHistory(tb.Context(), input.Domain, input.ClientID, 1, MaxStClientKeyHistoryBytes)
			if err != nil || len(history) != 1 || !bytes.Equal(history[0].EvidenceBytes, records[index].EvidenceBytes) {
				tb.Fatalf("policy %d did not retain its exact signed history: %v", index, err)
			}
		}
	})
}

// A positive archived policy cannot resurrect a current signed tombstone
// through either the API projection or the batched epoch-close overlay.
func TestStClientKeyPolicyProjectionRetainsCurrentTombstone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		old, current, _ := newStClientKeyProjectionNamespaceFixture(tb, false)
		key, err := GetClientPublicKey(tb.Context(), current.ClientID)
		if err != nil || len(key) != 0 {
			tb.Errorf("archived policy resurrected a current tombstone: %v", err)
		}
		projected := map[server.Id][32]byte{current.ClientID: [32]byte(old.PublicKey)}
		overlayStClientKeyCurrent(tb.Context(), []server.Id{current.ClientID}, projected)
		if len(projected) != 0 {
			tb.Fatal("epoch-close overlay retained an archived positive key")
		}
	})
}

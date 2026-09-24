// Policy rollover exercises signed client-key histories against disposable
// SQL and Redis state without rewriting an older policy's retained evidence.
package model

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A policy is a signing namespace. A new policy starts at generation one,
// while the prior namespace and its exact signed evidence remain readable.
func TestStClientKeyPolicyRolloverPreservesBothSignedHistories(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		old := newStClientKeyHistoryTestInput(tb)
		first, err := StoreStClientKeyRegistration(tb.Context(), old)
		if err != nil {
			tb.Fatal(err)
		}
		old.PublicKey = bytes.Repeat([]byte{10}, 32)
		old.Boundary.Block++
		old.Boundary.Hash = [32]byte{6}
		second, err := StoreStClientKeyRegistration(tb.Context(), old)
		if err != nil {
			tb.Fatal(err)
		}

		current := old
		current.Domain.PolicyHash = [32]byte{7}
		current.Boundary.Block++
		current.Boundary.Hash = [32]byte{8}
		rollover, err := StoreStClientKeyRegistration(tb.Context(), current)
		if err != nil || rollover.Registration.Generation != 1 || rollover.Registration.PreviousHash != ([32]byte{}) {
			tb.Fatalf("policy rollover did not start an independent signed history: %v", err)
		}
		beforeRetry := bytes.Clone(rollover.EvidenceBytes)
		current.CreatedAt = current.CreatedAt.Add(time.Hour)
		retry, err := StoreStClientKeyRegistration(tb.Context(), current)
		if err != nil || !bytes.Equal(retry.EvidenceBytes, beforeRetry) {
			tb.Fatalf("rollover retry changed signed evidence: %v", err)
		}
		oldHistory, err := LoadStClientKeyHistory(tb.Context(), old.Domain, old.ClientID, 2, MaxStClientKeyHistoryBytes)
		if err != nil || len(oldHistory) != 2 || !bytes.Equal(oldHistory[0].EvidenceBytes, first.EvidenceBytes) || !bytes.Equal(oldHistory[1].EvidenceBytes, second.EvidenceBytes) {
			tb.Fatalf("archived policy history changed: %v", err)
		}
		newHistory, err := LoadStClientKeyHistory(tb.Context(), current.Domain, current.ClientID, 1, MaxStClientKeyHistoryBytes)
		if err != nil || len(newHistory) != 1 || !bytes.Equal(newHistory[0].EvidenceBytes, beforeRetry) {
			tb.Fatalf("current policy history is not exact: %v", err)
		}
		key, err := GetClientPublicKey(tb.Context(), current.ClientID)
		if err != nil || !bytes.Equal(key, current.PublicKey) {
			tb.Fatalf("current key did not use the active policy head: %v", err)
		}
		projected := map[server.Id][32]byte{current.ClientID: {99}}
		overlayStClientKeyCurrent(tb.Context(), []server.Id{current.ClientID}, projected)
		if projected[current.ClientID] != [32]byte(current.PublicKey) {
			tb.Fatal("epoch-close projection did not use the active policy head")
		}
		current.PublicKey = nil
		current.Boundary.Block++
		current.Boundary.Hash = [32]byte{9}
		tombstone, err := StoreStClientKeyRegistration(tb.Context(), current)
		if err != nil || tombstone.Registration.Generation != 2 {
			tb.Fatalf("current policy could not append a signed tombstone: %v", err)
		}
		newHistory, err = LoadStClientKeyHistory(tb.Context(), current.Domain, current.ClientID, 2, MaxStClientKeyHistoryBytes)
		if err != nil || len(newHistory) != 2 || newHistory[1].Registration.Present {
			tb.Fatalf("current policy tombstone is not durable: %v", err)
		}
		key, err = GetClientPublicKey(tb.Context(), current.ClientID)
		if err != nil || len(key) != 0 {
			tb.Fatalf("signed tombstone retained a current key: %v", err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var count, currentHeads int
			server.Raise(conn.QueryRow(tb.Context(), "SELECT COUNT(*) FROM st_client_key_history WHERE client_id = $1", current.ClientID).Scan(&count))
			server.Raise(conn.QueryRow(tb.Context(), "SELECT COUNT(*) FROM st_client_key_head WHERE client_id = $1 AND is_current", current.ClientID).Scan(&currentHeads))
			if count != 4 || currentHeads != 1 {
				tb.Fatalf("rollover SQL census = %d rows, %d current heads", count, currentHeads)
			}
		})
	})
}

// Identity changes beyond policy and stale boundaries cannot take ownership
// of an existing client history or reactivate an archived policy namespace.
func TestStClientKeyPolicyRolloverRejectsForeignDomainsAndRollback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		old := newStClientKeyHistoryTestInput(tb)
		old.Boundary.Epoch = 10
		if _, err := StoreStClientKeyRegistration(tb.Context(), old); err != nil {
			tb.Fatal(err)
		}
		base := old
		base.Domain.PolicyHash = [32]byte{7}
		base.Boundary.Block++
		base.Boundary.Hash = [32]byte{8}
		for _, test := range []struct {
			name   string
			change func(*StClientKeyRegistrationInput)
		}{
			{name: "chain", change: func(v *StClientKeyRegistrationInput) { v.Domain.ChainID++ }},
			{name: "genesis", change: func(v *StClientKeyRegistrationInput) { v.Domain.GenesisHash[1]++ }},
			{name: "netuid", change: func(v *StClientKeyRegistrationInput) { v.Domain.Netuid++ }},
			{name: "coordinator", change: func(v *StClientKeyRegistrationInput) { v.Domain.Coordinator[1]++ }},
			{name: "vault", change: func(v *StClientKeyRegistrationInput) { v.Domain.SettlementVault[1]++ }},
			{name: "deployment", change: func(v *StClientKeyRegistrationInput) { v.Domain.DeploymentIDHash[1]++ }},
			{name: "noID", change: func(v *StClientKeyRegistrationInput) { v.Domain.NoID++ }},
			{name: "boundaryRollback", change: func(v *StClientKeyRegistrationInput) { v.Boundary = old.Boundary }},
			{name: "olderBlock", change: func(v *StClientKeyRegistrationInput) { v.Boundary.Block = old.Boundary.Block - 1 }},
			{name: "epochRollback", change: func(v *StClientKeyRegistrationInput) { v.Boundary.Epoch = old.Boundary.Epoch - 1 }},
		} {
			candidate := base
			test.change(&candidate)
			if _, err := StoreStClientKeyRegistration(tb.Context(), candidate); err == nil {
				tb.Fatalf("%s altered the historical policy head", test.name)
			}
		}
		if _, err := StoreStClientKeyRegistration(tb.Context(), base); err != nil {
			tb.Fatal(err)
		}
		archived := old
		archived.Boundary.Block = base.Boundary.Block + 1
		archived.Boundary.Hash = [32]byte{10}
		archived.PublicKey = bytes.Repeat([]byte{12}, 32)
		if _, err := StoreStClientKeyRegistration(tb.Context(), archived); err == nil {
			tb.Fatal("archived policy regained current signing authority")
		}
	})
}

// Concurrent registration retries share one new generation, and deletion
// retires both namespaces while their immutable signed rows remain stored.
func TestStClientKeyPolicyRolloverConcurrentRetryAndDeletion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		old := newStClientKeyHistoryTestInput(tb)
		if _, err := StoreStClientKeyRegistration(tb.Context(), old); err != nil {
			tb.Fatal(err)
		}
		current := old
		current.Domain.PolicyHash = [32]byte{7}
		current.Boundary.Block++
		current.Boundary.Hash = [32]byte{8}
		start := make(chan struct{})
		results := make(chan *StClientKeyHistoryRecord, 8)
		failures := make(chan error, 8)
		var wait sync.WaitGroup
		for range 8 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := StoreStClientKeyRegistration(tb.Context(), current)
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
		var exact []byte
		for result := range results {
			if result == nil || result.Registration.Generation != 1 {
				tb.Fatal("concurrent rollover created an unexpected generation")
			}
			if exact == nil {
				exact = result.EvidenceBytes
			} else if !bytes.Equal(exact, result.EvidenceBytes) {
				tb.Fatal("concurrent rollover changed exact signed evidence")
			}
		}
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "DELETE FROM network_client WHERE client_id = $1", current.ClientID))
		})
		for _, domain := range []StClientKeyRegistrationInput{old, current} {
			if _, err := LoadStClientKeyHistory(tb.Context(), domain.Domain, current.ClientID, 1, MaxStClientKeyHistoryBytes); err == nil {
				tb.Fatal("deleted client retained positive signed history")
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var rows, retired int
			server.Raise(conn.QueryRow(tb.Context(), "SELECT COUNT(*), COUNT(*) FILTER (WHERE retired) FROM st_client_key_head WHERE client_id = $1", current.ClientID).Scan(&rows, &retired))
			if rows != 2 || retired != 2 {
				tb.Fatalf("deletion retained %d unretired of %d domain heads", rows-retired, rows)
			}
		})
	})
}
